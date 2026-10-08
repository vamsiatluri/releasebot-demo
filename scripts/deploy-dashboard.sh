#!/usr/bin/env bash
# Build and deploy the management dashboard: Cognito, the status API, the
# private bucket, and the CloudFront distribution in front of both.
#
#   scripts/deploy-dashboard.sh <admin-email>
#
# The admin email is the ONE account that may sign in. Cognito emails them a
# temporary password when the user is first created; this script never sees it
# and never prints one.
#
# ⚠️ CloudFront takes several minutes to deploy on first create. A fast run on
# the first deploy means something did not happen.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

: "${AWS_PROFILE:=releasebot-sandbox}"
REGION="${AWS_REGION:-us-east-1}"
STACK="${STACK:-releasebot-dashboard}"
export AWS_PROFILE AWS_PAGER=""

ADMIN_EMAIL="${1:-}"
[ -n "$ADMIN_EMAIL" ] || { echo "usage: $0 <admin-email>" >&2; exit 2; }

ACCT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="releasebot-artifacts-$ACCT"
KEY="releasebot/dashboardapi.zip"

HEALTH_BASE=$(aws cloudformation describe-stacks --stack-name releasebot-edge --region "$REGION" \
  --query "Stacks[0].Outputs[?OutputKey=='ApiBaseUrl'].OutputValue" --output text 2>/dev/null)
if [ -z "$HEALTH_BASE" ] || [ "$HEALTH_BASE" = "None" ]; then
  API_ID=$(aws cloudformation describe-stacks --stack-name releasebot-edge --region "$REGION" \
    --query "Stacks[0].Parameters[?ParameterKey=='RestApiId'].ParameterValue" --output text 2>/dev/null)
  [ -n "$API_ID" ] && [ "$API_ID" != "None" ] \
    || API_ID=$(aws apigateway get-rest-apis --region "$REGION" \
         --query "items[?name=='releasebot'].id | [0]" --output text)
  HEALTH_BASE="https://${API_ID}.execute-api.${REGION}.amazonaws.com"
fi
echo "health base: $HEALTH_BASE"

# ── 1. Package the API ───────────────────────────────────────────────────────
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o "$WORK/bootstrap" ./cmd/dashboardapi
( cd "$WORK" && zip -q dashboardapi.zip bootstrap )
unzip -l "$WORK/dashboardapi.zip" | grep -qE ' bootstrap$' \
  || { echo "✗ bootstrap is not at the root of the zip" >&2; exit 1; }
aws s3 cp "$WORK/dashboardapi.zip" "s3://$BUCKET/$KEY" --only-show-errors
echo "✓ api packaged"

# ── 2. Infrastructure ────────────────────────────────────────────────────────
aws cloudformation deploy \
  --template-file infra/60-dashboard.yaml \
  --stack-name "$STACK" --region "$REGION" \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides \
      ArtifactBucket="$BUCKET" \
      ArtifactKey="$KEY" \
      AdminEmail="$ADMIN_EMAIL" \
      HealthBase="$HEALTH_BASE"
echo "✓ stack deployed"

out() {
  aws cloudformation describe-stacks --stack-name "$STACK" --region "$REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}
URL=$(out DashboardURL); SITE=$(out SiteBucketName)
LOGIN=$(out LoginDomain); CLIENT=$(out ClientId)

# ── 3. The page ──────────────────────────────────────────────────────────────
# The login domain and client id are PUBLIC values -- they appear in every
# OAuth redirect. Substituting them at deploy time keeps the committed page free
# of account-specific strings.
sed -e "s|__LOGIN_DOMAIN__|$LOGIN|g" -e "s|__CLIENT_ID__|$CLIENT|g" \
    web/index.html > "$WORK/index.html"
grep -q '__LOGIN_DOMAIN__\|__CLIENT_ID__' "$WORK/index.html" \
  && { echo "✗ a placeholder survived substitution" >&2; exit 1; }
aws s3 cp "$WORK/index.html" "s3://$SITE/index.html" \
  --content-type 'text/html; charset=utf-8' --cache-control 'no-cache' --only-show-errors
echo "✓ page uploaded"

# ── 4. Make the new page visible ─────────────────────────────────────────────
DIST=$(aws cloudfront list-distributions \
  --query "DistributionList.Items[?Comment=='$STACK dashboard'].Id | [0]" --output text)
if [ -n "$DIST" ] && [ "$DIST" != "None" ]; then
  aws cloudfront create-invalidation --distribution-id "$DIST" --paths '/*' \
    --query 'Invalidation.Status' --output text >/dev/null
  echo "✓ cache invalidated"
fi

cat <<EOS

  dashboard   $URL
  sign in as  $ADMIN_EMAIL

  First sign-in uses the temporary password Cognito emailed; it will ask for a
  new one. Nothing here ever handles that password.
EOS
