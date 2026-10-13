#!/usr/bin/env bash
# Build ReleaseBot end to end in an account that has nothing.
#
#   scripts/bootstrap.sh --profile msnbc-prod --owner msnbc --repo releasebot \
#       --admin-email you@nbcuni.com --approvers U123,U456
#
#   # into a VPC the networking team already owns
#   scripts/bootstrap.sh ... --vpc vpc-0abc --subnets subnet-a,subnet-b
#
#   # no VPC at all (what it ran on before the VPC existed)
#   scripts/bootstrap.sh ... --no-vpc
#
# ─────────────────────────────────────────────────────────────────────────────
# ORDER MATTERS, AND SO DOES WHAT IS *NOT* HERE.
#
# This creates infrastructure. It does NOT put credentials anywhere -- the two
# token scripts do that, deliberately, because a bootstrap that takes secrets
# on its command line is a bootstrap that writes them into shell history on
# somebody else's laptop.
#
# ⚠️ It uses update-stack via _update-stack.py, never `cloudformation deploy`.
# `deploy` substitutes template DEFAULTS for every parameter you omit; on this
# project that deleted an account's GitHub OIDC provider and no pipeline run
# could have assumed its deploy role until it was restored.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PROFILE=""; REGION="${AWS_REGION:-us-east-1}"
OWNER=""; REPO=""; ADMIN_EMAIL=""; APPROVERS=""
VPC_ID=""; SUBNETS=""; NETWORK_MODE="create"; NAT="true"; ENDPOINTS="true"
SKIP_PREFLIGHT=0

while [ $# -gt 0 ]; do
  case "$1" in
    --profile)      PROFILE="$2"; shift 2 ;;
    --region)       REGION="$2"; shift 2 ;;
    --owner)        OWNER="$2"; shift 2 ;;
    --repo)         REPO="$2"; shift 2 ;;
    --admin-email)  ADMIN_EMAIL="$2"; shift 2 ;;
    --approvers)    APPROVERS="$2"; shift 2 ;;
    --vpc)          VPC_ID="$2"; NETWORK_MODE="existing"; shift 2 ;;
    --subnets)      SUBNETS="$2"; shift 2 ;;
    --no-vpc)       NETWORK_MODE="none"; shift ;;
    --no-nat)       NAT="false"; shift ;;
    --no-endpoints) ENDPOINTS="false"; shift ;;
    --skip-preflight) SKIP_PREFLIGHT=1; shift ;;
    --label)        ACCOUNT_LABEL="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$PROFILE" ] || { echo "usage: $0 --profile <aws-profile> --owner <gh-owner> --repo <gh-repo> [--label <name>] [...]" >&2; exit 2; }
[ -n "$OWNER" ] && [ -n "$REPO" ] || { echo "--owner and --repo are required (they go in the OIDC trust policy)" >&2; exit 2; }
[ "$NETWORK_MODE" = "existing" ] && [ -z "$SUBNETS" ] && { echo "--vpc needs --subnets" >&2; exit 2; }

# ⛔ The account's human name, printed on every Slack message and reported on
# /health. Defaults to the PROFILE rather than to "unlabelled": a wrong-looking
# label gets fixed, an absent one gets ignored, and the whole point is that two
# accounts posting to one Slack channel must be distinguishable at the click.
ACCOUNT_LABEL="${ACCOUNT_LABEL:-$PROFILE}"

export AWS_PROFILE="$PROFILE" AWS_REGION="$REGION" AWS_PAGER=""
step() { printf '\n\033[1m── %s\033[0m\n' "$*"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

ACCT=$(aws sts get-caller-identity --query Account --output text) || die "cannot reach AWS with profile $PROFILE"

# ⚠️ A brand-new account has a Lambda concurrency limit of 10, and AWS refuses
# any reservation leaving fewer than 100 unreserved -- so the reservation that
# protects a mature account makes the stack undeployable in a fresh one. Decide
# it from the actual quota rather than making the operator discover it.
LIMIT=$(aws lambda get-account-settings --query 'AccountLimit.ConcurrentExecutions' --output text 2>/dev/null || echo 0)
if [ "${LIMIT:-0}" -lt 110 ]; then
  RESERVED=0
  echo "  concurrency limit $LIMIT — per-function reservation DISABLED (raise the quota, then redeploy with ReservedConcurrency=5)"
else
  RESERVED="${RESERVED_CONCURRENCY:-5}"
fi
BUCKET="releasebot-artifacts-$ACCT"
echo "account $ACCT ($ACCOUNT_LABEL) · region $REGION · repo $OWNER/$REPO · network: $NETWORK_MODE"

# ── 0. preflight ────────────────────────────────────────────────────────────
if [ "$SKIP_PREFLIGHT" = "0" ]; then
  step "0/9  preflight"
  EXISTING_VPC_ID="$VPC_ID" EXISTING_SUBNETS="$SUBNETS" \
    RESERVED_DECIDED="$RESERVED" \
    GITHUB_OWNER="$OWNER" GITHUB_REPO="$REPO" bash scripts/preflight.sh
  RC=$?
  [ "$RC" = "1" ] && die "preflight found blockers — fix them or pass --skip-preflight"
fi

# ── 1. the artifact bucket ──────────────────────────────────────────────────
step "1/9  artifact bucket"
if aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
  echo "  $BUCKET already exists"
else
  if [ "$REGION" = "us-east-1" ]; then
    aws s3api create-bucket --bucket "$BUCKET" >/dev/null
  else
    aws s3api create-bucket --bucket "$BUCKET" --region "$REGION" \
      --create-bucket-configuration "LocationConstraint=$REGION" >/dev/null
  fi
  aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  echo "  created $BUCKET (private, encrypted)"
fi

# ── 2. identity, FIRST and separately ───────────────────────────────────────
# Deployed before the application stacks on purpose: rolling the application
# back must never roll back the trust relationship the pipeline needs in order
# to roll it back.
step "2/9  identity (OIDC trust + four execution roles)"
IDS=$(curl -s "https://api.github.com/repos/$OWNER/$REPO" \
      | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("id",""),d.get("owner",{}).get("id",""))')
RID=$(echo "$IDS" | cut -d' ' -f1); OID=$(echo "$IDS" | cut -d' ' -f2)
[ -n "$RID" ] && [ "$RID" != "None" ] || die "could not read $OWNER/$REPO ids from GitHub (needed for the OIDC subject)"
python3 scripts/_update-stack.py releasebot-iam infra/10-iam.yaml \
  "GitHubOrg=$OWNER" "GitHubRepo=$REPO" "GitHubOwnerId=$OID" "GitHubRepoId=$RID" \
  CreateOIDCProvider=true SecretsPrefix=releasebot SetApiGatewayAccountRole=true || die "identity stack failed"

# ── 3. network ──────────────────────────────────────────────────────────────
VPC_SUBNETS=""; VPC_SG=""
case "$NETWORK_MODE" in
  none) step "3/9  network — skipped (--no-vpc)" ;;
  create)
    step "3/9  network — creating a dedicated VPC"
    python3 scripts/_update-stack.py releasebot-network infra/15-network.yaml \
      "CreateNatGateway=$NAT" "CreateEndpoints=$ENDPOINTS" || die "network stack failed" ;;
  existing)
    step "3/9  network — into $VPC_ID (creating only security groups and endpoints)"
    python3 scripts/_update-stack.py releasebot-network infra/15-network.yaml \
      "ExistingVpcId=$VPC_ID" "ExistingPrivateSubnetIds=$SUBNETS" \
      "CreateEndpoints=$ENDPOINTS" || die "network stack failed" ;;
esac
if [ "$NETWORK_MODE" != "none" ]; then
  VPC_SUBNETS=$(aws cloudformation describe-stacks --stack-name releasebot-network \
    --query "Stacks[0].Outputs[?OutputKey=='PrivateSubnetIds'].OutputValue" --output text)
  VPC_SG=$(aws cloudformation describe-stacks --stack-name releasebot-network \
    --query "Stacks[0].Outputs[?OutputKey=='LambdaSecurityGroup'].OutputValue" --output text)
  echo "  subnets $VPC_SUBNETS   sg $VPC_SG"
fi

# ── 4. the artifact, before the functions that reference it ─────────────────
step "4/9  build and stage the artifact"
make package >/dev/null || die "make package failed"
# ⚠️ CONTENT-ADDRESSED KEY. CloudFormation compares Code as STRINGS; overwrite
# the same key and UpdateFunctionCode is never called -- the stack reports
# success and the OLD binary keeps running.
# ⚠️ AND THE KEYS MUST BE PASSED TO THE TEMPLATE. Uploading a
# content-addressed object and leaving the stack on its default key means the
# functions point at releasebot/cutrelease.zip, which does not exist -- the
# create fails with "S3 Error Code: NoSuchKey" naming a key you never wrote.
# Found on the first real run of this script; the digest is the whole point and
# it is useless if nothing is told about it.
# ⚠️ slackapprove belongs in THIS list. It was missing, so step 9 created a
# function pointing at the template's default key `releasebot/slackapprove.zip`
# -- which nothing had ever uploaded. The stack failed with NoSuchKey naming a
# key that looks plausible, four steps away from the loop that was supposed to
# write it. `make package` had been building the zip the whole time.
# ⛔ slackapprove IS BUILT HERE, NOT READ FROM build/.
#
# Two fixes made earlier today contradicted each other, and this is where they
# collided:
#   - bootstrap was reading build/slackapprove.zip, added because the approval
#     stack was being created with a key nothing had uploaded (NoSuchKey);
#   - `make package` now DELETES build/slackapprove.zip, added because a stale
#     copy of it sat there for weeks and got hand-uploaded, deploying code that
#     predated the secret resolver.
# Each fix was right on its own. Together the second removed the file the first
# depended on, and bootstrap died at step 4.
#
# ★ The resolution is not to pick one: it is to stop relying on a file that may
# or may not be current. Build it, here, from source, every time. The stack
# needs a real S3 key before it can create the function, so this cannot be
# deferred to deploy-slack-approve.sh.
SA_WORK=$(mktemp -d); trap 'rm -rf "$SA_WORK"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o "$SA_WORK/bootstrap" ./cmd/slackapprove \
  || die "could not build cmd/slackapprove"
( cd "$SA_WORK" && zip -q -X slackapprove.zip bootstrap )
cp "$SA_WORK/slackapprove.zip" "build/slackapprove.zip"

CUT_KEY=""; MERGE_KEY=""; APPROVE_KEY=""
for ART in cutrelease mergeback slackapprove; do
  [ -f "build/$ART.zip" ] || die "make package did not produce build/$ART.zip"
  D=$(openssl dgst -sha256 -binary "build/$ART.zip" | xxd -p -c 32 | cut -c1-12)
  K="releasebot/$ART-$D.zip"
  aws s3 cp "build/$ART.zip" "s3://$BUCKET/$K" --only-show-errors || die "upload failed"
  case "$ART" in
    cutrelease)   CUT_KEY="$K" ;;
    mergeback)    MERGE_KEY="$K" ;;
    slackapprove) APPROVE_KEY="$K" ;;
  esac
  echo "  $ART -> $K"
done

# ── 4b. the retained-ledger trap ─────────────────────────────────────────────
# ⚠️ LedgerTable is DeletionPolicy: Retain, which is right -- it is the audit
# record of what shipped. The consequence is that a compute stack whose CREATE
# rolls back leaves `releasebot-ledger` behind with no stack owning it, and
# every subsequent create then fails on the table already existing. Worse, the
# failure arrives as AWS::EarlyValidation::ResourceExistenceCheck with no
# resource-level event, so nothing in the output names the table.
#
# An EMPTY orphan can only be the wreckage of a rolled-back create, so remove
# it. A table with rows in it is real release history: stop, and let a human
# decide, because deleting it to get a green deploy is the wrong trade.
if aws dynamodb describe-table --table-name releasebot-ledger >/dev/null 2>&1; then
  LSTATUS=$(aws cloudformation describe-stacks --stack-name releasebot-lambda \
    --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo ABSENT)
  case "$LSTATUS" in
    CREATE_COMPLETE|UPDATE_COMPLETE|UPDATE_ROLLBACK_COMPLETE)
      : ;;  # a live stack owns it — leave it alone
    *)
      ROWS=$(aws dynamodb scan --table-name releasebot-ledger --select COUNT \
        --max-items 1 --query Count --output text 2>/dev/null || echo unknown)
      if [ "$ROWS" = "0" ]; then
        echo "  releasebot-ledger exists with no stack owning it and no rows —"
        echo "  wreckage of a rolled-back create (DeletionPolicy: Retain). Removing it."
        aws dynamodb delete-table --table-name releasebot-ledger >/dev/null \
          || die "could not delete the orphaned ledger table"
        aws dynamodb wait table-not-exists --table-name releasebot-ledger
      else
        die "releasebot-ledger exists with $ROWS row(s) and no healthy stack owns it.
    That is real release history, so this script will not delete it. Either
    import it into releasebot-lambda, or move it aside deliberately:
      aws dynamodb delete-table --table-name releasebot-ledger --profile $PROFILE"
      fi ;;
  esac
fi

# ── 5. compute ──────────────────────────────────────────────────────────────
step "5/9  compute (four functions, each with a live alias)"
for attempt in 1 2 3 4 5 6; do
  if python3 scripts/_update-stack.py releasebot-lambda infra/20-lambda.yaml \
      "ArtifactBucket=$BUCKET" "CutReleaseKey=$CUT_KEY" "MergebackKey=$MERGE_KEY" \
      "CutReleaseRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-cutrelease" \
      "MergebackRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-mergeback" \
      "CutReleaseTestRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-cutrelease-test" \
      "MergebackTestRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-mergeback-test" \
      "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" \
      "AccountLabel=$ACCOUNT_LABEL" \
      "ReservedConcurrency=$RESERVED" 2>&1 | tail -3; then
    break
  fi
  # ⚠️ Retry, not sleep -- a role created seconds ago is not yet visible to
  # CloudFormation's early validation. But ONLY retry that: the first version
  # of this loop printed "IAM likely not propagated" for every failure,
  # including a missing S3 key, and hammered six times at something that was
  # never going to succeed while hiding the real reason. A retry that cannot
  # distinguish a transient failure from a permanent one is a way of not
  # reading the error.
  REASON=$(aws cloudformation describe-stack-events --stack-name releasebot-lambda \
    --query "StackEvents[?contains(ResourceStatus,'FAILED') && ResourceStatusReason!='Resource creation cancelled'].ResourceStatusReason | [0]" \
    --output text 2>/dev/null)
  case "$REASON" in
    *"not authorized"*|*"Invalid principal"*|*PropertyValidation*|*"cannot be assumed"*)
      echo "  attempt $attempt: IAM not propagated yet — retrying in 10s" ;;
    None|"") echo "  attempt $attempt failed with no recorded reason — retrying in 10s" ;;
    *) die "compute stack failed and retrying will not help:
    $REASON" ;;
  esac
  [ "$attempt" = 6 ] && die "compute stack failed after 6 attempts"
  sleep 10
done

# ── 5b. prove the ALIAS moved, not just that the stack went green ───────────
# ⛔ THE CHECK THAT CAUGHT THE WORST DEFECT IN THIS SCRIPT. A deploy with a new
# binary reported UPDATE_COMPLETE, updated $LATEST, and left `live` pointing at
# the previous version -- so the stack was green and the old code was still
# serving. Nothing about the stack's status can tell you this. The only thing
# that can is reading the alias back and comparing what it actually serves
# against the artifact you just uploaded.
#
# Verify at the destination, not at the transport.
SHA_CUT=$(openssl dgst -sha256 -binary build/cutrelease.zip | openssl base64)
SHA_MRG=$(openssl dgst -sha256 -binary build/mergeback.zip | openssl base64)
for PAIR in "cutRelease=$SHA_CUT" "cutReleaseTest=$SHA_CUT" \
            "releaseAutomationMergeback=$SHA_MRG" "releaseAutomationMergebackTest=$SHA_MRG"; do
  FN="${PAIR%%=*}"; WANT="${PAIR#*=}"
  GOT=$(aws lambda get-function --function-name "$FN" --qualifier live \
    --query 'Configuration.CodeSha256' --output text 2>/dev/null)
  VER=$(aws lambda get-alias --function-name "$FN" --name live \
    --query FunctionVersion --output text 2>/dev/null)
  if [ "$WANT" = "$GOT" ]; then
    echo "  ✓ $FN live -> v$VER serves the artifact just uploaded"
  else
    die "$FN: the live alias serves v$VER, which is NOT the binary just uploaded.
    expected CodeSha256 $WANT
    alias serves      $GOT
    The stack reported success and the OLD code is still live. Check that
    AWS::Lambda::Version's Description carries the content-addressed artifact
    key -- a Version resource that does not change is never republished, so
    the code lands on \$LATEST and the alias never moves."
  fi
done

# ── 6. edge ─────────────────────────────────────────────────────────────────
step "6/9  edge (one API, two stages, WAF and throttle on prod)"
L="arn:aws:lambda:$REGION:$ACCT:function"
python3 scripts/_update-stack.py releasebot-edge infra/30-apigw.yaml \
  "CutReleaseAliasArn=${L}:cutRelease:live" \
  "CutReleaseTestAliasArn=${L}:cutReleaseTest:live" \
  "MergebackAliasArn=${L}:releaseAutomationMergeback:live" \
  "MergebackTestAliasArn=${L}:releaseAutomationMergebackTest:live" || die "edge stack failed"
BASE=$(aws cloudformation describe-stacks --stack-name releasebot-edge \
  --query "Stacks[0].Outputs[?OutputKey=='ProdInvokeUrl'].OutputValue" --output text)
BASE="${BASE%/prod}"
echo "  api: $BASE"

# ── 7. per-environment configuration ────────────────────────────────────────
# ⚠️ Parameter Store, NOT environment variables. A published version freezes
# code AND configuration, so values baked into env vars mean rotating a secret
# silently invalidates every older version as a rollback target -- on exactly
# the day you need one.
step "7/9  per-environment configuration"
PROPS="${PROPERTIES_JSON:-{\"news-app\":\"NBC News\",\"today-web\":\"TODAY\",\"telemundo-web\":\"Telemundo\",\"msnbc-web\":\"MSNBC\"}}"
for E in prod test; do
  put() { aws ssm put-parameter --name "/releasebot/$E/$1" --type String --value "$2" --overwrite >/dev/null; }
  put default-owner   "$OWNER"
  put default-branch  main
  put github-api-url  https://api.github.com
  put datadog-site    datadoghq.com
  put datadog-service "releasebot$([ "$E" = test ] && echo -test)"
  put slack-channel   "C0RELEASE$([ "$E" = test ] && echo -TEST)"
  put ack-only        true
  put properties      "$PROPS"
  echo "  /releasebot/$E/ — 8 parameters"
done

# ── 8. observability ────────────────────────────────────────────────────────
step "8/9  observability"
python3 scripts/_update-stack.py releasebot-observability infra/40-observability.yaml \
  "ApiBaseUrl=$BASE/prod" "CanaryBucket=$BUCKET" EnableCanary=false || die "observability stack failed"

# ── 9. the two optional stacks ──────────────────────────────────────────────
step "9/9  approval endpoint and dashboard"
if [ -n "$APPROVERS" ]; then
  API_ID=$(aws apigateway get-rest-apis --query "items[?name=='releasebot'].id | [0]" --output text)
  BETA_ID=$(aws apigateway get-resources --rest-api-id "$API_ID" \
    --query "items[?pathPart=='beta'].id | [0]" --output text)
  # ⚠️ Three parameters here are credentials or someone else's identifiers, and
  # this stack has no defaults for them -- so the first run used to abort with
  # "[SlackSigningSecret, GitHubApprovalToken, GitHubEnvironmentId] must have
  # values", which reads like a broken script rather than like work a human
  # still owes. The stack has to exist BEFORE set-approval-token.sh can fill in
  # the token (it updates one parameter and carries the rest forward), so the
  # right answer is to create it with values that FAIL CLOSED and say so.
  #
  # The environment id is not a secret and the GitHub API hands it over for a
  # public repo, so fetch it rather than asking.
  ENV_ID=$(curl -fsS "https://api.github.com/repos/$OWNER/$REPO/environments" 2>/dev/null \
    | python3 -c 'import sys,json
d=json.load(sys.stdin)
for e in d.get("environments",[]):
    if e["name"] in ("production","prod"):
        print(e["id"]); break
else:
    print("")' 2>/dev/null)
  if [ -z "$ENV_ID" ]; then
    ENV_ID=0
    echo "  ⚠ no 'production' environment on $OWNER/$REPO — GitHubEnvironmentId=0"
    echo "    create the environment, then rerun with the real id; an approval"
    echo "    posted against environment 0 is rejected by GitHub, not accepted."
  fi
  # A signing secret that cannot verify anything makes every Slack request fail
  # its HMAC check, and an unset token makes every approval fail at GitHub.
  # Both are the safe direction: the endpoint is live and refuses everything.
  SIGNING="${SLACK_SIGNING_SECRET:-UNSET-see-docs/CONNECTING-SLACK-AND-JIRA.md}"
  APPROVAL="${RELEASEBOT_APPROVAL_TOKEN:-UNSET-run-scripts/set-approval-token.sh}"
  # Exported so _update-stack.py can read them from the environment. They must
  # NOT become command-line arguments: argv is visible in `ps` to every process
  # on the machine and lands in shell history.
  export SIGNING APPROVAL
  python3 scripts/_update-stack.py releasebot-slack-approve infra/50-slack-approve.yaml \
    "ArtifactBucket=$BUCKET" "ArtifactKey=$APPROVE_KEY" \
    "RestApiId=$API_ID" "BetaResourceId=$BETA_ID" StageName=prod \
    "SlackApprovers=$APPROVERS" "GitHubOwner=$OWNER" "GitHubRepo=$REPO" \
    "GitHubEnvironmentId=$ENV_ID" "AccountLabel=$ACCOUNT_LABEL" \
    "SlackSigningSecret=@SIGNING" "GitHubApprovalToken=@APPROVAL" \
    "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" \
    "ReservedConcurrency=$(if [ "$RESERVED" = 0 ]; then echo 0; else echo 2; fi)" \
    || echo "  ⚠ slack-approve failed"
  case "$SIGNING$APPROVAL" in
    *UNSET*) echo "  approval endpoint deployed and FAILING CLOSED — it rejects every"
             echo "  request until the signing secret and approval token are set" ;;
    *) echo "  approval endpoint deployed with a signing secret and token from the environment" ;;
  esac
else
  echo "  slack-approve skipped (--approvers U123,U456)"
fi
if [ -n "$ADMIN_EMAIL" ]; then
  bash scripts/deploy-dashboard.sh "$ADMIN_EMAIL" | tail -5
else
  echo "  dashboard skipped (--admin-email you@example.com)"
fi

# ── 10. ask the thing you just built whether it is alive ────────────────────
# A build script that never calls its own endpoint is reporting on its
# intentions. Both stages are probed; prod is EXPECTED to answer 503-unready
# until the credentials are set, and saying so here is the difference between
# a documented state and an operator debugging a 502 at 9am.
step "verify  both stages answer"
for S in prod test; do
  CODE=$(curl -s -o /tmp/rb-health.$$ -w '%{http_code}' --max-time 15 \
    "$BASE/$S/beta/health" 2>/dev/null || echo 000)
  case "$CODE" in
    200) echo "  ✓ /$S 200 — ready: $(sed -n 's/.*"commit":"\([^"]*\)".*/commit \1/p' /tmp/rb-health.$$)" ;;
    503) echo "  • /$S 503 — UNREADY, and reporting why (expected before credentials):"
         echo "      $(sed -n 's/.*"reason":"\([^"]*\)".*/\1/p' /tmp/rb-health.$$)" ;;
    000) echo "  ⚠ /$S did not respond within 15s" ;;
    *)   echo "  ⚠ /$S returned $CODE — unexpected; check the function's log group"
         head -c 200 /tmp/rb-health.$$; echo ;;
  esac
  rm -f /tmp/rb-health.$$
done

cat <<EOS

$(printf '\033[1m── built ──\033[0m')

  api            $BASE
  health         $BASE/prod/beta/health
  stages         /prod and /test

  ⚠️ NOTHING CAN TALK TO GITHUB OR SLACK YET. Three things remain, and all
  three need a human because they involve credentials or someone else's
  console:

  1. the two GitHub tokens — they are NOT interchangeable
       bash scripts/set-write-token.sh      fine-grained, Contents + PRs
       bash scripts/set-approval-token.sh   classic PAT, the ONLY kind that
                                            can approve a deployment
  2. Slack and Jira wiring            docs/CONNECTING-SLACK-AND-JIRA.md
  3. the repository secrets           AWS_DEPLOY_ROLE_ARN, SLACK_SIGNING_SECRET,
                                      JIRA_WEBHOOK_SECRET, SLACK_WEBHOOK_URL

  Then prove the network before trusting it:
       bash scripts/probe-egress.sh
EOS
