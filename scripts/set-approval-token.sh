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
TOKEN=$(printf '%s' "$TOKEN" | tr -d '[:space:]')

[ -n "$TOKEN" ] || { echo "no token on stdin (is the clipboard empty?)" >&2; exit 2; }

case "$TOKEN" in
  github_pat_*)
    echo "✗ that is a FINE-GRAINED token, which this endpoint cannot use." >&2
    echo "  It needs a classic token (ghp_…) with the 'repo' scope." >&2
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

PARAMS="$(mktemp)"; trap 'rm -f "$PARAMS"' EXIT
chmod 600 "$PARAMS"
export PARAMS TOKEN
python3 "$HERE/_approval-params.py"
unset TOKEN

aws cloudformation update-stack --stack-name "$STACK" --region "$REGION" \
  --use-previous-template --capabilities CAPABILITY_IAM \
  --parameters "file://$PARAMS" >/dev/null
echo "… updating $STACK"
aws cloudformation wait stack-update-complete --stack-name "$STACK" --region "$REGION"
echo "✓ endpoint now holds the new token — click Approve in Slack"
