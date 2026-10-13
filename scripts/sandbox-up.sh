#!/usr/bin/env bash
# Build the whole of ReleaseBot in an empty AWS account, from nothing, and prove
# it works before exiting.
#
# Pairs with sandbox-down.sh. Being able to run down-then-up and get an identical
# working system is the only way to know the templates are the source of truth
# and not a record of what someone once clicked.
set -euo pipefail
cd "$(dirname "$0")/.."
export AWS_PROFILE="${AWS_PROFILE:-releasebot-sandbox}" AWS_REGION="${AWS_REGION:-us-east-1}" AWS_PAGER=""

ACCT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="releasebot-artifacts-$ACCT"
# ⛔ No secret defaults. A literal here is a literal in the published repository,
# and a signing secret in source next to a public URL lets anyone sign a valid
# request. The SOW is explicit about this: "do not expose secret values in
# source, logs, tickets, or documentation."
: "${SLACK_SIGNING_SECRET:?set SLACK_SIGNING_SECRET before running}"
: "${JIRA_WEBHOOK_SECRET:?set JIRA_WEBHOOK_SECRET before running}"
SIG="$SLACK_SIGNING_SECRET"
JIRA="$JIRA_WEBHOOK_SECRET"
step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

step "1/10  build and package"
make package 2>&1 | tail -2

step "2/10  artifact bucket"
aws s3api head-bucket --bucket "$BUCKET" 2>/dev/null || \
  aws s3api create-bucket --bucket "$BUCKET" >/dev/null
aws s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3 cp build/cutrelease.zip "s3://$BUCKET/releasebot/cutrelease.zip" --only-show-errors
aws s3 cp build/mergeback.zip  "s3://$BUCKET/releasebot/mergeback.zip"  --only-show-errors
echo "  artifacts uploaded to $BUCKET"

step "3/10  identity (roles, OIDC trust, API Gateway account logging role)"
aws cloudformation deploy --template-file infra/10-iam.yaml --stack-name releasebot-iam \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides GitHubOrg=vamsiatluri GitHubRepo=releasebot-migration \
    CreateOIDCProvider=true SecretsPrefix=releasebot SetApiGatewayAccountRole=true | tail -1

step "3.5/10  network (dedicated VPC; set NETWORK=0 to skip the NAT's ~$32/month)"
# ⛔ NOT the default VPC. See infra/15-network.yaml for why.
# ⚠️ The NAT Gateway is the expensive line. NETWORK=0 brings the environment up
# with the functions on the AWS-managed network, which is what they ran on
# before the VPC existed and is the right default for a short-lived rebuild.
VPC_SUBNETS=""; VPC_SG=""
if [ "${NETWORK:-1}" = "1" ]; then
  aws cloudformation deploy --template-file infra/15-network.yaml \
    --stack-name releasebot-network --parameter-overrides CreateNatGateway=true | tail -1
  VPC_SUBNETS=$(aws cloudformation describe-stacks --stack-name releasebot-network \
    --query "Stacks[0].Outputs[?OutputKey=='PrivateSubnetIds'].OutputValue" --output text)
  VPC_SG=$(aws cloudformation describe-stacks --stack-name releasebot-network \
    --query "Stacks[0].Outputs[?OutputKey=='LambdaSecurityGroup'].OutputValue" --output text)
  echo "  vpc subnets: $VPC_SUBNETS"
else
  echo "  skipped (NETWORK=0) — functions will run on the AWS-managed network"
fi

step "4/10  compute"
# ⚠️ Retry loop, not a fixed sleep. A role created seconds ago is not yet visible
# to CloudFormation's early validation, which fails with a detail-free
# AWS::EarlyValidation::PropertyValidation. The identical template succeeds once
# IAM has propagated. A `sleep 30` would be a guess that is both too long most
# of the time and too short occasionally; retrying until it takes is neither.
for attempt in 1 2 3 4 5 6; do
  if aws cloudformation deploy --template-file infra/20-lambda.yaml --stack-name releasebot-lambda \
      --parameter-overrides ArtifactBucket="$BUCKET" \
        CutReleaseRoleArn="arn:aws:iam::$ACCT:role/releasebot-exec-cutrelease" \
        MergebackRoleArn="arn:aws:iam::$ACCT:role/releasebot-exec-mergeback" \
        DefaultOwner=msnbc SlackChannel=C0RELEASE SecretsPrefix=releasebot \
        LogRetentionDays=7 Runtime=provided.al2 \
        "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" >/dev/null 2>&1; then
    echo "  four functions deployed (attempt $attempt)"; break
  fi
  [ "$attempt" = 6 ] && { echo "  compute stack failed after 6 attempts"; exit 1; }
  echo "  attempt $attempt failed (IAM not propagated yet), retrying in 10s"
  sleep 10
done

step "5/10  edge (API, two stages wired to the aliases)"
L="arn:aws:lambda:$AWS_REGION:$ACCT:function"
aws cloudformation deploy --template-file infra/30-apigw.yaml --stack-name releasebot-edge \
  --parameter-overrides \
    "CutReleaseAliasArn=${L}:cutRelease:live" \
    "CutReleaseTestAliasArn=${L}:cutReleaseTest:live" \
    "MergebackAliasArn=${L}:releaseAutomationMergeback:live" \
    "MergebackTestAliasArn=${L}:releaseAutomationMergebackTest:live" | tail -1

BASE=$(aws cloudformation describe-stacks --stack-name releasebot-edge \
  --query "Stacks[0].Outputs[?OutputKey=='ProdInvokeUrl'].OutputValue" --output text)
BASE="${BASE%/prod}"
echo "  api: $BASE"

step "6/10  configure, publish a version, move the alias"
# Exactly the real deploy path: change config, publish an immutable version,
# repoint the alias. Rollback is then one update-alias call.
configure() {
  local FN="$1" ENVV="$2"
  aws lambda update-function-configuration --function-name "$FN" \
    --environment "Variables={RARC_ENV=$ENVV,DEFAULT_OWNER=msnbc,SLACK_CHANNEL=C0RELEASE,RELEASEBOT_DRY_RUN=1,GITHUB_TOKEN=sandbox-not-a-real-token,SLACK_TOKEN=sandbox-not-a-real-token,SLACK_SIGNING_SECRET=$SIG,JIRA_WEBHOOK_SECRET=$JIRA,DD_SERVICE=releasebot,DD_ENV=$ENVV}" >/dev/null
  aws lambda wait function-updated-v2 --function-name "$FN"
  local V; V=$(aws lambda publish-version --function-name "$FN" --query Version --output text)
  aws lambda update-alias --function-name "$FN" --name live --function-version "$V" >/dev/null
  echo "  $FN -> version $V (alias live, env $ENVV)"
}
configure cutRelease                      prod
configure cutReleaseTest                  test
configure releaseAutomationMergeback      prod
configure releaseAutomationMergebackTest  test

step "7/10  monitoring (alarms, dashboard; canary off by default)"
aws cloudformation deploy --template-file infra/40-observability.yaml \
  --stack-name releasebot-observability --capabilities CAPABILITY_IAM \
  --parameter-overrides ApiBaseUrl="$BASE/prod" CanaryBucket="$BUCKET" EnableCanary=false | tail -1

step "7.5/10  the approval endpoint and the dashboard"
# ⚠️ THESE WERE MISSING FROM THIS SCRIPT, and that is the defect this rebuild
# exists to prevent: four of the eight templates were deployed by hand during
# development and never added here. An environment that cannot be rebuilt from
# the repository is an environment that only exists in the account -- which is
# exactly the thing this whole project is about migrating away from.
#
# Both need credentials that are NOT in this repository. They deploy with
# placeholders and the two token scripts put the real ones in:
#     scripts/set-approval-token.sh   (classic PAT  -> slackApprove)
#     scripts/set-write-token.sh      (fine-grained -> the four functions)
API_ID=$(aws cloudformation describe-stacks --stack-name releasebot-edge \
  --query "Stacks[0].Parameters[?ParameterKey=='RestApiId'].ParameterValue" --output text 2>/dev/null)
[ -n "$API_ID" ] && [ "$API_ID" != "None" ] || \
  API_ID=$(aws apigateway get-rest-apis --query "items[?name=='releasebot'].id | [0]" --output text)
BETA_ID=$(aws apigateway get-resources --rest-api-id "$API_ID" \
  --query "items[?pathPart=='beta'].id | [0]" --output text)

if [ -n "${SLACK_APPROVERS:-}" ]; then
  aws cloudformation deploy --template-file infra/50-slack-approve.yaml \
    --stack-name releasebot-slack-approve --capabilities CAPABILITY_IAM \
    --parameter-overrides ArtifactBucket="$BUCKET" RestApiId="$API_ID" \
      BetaResourceId="$BETA_ID" StageName=prod SlackApprovers="$SLACK_APPROVERS" \
      GitHubOwner="${GITHUB_OWNER:-vamsiatluri}" GitHubRepo="${GITHUB_REPO:-releasebot-demo}" \
      SlackSigningSecret="$SIG" GitHubApprovalToken=placeholder-run-set-approval-token \
      "VpcSubnetIds=$VPC_SUBNETS" "VpcSecurityGroupIds=$VPC_SG" | tail -1
else
  echo "  slack-approve skipped (set SLACK_APPROVERS=U123,U456 to deploy it)"
fi

if [ -n "${ADMIN_EMAIL:-}" ]; then
  AWS_PROFILE="$AWS_PROFILE" bash scripts/deploy-dashboard.sh "$ADMIN_EMAIL" | tail -4
else
  echo "  dashboard skipped (set ADMIN_EMAIL=you@example.com to deploy it)"
fi

step "8/10  prove it works"
go build -o build/contracttest ./cmd/contracttest
RC=0
for stage in test prod; do
  SLACK_SIGNING_SECRET="$SIG" JIRA_WEBHOOK_SECRET="$JIRA" \
    ./build/contracttest -base "$BASE/$stage" -env "$stage" \
      -metrics "build/metrics-$stage.json" || RC=$?
  aws cloudwatch put-metric-data --namespace ReleaseBot \
    --metric-data "file://build/metrics-$stage.json" 2>/dev/null || true
done

echo
echo "  api base:   $BASE"
echo "  dashboard:  https://$AWS_REGION.console.aws.amazon.com/cloudwatch/home?region=$AWS_REGION#dashboards:name=ReleaseBot"
[ "$RC" -eq 0 ] && echo "  BUILD VERIFIED: every behaviour check passed on both stages." \
                || { echo "  BUILD FAILED VERIFICATION (exit $RC)"; exit "$RC"; }
