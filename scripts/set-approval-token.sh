#!/usr/bin/env bash
# Swap the GitHub token the Slack approve endpoint uses.
#
#   scripts/set-approval-token.sh            # prompts; typing is hidden
#   pbpaste | scripts/set-approval-token.sh --stdin
#
# ⚠️ It must be a **classic** personal access token with the `repo` scope.
#
# A fine-grained token cannot review pending deployments AT ALL. GitHub answers
#   403 "Resource not accessible by personal access token"
# no matter which permissions the token carries -- which reads like a permission
# you forgot to tick, and is not one. The endpoint accepts only OAuth app tokens
# and classic tokens:
#   docs.github.com/rest/actions/workflow-runs#review-pending-deployments-for-a-workflow-run
#
# The token is read from the terminal or stdin and handed to CloudFormation in a
# parameters FILE. It is never a command-line argument: arguments are visible in
# `ps` to every other process on the box, and land in shell history.
set -euo pipefail

STACK="${STACK:-releasebot-slack-approve}"
REGION="${AWS_REGION:-us-east-1}"
: "${AWS_PROFILE:=releasebot-sandbox}"
export AWS_PROFILE STACK REGION

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ⚠️ `read` returns 1 when it hits EOF without seeing its delimiter, EVEN THOUGH
# it has populated the variable. `pbpaste` emits no trailing newline, so under
# `set -e` this line exited the script -- silently, before a single line of
# output, with the token read correctly and then thrown away. The symptom is a
# command that appears to do nothing at all.
TOKEN=""
if [ "${1:-}" = "--stdin" ]; then
  IFS= read -r TOKEN || true
else
  printf 'classic PAT (repo scope), input hidden: ' >&2
  IFS= read -rs TOKEN || true
  printf '\n' >&2
fi

# A clipboard copy routinely carries a trailing newline, a stray space, or CRLF
# from a browser. A token is alphanumerics and underscores, so stripping every
# whitespace character cannot damage a valid one -- and a token with an
# invisible newline on the end fails authentication with a 401 that looks like
# the wrong token rather than the right one, badly pasted.
# ⚠️ Bracketed paste: terminals wrap a paste in ESC[200~ ... ESC[201~, `read`
# captures those bytes, and every prefix check then fails on the escape
# sequence instead of the token -- reporting a bad token when the token is
# fine. ESC is not whitespace, so tr -d '[:space:]' does not remove it.
TOKEN=$(printf '%s' "$TOKEN" \
  | sed $'s/\033\[20[01]~//g' \
  | tr -d '[:cntrl:][:space:]')

[ -n "$TOKEN" ] || { echo "no token on stdin (is the clipboard empty?)" >&2; exit 2; }

case "$TOKEN" in
  github_pat_*)
    echo "✗ that is a FINE-GRAINED token, which this endpoint cannot use." >&2
    echo "  It needs a classic token (ghp_...) with the 'repo' scope." >&2
    echo "  Refusing to store one that can only fail at the click." >&2
    exit 3 ;;
  ghp_*) ;;
  *) echo "⚠ unexpected prefix; continuing, but a classic PAT starts ghp_" >&2 ;;
esac

# Prove the token works BEFORE deploying it. A token that cannot read the repo
# cannot approve anything either, and finding that out from a Slack click two
# days later is the expensive way to learn it.
code=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' \
  "https://api.github.com/repos/vamsiatluri/releasebot-demo")
[ "$code" = "200" ] || { echo "✗ token cannot read the repo (HTTP $code)" >&2; exit 4; }
echo "✓ token reads the repo"

# ── Verify the token can do the ONE THING it exists for ─────────────────────
#
# ⛔ Reading the repo is NOT evidence it can approve. A fine-grained token reads
# a public repo with no permissions at all, and the approve endpoint refuses
# fine-grained tokens entirely — so "it reads the repo" is exactly the check
# that lets the wrong credential through. The classic/fine-grained prefix test
# above is the real guard; this is a second, narrower one.
#
# ⚠️ An empty x-oauth-scopes header means the token is NOT classic, whatever its
# prefix claims. Classic tokens always report their scopes.
SCOPES=$(curl -sS -D - -o /dev/null -H "Authorization: Bearer $TOKEN" \
           https://api.github.com/user 2>/dev/null \
         | grep -i '^x-oauth-scopes:' | cut -d: -f2- | tr -d '\r' | sed 's/^ *//')
if [ -z "$SCOPES" ]; then
  echo "✗ GitHub reports NO scopes for this token." >&2
  echo "  Classic tokens always report their scopes; an empty header means this" >&2
  echo "  is not one, and the approve endpoint will return 403 at the click." >&2
  exit 5
fi
case "$SCOPES" in
  *repo*) echo "✓ classic token, scopes: $SCOPES" ;;
  *) echo "✗ token lacks the 'repo' scope (has: $SCOPES)." >&2
     echo "  A public repo still needs 'repo', not 'public_repo'." >&2
     exit 6 ;;
esac

# ── Write it where the function actually reads from ─────────────────────────
#
# ⛔ THIS USED TO UPDATE A CLOUDFORMATION PARAMETER, AND THAT IS NOW INERT.
# The function resolves its secrets from SSM at INIT (internal/secretref), so
# the template's GitHubApprovalToken parameter is no longer read when
# UseSecretRefs is on. Updating it would report success and change nothing --
# the exact failure this whole project keeps finding, committed by the script
# whose only job is to set this value.
#
# Which parameter to write is read FROM THE STACK rather than hardcoded, so the
# two can never drift apart.
NAME=$(aws cloudformation describe-stacks --stack-name "$STACK" --region "$REGION" \
         --query "Stacks[0].Parameters[?ParameterKey=='GitHubApprovalTokenParam'].ParameterValue | [0]" \
         --output text 2>/dev/null)
if [ -z "$NAME" ] || [ "$NAME" = "None" ]; then
  echo "✗ $STACK has no GitHubApprovalTokenParam — this stack predates the" >&2
  echo "  secret-reference change. Deploy infra/50-slack-approve.yaml first." >&2
  exit 7
fi
echo "  destination: $NAME"

# rotate-secret.sh writes the SecureString, verifies the type and the round
# trip, AND recycles the function -- which is required, because the value is
# read once at INIT and a warm environment keeps serving the old one.
RELEASEBOT_SECRET="$TOKEN" bash "$HERE/rotate-secret.sh" "$NAME" || exit 1
unset TOKEN

echo "✓ the approval endpoint now resolves the new token — click Approve in Slack"
