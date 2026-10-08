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

step "1/8  build and package"
make package 2>&1 | tail -2

step "2/8  artifact bucket"
aws s3api head-bucket --bucket "$BUCKET" 2>/dev/null || \
  aws s3api create-bucket --bucket "$BUCKET" >/dev/null
aws s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3 cp build/cutrelease.zip "s3://$BUCKET/releasebot/cutrelease.zip" --only-show-errors
aws s3 cp build/mergeback.zip  "s3://$BUCKET/releasebot/mergeback.zip"  --only-show-errors
echo "  artifacts uploaded to $BUCKET"

step "3/8  identity (roles, OIDC trust, API Gateway account logging role)"
aws cloudformation deploy --template-file infra/10-iam.yaml --stack-name releasebot-iam \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides GitHubOrg=vamsiatluri GitHubRepo=releasebot-migration \
    CreateOIDCProvider=true SecretsPrefix=releasebot SetApiGatewayAccountRole=true | tail -1

step "4/8  compute"
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
        LogRetentionDays=7 Runtime=provided.al2 >/dev/null 2>&1; then
    echo "  four functions deployed (attempt $attempt)"; break
  fi
  [ "$attempt" = 6 ] && { echo "  compute stack failed after 6 attempts"; exit 1; }
  echo "  attempt $attempt failed (IAM not propagated yet), retrying in 10s"
  sleep 10
done

step "5/8  edge (API, two stages wired to the aliases)"
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

step "6/8  configure, publish a version, move the alias"
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

step "7/8  monitoring (alarms, dashboard; canary off by default)"
aws cloudformation deploy --template-file infra/40-observability.yaml \
  --stack-name releasebot-observability --capabilities CAPABILITY_IAM \
  --parameter-overrides ApiBaseUrl="$BASE/prod" CanaryBucket="$BUCKET" EnableCanary=false | tail -1

step "8/8  prove it works"
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
