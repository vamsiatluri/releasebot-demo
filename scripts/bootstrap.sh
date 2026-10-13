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
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$PROFILE" ] || { echo "usage: $0 --profile <aws-profile> --owner <gh-owner> --repo <gh-repo> [...]" >&2; exit 2; }
[ -n "$OWNER" ] && [ -n "$REPO" ] || { echo "--owner and --repo are required (they go in the OIDC trust policy)" >&2; exit 2; }
[ "$NETWORK_MODE" = "existing" ] && [ -z "$SUBNETS" ] && { echo "--vpc needs --subnets" >&2; exit 2; }

export AWS_PROFILE="$PROFILE" AWS_REGION="$REGION" AWS_PAGER=""
step() { printf '\n\033[1m── %s\033[0m\n' "$*"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

ACCT=$(aws sts get-caller-identity --query Account --output text) || die "cannot reach AWS with profile $PROFILE"
BUCKET="releasebot-artifacts-$ACCT"
echo "account $ACCT · region $REGION · repo $OWNER/$REPO · network: $NETWORK_MODE"

# ── 0. preflight ────────────────────────────────────────────────────────────
if [ "$SKIP_PREFLIGHT" = "0" ]; then
  step "0/9  preflight"
  EXISTING_VPC_ID="$VPC_ID" EXISTING_SUBNETS="$SUBNETS" \
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
for ART in cutrelease mergeback; do
  D=$(openssl dgst -sha256 -binary "build/$ART.zip" | xxd -p -c 32 | cut -c1-12)
  aws s3 cp "build/$ART.zip" "s3://$BUCKET/releasebot/$ART-$D.zip" --only-show-errors
  echo "  $ART -> releasebot/$ART-$D.zip"
done

# ── 5. compute ──────────────────────────────────────────────────────────────
step "5/9  compute (four functions, each with a live alias)"
for attempt in 1 2 3 4 5 6; do
  if python3 scripts/_update-stack.py releasebot-lambda infra/20-lambda.yaml \
      "ArtifactBucket=$BUCKET" \
      "CutReleaseRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-cutrelease" \
      "MergebackRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-mergeback" \
      "CutReleaseTestRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-cutrelease-test" \
      "MergebackTestRoleArn=arn:aws:iam::$ACCT:role/releasebot-exec-mergeback-test" \
      "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" 2>&1 | tail -2; then
    break
  fi
  # ⚠️ Retry, not sleep. A role created seconds ago is not yet visible to
  # CloudFormation's early validation, which fails with a detail-free
  # PropertyValidation error. A fixed sleep is a guess that is both too long
  # most of the time and too short occasionally.
  [ "$attempt" = 6 ] && die "compute stack failed after 6 attempts"
  echo "  attempt $attempt failed (IAM likely not propagated) — retrying in 10s"
  sleep 10
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
  python3 scripts/_update-stack.py releasebot-slack-approve infra/50-slack-approve.yaml \
    "ArtifactBucket=$BUCKET" "RestApiId=$API_ID" "BetaResourceId=$BETA_ID" StageName=prod \
    "SlackApprovers=$APPROVERS" "GitHubOwner=$OWNER" "GitHubRepo=$REPO" \
    "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" || echo "  ⚠ slack-approve failed"
else
  echo "  slack-approve skipped (--approvers U123,U456)"
fi
if [ -n "$ADMIN_EMAIL" ]; then
  bash scripts/deploy-dashboard.sh "$ADMIN_EMAIL" | tail -5
else
  echo "  dashboard skipped (--admin-email you@example.com)"
fi

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
