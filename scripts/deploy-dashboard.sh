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
# ⚠️ Listed to a FILE first: `unzip -l | grep -q` under pipefail fails when it
# MATCHES (grep exits first, unzip gets SIGPIPE).
unzip -l "$WORK/dashboardapi.zip" > "$WORK/zip.listing"
grep -qE ' bootstrap$' "$WORK/zip.listing" \
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

# ── The dashboard's GitHub credential ────────────────────────────────────────
#
# This page only ever READS a PUBLIC repository, so it needs no permission at
# all. What it needs is an IDENTITY: anonymous reads are capped at 60/hour PER
# EGRESS IP and a Lambda's egress is a shared NAT address, so an ordinary busy
# afternoon exhausts the cap for everyone behind it. A credential moves that to
# 5000/hour. Conditional requests already make a 304 free, which is why the page
# works with no credential at all -- but a burst of real changes still hits 60.
#
# ⛔ SO IT IS A ZERO-PERMISSION TOKEN, and the whole point is that it can do
# nothing. scripts/set-dashboard-token.sh refuses a classic PAT, refuses one
# reporting any scope, and ATTEMPTS A WRITE -- refusing the token if the write
# succeeds. verify-secrets.sh re-runs that check.
#
# ⚠️ THE DEFAULT IS THE SSM REFERENCE, NOT EMPTY, and that is a defect fix.
# `GitHubToken` defaults to '' in the template, so a deploy that simply did not
# mention it reported UPDATE_COMPLETE while leaving GITHUB_TOKEN="" -- the
# dashboard stayed anonymous and nothing said so. Making the reference the
# default means the deploy has to be told to go WITHOUT a credential, rather
# than falling into it silently.
TOKEN_PARAM="/releasebot/${TOKEN_STAGE:-prod}/secrets/dashboard-github-token"
TOKEN="${RELEASEBOT_DASHBOARD_TOKEN-}"
if [ -z "${RELEASEBOT_DASHBOARD_TOKEN+set}" ]; then
  # Unset (as opposed to deliberately empty): adopt the reference IF the
  # parameter actually exists in THIS account. An `ssm:` reference to a missing
  # parameter would resolve to nothing and degrade to anonymous -- working,
  # slower, and for a reason nothing would explain.
  if aws ssm get-parameter --name "$TOKEN_PARAM" --region "$REGION" \
       --query 'Parameter.Name' --output text >/dev/null 2>&1; then
    TOKEN="ssm:$TOKEN_PARAM"
  fi
fi

case "$TOKEN" in
  ssm:*|secretsmanager:*)
        TOKEN_SOURCE="reference to $TOKEN_PARAM (resolved at INIT, zero-permission)" ;;
  github_pat_*)
        # ⚠️ A literal here lands in the function's environment in plaintext and
        # is visible to anyone with lambda:GetFunctionConfiguration.
        TOKEN_SOURCE="⚠️  a LITERAL fine-grained token — prefer ssm: (scripts/set-dashboard-token.sh)" ;;
  ghp_*)
        echo "✗ that is a CLASSIC token. A read-only status page must not hold one." >&2
        echo "  Run scripts/set-dashboard-token.sh, which refuses anything that can write." >&2
        exit 1 ;;
  '')   TOKEN_SOURCE="no credential — public repo + conditional requests, capped at 60/hour" ;;
  *)    TOKEN_SOURCE="token supplied (unrecognised prefix)" ;;
esac

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
#
# ⚠️ And it compares a HASH, not a marker string. The first version of this
# check looked for an element id that only the new page had -- which went stale
# the moment that element was renamed, and then failed a deploy that had
# actually worked. A verification step whose own correctness depends on being
# edited alongside the thing it verifies will eventually lie. The hash of the
# file we uploaded cannot go stale.
WANT=$(openssl dgst -sha256 -binary "$WORK/index.html" | xxd -p -c 32)
SERVED="$WORK/served.html"
for i in $(seq 1 24); do
  if curl -fsS -o "$SERVED" "$URL" 2>/dev/null; then
    GOT=$(openssl dgst -sha256 -binary "$SERVED" | xxd -p -c 32)
    if [ "$GOT" = "$WANT" ]; then echo "✓ the served page IS the page we uploaded"; break; fi
  fi
  [ "$i" = 24 ] && { echo "✗ $URL is serving something other than what we just uploaded" >&2
                     echo "  want $WANT" >&2; echo "  got  ${GOT:-<unfetchable>}" >&2; exit 1; }
  sleep 10
done

# ── 7. Prove the credential ARRIVED and WORKS ────────────────────────────────
# ⛔ THIS STEP EXISTS BECAUSE EVERY OTHER SIGNAL IS SILENT.
#
# The template defaults `GitHubToken` to '', so a deploy that omitted it left
# GITHUB_TOKEN="" and the dashboard read GitHub anonymously at 60/hour -- while
# reporting UPDATE_COMPLETE. And the function's own logs do not help: the
# resolver logs only on FAILURE, and the no-token path returns without logging
# at all, so a correctly wired token and an empty one produce byte-identical
# logs. "No error in the log" is not evidence.
#
# So ask the other side. `X-RateLimit-Limit` is 60 anonymous against 5000
# authenticated, GitHub asserts it on every response, and the API now surfaces
# it as run.budget. Invoking the function reads the state the browser would get.
echo "── proving the GitHub credential"
EV="$WORK/probe.json"; RESP="$WORK/probe-out.json"
printf '{"path":"/api/state","httpMethod":"GET"}' > "$EV"
aws lambda invoke --function-name "$(aws cloudformation describe-stack-resource \
      --stack-name "$STACK" --region "$REGION" --logical-resource-id ApiFunction \
      --query 'StackResourceDetail.PhysicalResourceId' --output text)" \
  --region "$REGION" --payload "fileb://$EV" --cli-read-timeout 60 \
  "$RESP" --query 'StatusCode' --output text >/dev/null

# ⚠️ Read the BUDGET, not the status code. A 200 carrying an anonymous budget is
# exactly the outcome this is here to catch.
BUDGET=$(python3 - "$RESP" <<'PY2'
import json, sys
try:
    outer = json.load(open(sys.argv[1]))
    body = json.loads(outer.get("body") or "{}")
    b = (body.get("run") or {}).get("budget")
except Exception as e:
    print("unreadable:%s" % e); raise SystemExit(0)
if not b:
    # GitHub was never reached on this invocation -- no headers, so nothing to
    # report either way. Not a pass and not a failure.
    print("unknown")
else:
    print("%s:%d:%d" % ("authed" if b.get("authed") else "anon",
                        b.get("limit", 0), b.get("remaining", 0)))
PY2
)

case "$BUDGET" in
  authed:*)
    echo "  ✓ GitHub saw a credential — limit $(echo "$BUDGET" | cut -d: -f2)/hour, $(echo "$BUDGET" | cut -d: -f3) left" ;;
  anon:*)
    if [ -n "$TOKEN" ]; then
      echo "✗ a credential was configured but GitHub read this as ANONYMOUS" >&2
      echo "  limit $(echo "$BUDGET" | cut -d: -f2)/hour. The token did not reach the function," >&2
      echo "  or did not resolve. Check the function log for 'anonymous', and that the" >&2
      echo "  execution role can ssm:GetParameter + kms:Decrypt on $TOKEN_PARAM." >&2
      exit 1
    fi
    echo "  ⚠️  anonymous, $(echo "$BUDGET" | cut -d: -f3) of $(echo "$BUDGET" | cut -d: -f2) reads left this hour (no credential configured)" ;;
  unknown)
    echo "  ⚠️  GitHub was not reached on this probe — budget unknown, not proven either way" ;;
  *)
    echo "  ⚠️  could not read the probe response ($BUDGET)" ;;
esac

cat <<EOS

  dashboard   $URL
  sign in as  $ADMIN_EMAIL
  pipeline    $GH_OWNER/$GH_REPO  ($TOKEN_SOURCE)

  First sign-in uses the temporary password Cognito emailed; it will ask for a
  new one. Nothing here ever handles that password.
EOS
