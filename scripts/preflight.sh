#!/usr/bin/env bash
# Everything that went wrong building this, checked BEFORE anything is created.
#
#   scripts/preflight.sh
#
# Environment (all optional except the profile):
#   AWS_PROFILE          the target account                         [required]
#   AWS_REGION           default us-east-1
#   GITHUB_OWNER/REPO    the repository the pipeline will deploy from
#   EXISTING_VPC_ID      deploy into their VPC instead of creating one
#   EXISTING_SUBNETS     subnet-a,subnet-b  (required with EXISTING_VPC_ID)
#
# Exit 0 clean · 1 at least one BLOCKER · 2 warnings only.
#
# ─────────────────────────────────────────────────────────────────────────────
# WHY THIS EXISTS
#
# Every check below is a real failure from building this system, not a
# hypothetical. Each one cost between twenty minutes and an afternoon, and
# every one of them is cheaper to find here than in someone else's account
# halfway through an engagement. docs/WHAT-BROKE.md is the long version.
#
# ⚠️ The checks that matter most are the ones that PROVE rather than READ. A
# route table that looks correct and a packet that arrives are different facts;
# a permission that is granted and a permission that has been exercised are
# different facts. Where proving is possible, this proves.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

REGION="${AWS_REGION:-us-east-1}"
export AWS_PAGER=""
BLOCK=0; WARN=0

ok()    { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn()  { printf '  \033[33m!\033[0m %s\n' "$*"; WARN=$((WARN+1)); }
block() { printf '  \033[31m✗\033[0m %s\n' "$*"; BLOCK=$((BLOCK+1)); }
note()  { printf '      %s\n' "$*"; }
head()  { printf '\n\033[1m%s\033[0m\n' "$*"; }

head "account"
ACCT=$(aws sts get-caller-identity --query Account --output text 2>/dev/null) || {
  block "cannot call sts — check AWS_PROFILE"; exit 1; }
WHO=$(aws sts get-caller-identity --query Arn --output text)
ok "account $ACCT in $REGION"
note "as ${WHO##*/}"

# ── 1. the CLI's own output format ─────────────────────────────────────────
# A machine that has `output = yaml` in ~/.aws/config makes every `aws ... |
# json.load` in every script fail with a byte-1 parse error, while the CLI
# exits 0. Interactive probing works; the script written from it does not.
head "the AWS CLI itself"
FMT=$(aws configure get output 2>/dev/null || echo "")
if [ "$FMT" = "yaml" ] || [ "$FMT" = "yaml-stream" ]; then
  warn "this profile's default output is '$FMT'"
  note "every script here passes --output json explicitly, but anything you add must too"
else
  ok "default output is ${FMT:-json}"
fi

# ── 2. the OIDC subject shape ──────────────────────────────────────────────
# Newer GitHub accounts emit an immutable-ID subject. A trust policy written
# for the legacy shape denies EVERY deploy, and the error never says why.
head "GitHub OIDC"
OWNER="${GITHUB_OWNER:-}"; REPO="${GITHUB_REPO:-}"
if [ -n "$OWNER" ] && [ -n "$REPO" ]; then
  IDS=$(curl -s "https://api.github.com/repos/$OWNER/$REPO" \
        | python3 -c 'import json,sys
d=json.load(sys.stdin)
print(d.get("id",""), d.get("owner",{}).get("id",""), str(d.get("private","")).lower())' 2>/dev/null)
  RID=$(echo "$IDS" | cut -d' ' -f1); OID=$(echo "$IDS" | cut -d' ' -f2); PRIV=$(echo "$IDS" | cut -d' ' -f3)
  if [ -n "$RID" ] && [ "$RID" != "None" ]; then
    ok "repo $OWNER/$REPO  repo id $RID  owner id $OID"
    note "trust BOTH subject shapes:"
    note "  repo:$OWNER@$OID/$REPO@$RID:ref:refs/heads/main"
    note "  repo:$OWNER/$REPO:ref:refs/heads/main"
    note "AND one per ENVIRONMENT -- a job naming an environment gets"
    note "  repo:...:environment:<name>, NOT the ref form. Miss it and exactly"
    note "  that one job's assume-role is denied, which reads as a flaky pipeline."
    # ── 3. can this repo even have a required reviewer? ────────────────────
    if [ "$PRIV" = "true" ]; then
      block "the repository is PRIVATE"
      note "required reviewers on an environment need Team or Enterprise; on Pro"
      note "+ private, creating the rule is refused with a billing error (HTTP 422)."
      note "Either the repo is public, the org is Team+, or the gate becomes a"
      note "deliberate workflow_dispatch input instead."
    else
      ok "repository is public — environment required-reviewers are available"
    fi
  else
    warn "could not read $OWNER/$REPO from the GitHub API (private, or no network)"
  fi
  PROV=$(aws iam list-open-id-connect-providers --output json \
         | python3 -c 'import json,sys;print(len([p for p in json.load(sys.stdin)["OpenIDConnectProviderList"] if "token.actions.githubusercontent.com" in p["Arn"]]))')
  [ "$PROV" = "1" ] && ok "the GitHub OIDC provider exists in this account" \
                    || warn "no GitHub OIDC provider yet — 10-iam.yaml creates it (CreateOIDCProvider=true)"
else
  warn "GITHUB_OWNER/GITHUB_REPO not set — skipped the OIDC subject check"
fi

# ── 4. CloudFormation parameter handling ───────────────────────────────────
# `cloudformation deploy` substitutes template DEFAULTS for every parameter you
# omit. It deleted this account's OIDC provider once.
head "deployment habits"
if grep -rqE '^\s*aws cloudformation deploy' scripts/*.sh 2>/dev/null; then
  warn "some scripts still use 'cloudformation deploy'"
  note "it silently resets omitted parameters to template defaults."
  note "scripts/_update-stack.py re-sends everything as UsePreviousValue."
else
  ok "no bare 'cloudformation deploy' in scripts/"
fi
python3 scripts/lint-shell.py >/dev/null 2>&1 \
  && ok "shell lint clean (no \$VAR followed by non-ASCII, no curl|grep -q under pipefail)" \
  || block "scripts/lint-shell.py found problems — run it"

# ── 5. the network, PROVEN not read ────────────────────────────────────────
head "network"
if [ -n "${EXISTING_VPC_ID:-}" ]; then
  [ -n "${EXISTING_SUBNETS:-}" ] || { block "EXISTING_VPC_ID set without EXISTING_SUBNETS"; }
  aws ec2 describe-vpcs --vpc-ids "$EXISTING_VPC_ID" --output json >/dev/null 2>&1 \
    && ok "VPC $EXISTING_VPC_ID exists" || block "VPC $EXISTING_VPC_ID not found in $ACCT/$REGION"
  DNS=$(aws ec2 describe-vpc-attribute --vpc-id "$EXISTING_VPC_ID" \
          --attribute enableDnsHostnames --query 'EnableDnsHostnames.Value' --output text 2>/dev/null)
  if [ "$DNS" = "True" ]; then ok "DNS hostnames enabled (interface endpoints need it)"
  else
    block "enableDnsHostnames is $DNS on $EXISTING_VPC_ID"
    note "without it an interface endpoint is created, billed, and NEVER USED --"
    note "the service name keeps resolving to the public address and the traffic"
    note "quietly leaves through the NAT. Nothing errors; it is simply not private."
  fi
  AZS=$(aws ec2 describe-subnets --subnet-ids ${EXISTING_SUBNETS//,/ } \
          --query 'Subnets[].AvailabilityZone' --output text 2>/dev/null | tr '\t' '\n' | sort -u | wc -l | tr -d ' ')
  [ "${AZS:-0}" -ge 2 ] && ok "subnets span $AZS availability zones" \
                        || warn "subnets are in ${AZS:-0} AZ — a single-AZ event takes the function down"
  note "⚠️ EGRESS IS NOT CHECKED HERE. Run scripts/probe-egress.sh after the"
  note "   first deploy: it puts a throwaway function in these subnets and"
  note "   actually calls api.github.com. A route table that looks right and a"
  note "   packet that arrives are different facts."
else
  ok "no existing VPC given — 15-network.yaml will create one"
  note "a NAT Gateway is ~\$32/month plus data; CreateNatGateway=false if the"
  note "functions never need to leave AWS (dry run, or endpoints for everything)"
fi

# ── 6. service quotas that bite late ───────────────────────────────────────
head "limits"
CONC=$(aws lambda get-account-settings --query 'AccountLimit.ConcurrentExecutions' --output text 2>/dev/null)
if [ -n "$CONC" ] && [ "$CONC" != "None" ]; then
  if [ "$CONC" -ge 1000 ]; then
    ok "Lambda concurrency limit $CONC"
  elif [ "$CONC" -lt 110 ]; then
    # ⚠️ A BLOCKER, not a warning. A brand-new account gets TEN, and AWS refuses
    # any reservation leaving fewer than 100 unreserved -- so the compute stack
    # cannot deploy at all, and the error talks about unreserved concurrency
    # rather than about the account being new.
    # ⚠️ Only a BLOCKER when the caller has not already decided to deploy
    # without reservations. bootstrap.sh reads this same quota and exports
    # RESERVED_DECIDED=0, so blocking it there would stop a run that cannot
    # possibly hit the limit -- a check that fails on a condition the caller
    # has already handled trains people to pass --skip-preflight.
    if [ "${RESERVED_DECIDED:-unset}" = "0" ]; then
      warn "Lambda concurrency limit is only $CONC — reservations disabled, as required"
      note "raise the quota later and redeploy with ReservedConcurrency=5"
    else
      block "Lambda concurrency limit is only $CONC"
      note "AWS refuses a reservation that leaves <100 unreserved, so"
      note "ReservedConcurrentExecutions cannot be set at all here."
      note "Deploy with ReservedConcurrency=0, or raise the quota first"
      note "(Service Quotas -> Lambda -> Concurrent executions)."
    fi
  else
    warn "Lambda concurrency limit is $CONC — size any reservation against it"
  fi

  # ⚠️ A brand-new account has no aws/dynamodb managed key until something
  # creates one, and a template asking for it fails with a KMS NotFoundException
  # naming a key id that is nowhere in the template.
  if aws kms describe-key --key-id alias/aws/dynamodb >/dev/null 2>&1; then
    ok "the aws/dynamodb managed key exists"
  else
    warn "no aws/dynamodb managed key yet (normal in a new account)"
    note "the ledger table deliberately does NOT request it — it is encrypted"
    note "at rest with an AWS-owned key either way"
  fi
fi
EIP=$(aws ec2 describe-account-attributes --attribute-names default-vpc --output text >/dev/null 2>&1 && echo ok || echo "")
ok "credentials can read EC2 (needed for VPC attachment)"

# ── 7. the two GitHub credentials ──────────────────────────────────────────
head "credentials"
note "TWO tokens, and they are not interchangeable:"
note "  classic PAT (repo)        -> slackApprove only. A fine-grained token"
note "     CANNOT approve a deployment at all; its 403 reads as a missing"
note "     permission even with Actions: write, and no permission fixes it."
note "  fine-grained (contents+PRs) -> the four release functions."
note "set-approval-token.sh refuses fine-grained; set-write-token.sh refuses classic."
note "⚠️ A fine-grained token reads a PUBLIC repo without the Contents permission,"
note "   so 'it can read the repo' proves nothing about whether it can write."
note "   set-write-token.sh proves write BY WRITING a throwaway ref."

# ── 7b. the templates themselves, and what a rollback leaves behind ────────
head "templates and leftovers"

# ⚠️ Every concurrency reservation in the templates must route through the
# CapConcurrency switch. Two of them were hardcoded literals, which meant
# `ReservedConcurrency=0` silently did not apply to those functions and the
# compute stack could not create at all in an account with a limit of 10.
HARD=$(grep -rn 'ReservedConcurrentExecutions:[[:space:]]*[0-9]' infra/*.yaml || true)
if [ -n "$HARD" ]; then
  block "a hardcoded concurrency reservation ignores the disable switch"
  printf '%s\n' "$HARD" | while IFS= read -r L; do note "$L"; done
  note "route it through !If [CapConcurrency, N, !Ref 'AWS::NoValue']"
else
  ok "every concurrency reservation routes through CapConcurrency"
fi

# ⚠️ DeletionPolicy: Retain is correct for an audit table and poisonous for a
# retry: a rolled-back CREATE leaves the table behind, and the next create
# fails with AWS::EarlyValidation::ResourceExistenceCheck and NO resource-level
# event -- nothing in the output names the table.
if aws dynamodb describe-table --table-name releasebot-ledger >/dev/null 2>&1; then
  LS=$(aws cloudformation describe-stacks --stack-name releasebot-lambda \
    --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo ABSENT)
  case "$LS" in
    CREATE_COMPLETE|UPDATE_COMPLETE|UPDATE_ROLLBACK_COMPLETE)
      ok "releasebot-ledger exists and releasebot-lambda ($LS) owns it" ;;
    *)
      ROWS=$(aws dynamodb scan --table-name releasebot-ledger --select COUNT \
        --max-items 1 --query Count --output text 2>/dev/null || echo unknown)
      if [ "$ROWS" = "0" ]; then
        warn "releasebot-ledger is orphaned and empty (stack: $LS)"
        note "wreckage of a rolled-back create; bootstrap.sh removes it for you"
      else
        block "releasebot-ledger holds $ROWS row(s) and no healthy stack owns it"
        note "it will block the compute stack's create with an early-validation"
        note "error that never names the table. Import it, or move it aside."
      fi ;;
  esac
else
  ok "no leftover releasebot-ledger table"
fi

# A dead stack cannot be updated, only deleted. _update-stack.py does that
# itself, but a standalone preflight should say so rather than let it surprise.
for S in releasebot-iam releasebot-lambda releasebot-edge releasebot-network; do
  ST=$(aws cloudformation describe-stacks --stack-name "$S" \
    --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo ABSENT)
  case "$ST" in
    ROLLBACK_COMPLETE|REVIEW_IN_PROGRESS)
      warn "$S is $ST — a dead stack; it will be deleted and recreated" ;;
  esac
done

# ── 8. things only a real deploy teaches ───────────────────────────────────
head "reminders that cost a day each"
note "• CloudFormation compares Lambda Code as STRINGS — overwrite the same S3"
note "  key and UpdateFunctionCode is never called. The stack goes green and the"
note "  OLD binary keeps running. Use a content-addressed key."
note "• A published VERSION freezes code AND configuration. Changing a parameter"
note "  updates \$LATEST only; the live alias keeps serving the old value until"
note "  you publish and move it."
note "• Rotating a secret strands every older version as a rollback target —"
note "  it fails at INIT with a 500 and an EMPTY log. Keep config in Parameter"
note "  Store so the version freezes a POINTER, not the values."
note "• A REST API resource policy does not apply until the stage is REDEPLOYED,"
note "  and then takes 20-30 SECONDS to propagate. A probe 6 seconds later"
note "  reports the old behaviour and looks like a broken condition."
note "• 'live' is a DEPLOYMENT POINTER, not an environment. Never let an alias"
note "  named live decide configuration."

printf '\n'
if [ "$BLOCK" -gt 0 ]; then
  printf '\033[31m%d blocker(s), %d warning(s)\033[0m\n' "$BLOCK" "$WARN"; exit 1
elif [ "$WARN" -gt 0 ]; then
  printf '\033[33m0 blockers, %d warning(s)\033[0m\n' "$WARN"; exit 2
fi
printf '\033[32mclean\033[0m\n'
