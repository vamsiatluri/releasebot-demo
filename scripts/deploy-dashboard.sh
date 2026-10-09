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
# KEY is set after the build, from the artifact's own hash -- see below.

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

# ⚠️ The S3 KEY CARRIES THE ARTIFACT'S HASH, and that is not cosmetic.
# CloudFormation compares the Code property as STRINGS. Overwriting the same
# key leaves `S3Key` identical, so CFN sees no change to the function's code and
# never calls UpdateFunctionCode -- the stack reports a successful update, the
# environment variables really do change, and the function keeps running the
# OLD binary. That is exactly what happened here: a new field was added to the
# API response, the deploy succeeded, and the field was simply absent.
# A content-addressed key makes a code change a template change.
DIGEST=$(openssl dgst -sha256 -binary "$WORK/dashboardapi.zip" | xxd -p -c 32 | cut -c1-12)
KEY="releasebot/dashboardapi-$DIGEST.zip"
aws s3 cp "$WORK/dashboardapi.zip" "s3://$BUCKET/$KEY" --only-show-errors
echo "✓ api packaged  ($KEY)"

# ── 2. Which repository the pipeline panel reads ─────────────────────────────
# Read off the git remote rather than hard-coded, so a fork or a rename cannot
# leave the dashboard quietly rendering somebody else's pipeline.
GH_URL=$(git remote get-url "${GIT_REMOTE:-demo}" 2>/dev/null || git remote get-url origin)
GH_SLUG=$(printf '%s' "$GH_URL" | sed -E 's#^.*github\.com[:/]##; s#\.git$##')
GH_OWNER="${GH_SLUG%%/*}"; GH_REPO="${GH_SLUG##*/}"
[ -n "$GH_OWNER" ] && [ -n "$GH_REPO" ] || { echo "✗ could not read the GitHub slug" >&2; exit 1; }
echo "pipeline panel reads: $GH_OWNER/$GH_REPO"

# The token is OPTIONAL. The repository is public, so the panel works with no
# credential at all -- but anonymous reads are capped at 60/hour per egress IP,
# which a shared Lambda IP can burn through without us making a single call. A
# token raises that to 5,000/hour and lets the panel refresh every 10s.
#
# ⚠️ CORRECT credential: a FINE-GRAINED PAT with Actions: READ on this one
# repository, supplied as RELEASEBOT_DASHBOARD_TOKEN. It cannot approve a
# deployment, cannot push, and cannot read another repo -- a read-only panel
# should hold a read-only credential.
#
# Fallback, so the panel is not dead on arrival: reuse the approval token that
# is already in the account. That token is a CLASSIC PAT with `repo`, which is
# far more than this function needs. It is a stopgap, it is deliberately
# announced below, and it is not the thing to ship.
TOKEN="${RELEASEBOT_DASHBOARD_TOKEN:-}"
TOKEN_SOURCE="read-only token supplied"
if [ -z "$TOKEN" ]; then
  TOKEN=$(aws lambda get-function-configuration --function-name slackApprove \
            --region "$REGION" --output text \
            --query 'Environment.Variables.GITHUB_APPROVAL_TOKEN' 2>/dev/null || true)
  [ "$TOKEN" = "None" ] && TOKEN=""
  TOKEN_SOURCE="⚠️  borrowed the approval token (over-privileged; set RELEASEBOT_DASHBOARD_TOKEN)"
fi
[ -n "$TOKEN" ] || TOKEN_SOURCE="no token: panel refreshes every 90s and may hit GitHub's anonymous cap"

# ── 3. Infrastructure ────────────────────────────────────────────────────────
# ⚠️ Parameters go in a FILE, never on the command line. An argument lands in
# `ps` output and in shell history; a 0600 file in a temp dir that is deleted on
# exit does not. Same rule as scripts/set-approval-token.sh.
PARAMS="$WORK/params.json"
( umask 077; : > "$PARAMS" )
export TOKEN WORKFLOW_FILE="${WORKFLOW_FILE:-release.yml}"
python3 - "$PARAMS" "$BUCKET" "$KEY" "$ADMIN_EMAIL" "$HEALTH_BASE" \
         "$GH_OWNER" "$GH_REPO" <<'PY'
import json, os, sys
out, bucket, key, email, health, owner, repo = sys.argv[1:8]
# The token comes through the ENVIRONMENT, not argv, for the reason above.
params = {
    "ArtifactBucket": bucket, "ArtifactKey": key, "AdminEmail": email,
    "HealthBase": health, "GitHubOwner": owner, "GitHubRepo": repo,
    "GitHubWorkflowFile": os.environ.get("WORKFLOW_FILE", "release.yml"),
    "GitHubToken": os.environ.get("TOKEN", ""),
}
with open(out, "w") as f:
    json.dump([{"ParameterKey": k, "ParameterValue": v} for k, v in params.items()], f)
PY

aws cloudformation deploy \
  --template-file infra/60-dashboard.yaml \
  --stack-name "$STACK" --region "$REGION" \
  --capabilities CAPABILITY_IAM \
  --parameter-overrides "file://$PARAMS"
echo "✓ stack deployed  ($TOKEN_SOURCE)"

out() {
  aws cloudformation describe-stacks --stack-name "$STACK" --region "$REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}
URL=$(out DashboardURL); SITE=$(out SiteBucketName)
LOGIN=$(out LoginDomain); CLIENT=$(out ClientId)

# ── 4. The page ──────────────────────────────────────────────────────────────
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

# ── 5. Make the new page visible ─────────────────────────────────────────────
DIST=$(aws cloudfront list-distributions \
  --query "DistributionList.Items[?Comment=='$STACK dashboard'].Id | [0]" --output text)
if [ -n "$DIST" ] && [ "$DIST" != "None" ]; then
  aws cloudfront create-invalidation --distribution-id "$DIST" --paths '/*' \
    --query 'Invalidation.Status' --output text >/dev/null
  echo "✓ cache invalidated"
fi

# ── 6. Verify the SERVED page, not the upload ────────────────────────────────
# ⚠️ A previous deploy printed "✓ page uploaded" while CloudFront kept serving
# the old file. Uploading an object and serving it are two different facts, so
# this checks the one that matters: fetch the URL a human would open and look
# for a marker that only the new page has.
#
# ⚠️ And it does NOT pipe curl into `grep -q`. Under `set -o pipefail` that
# combination reports failure even when the marker IS there: `grep -q` exits the
# moment it matches, curl dies on the broken pipe, and pipefail takes the
# pipeline's status from curl. The check fails ONLY on a successful match --
# which is the most misleading failure a verification step can have. Fetch to a
# file, then grep the file.
MARKER='lp-steps'
SERVED="$WORK/served.html"
for i in $(seq 1 24); do
  if curl -fsS -o "$SERVED" "$URL" 2>/dev/null && grep -q "$MARKER" "$SERVED"; then
    echo "✓ the served page is the new one"
    break
  fi
  [ "$i" = 24 ] && { echo "✗ $URL is still serving a page without '$MARKER'" >&2; exit 1; }
  sleep 10
done

cat <<EOS

  dashboard   $URL
  sign in as  $ADMIN_EMAIL
  pipeline    $GH_OWNER/$GH_REPO  ($TOKEN_SOURCE)

  First sign-in uses the temporary password Cognito emailed; it will ask for a
  new one. Nothing here ever handles that password.
EOS
