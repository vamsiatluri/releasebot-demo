#!/usr/bin/env bash
# Put the RELEASE WRITE token on the four ReleaseBot functions.
#
#   RELEASEBOT_WRITE_TOKEN='github_pat_...' bash scripts/set-write-token.sh
#   bash scripts/set-write-token.sh --stdin        # reads one line from stdin
#
# ─────────────────────────────────────────────────────────────────────────────
# WHY THERE ARE TWO TOKENS, AND WHY THIS ONE REFUSES A CLASSIC PAT
#
# They do different jobs and need different powers, and the only reason they
# were ever the same token is that I copied one across:
#
#   slackApprove      POST /actions/runs/{id}/pending_deployments
#                     → CLASSIC PAT with `repo`. A fine-grained token CANNOT do
#                       this at all; the 403 reads as a missing permission even
#                       when Actions: write is granted. See
#                       scripts/set-approval-token.sh, which refuses a
#                       fine-grained token for exactly that reason.
#
#   the 4 functions   GET  /git/ref/heads/{branch}        contents: read
#                     POST /git/refs                      contents: WRITE
#                     GET  /compare/{base}...{head}       contents: read
#                     GET  /pulls                         pull requests: read
#                     POST /pulls                         pull requests: WRITE
#                     → that is the WHOLE list. No Actions. No admin. No org.
#                       A FINE-GRAINED PAT scoped to the release repositories
#                       with Contents + Pull requests is exactly right, and a
#                       classic `repo` token is push access to every repository
#                       the owner can reach.
#
# So this script is the mirror image of set-approval-token.sh: that one refuses
# fine-grained, this one refuses classic. Between them the two credentials
# cannot be swapped by accident.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

: "${AWS_PROFILE:=releasebot-sandbox}"
REGION="${AWS_REGION:-us-east-1}"
export AWS_PROFILE AWS_PAGER=""

# ⚠️ `read` returns 1 at EOF without seeing its delimiter EVEN THOUGH it has
# populated the variable, and pbpaste emits no trailing newline -- so under
# `set -e` this line silently exits the script. Cost a debugging round once.
TOKEN="${RELEASEBOT_WRITE_TOKEN:-}"
if [ "${1:-}" = "--stdin" ]; then
  IFS= read -r TOKEN || true
fi
# ⚠️ BRACKETED PASTE. Most terminals wrap a paste in ESC[200~ ... ESC[201~ so
# an application can tell typed input from pasted input. `read -s` captures
# those bytes like any others, so a pasted token arrives as
# ESC[200~github_pat_xxx and every prefix check fails on the escape sequence
# rather than on the token. The error then reads "unrecognised token prefix",
# which sends you to look at the TOKEN -- and the token was fine.
#
# Strip the paste markers and every other control character before looking at
# anything. tr -d '[:space:]' alone does not: ESC is not whitespace.
clean() {
  printf '%s' "$1" \
    | sed $'s/\033\[20[01]~//g' \
    | tr -d '[:cntrl:][:space:]'
}
TOKEN=$(clean "$TOKEN")

# Nothing supplied: ASK, silently. This is the best of the three ways in --
# nothing on argv (visible in `ps`), nothing in the environment of a logged
# shell, nothing in history. `read -s` echoes nothing.
if [ -z "$TOKEN" ] && [ -t 0 ]; then
  printf 'fine-grained GitHub token (input hidden): ' >&2
  IFS= read -rs TOKEN || true
  printf '\n' >&2
  TOKEN=$(clean "$TOKEN")
fi

[ -n "$TOKEN" ] || {
  echo "usage: $0            # prompts, input hidden -- the safest way" >&2
  echo "   or: pbpaste | $0 --stdin" >&2
  echo "   or: RELEASEBOT_WRITE_TOKEN=<token> $0" >&2; exit 2; }

# ⚠️ Catch the PLACEHOLDER before spending a round trip on it. A documented
# command containing `github_pat_...` is a thing people paste verbatim -- I
# handed one over myself -- and the result is an HTTP 401 that reads as "your
# token is wrong" rather than "you did not substitute the token". Those send
# you to completely different places.
case "$TOKEN" in
  *...*|*'<'*|*'>'*|*YOUR*|*your_token*|*REPLACE*)
    echo "✗ That is the PLACEHOLDER, not a token." >&2
    echo "  It still contains the literal text from the example command." >&2
    echo "  Run $0 with no arguments and paste the real one at the prompt." >&2
    exit 2 ;;
esac

# A fine-grained PAT is ~93 characters. Anything much shorter is a truncated
# paste, which also 401s and also looks like a bad token.
if [ "${#TOKEN}" -lt 40 ]; then
  echo "✗ That token is only ${#TOKEN} characters — a fine-grained PAT is around 93." >&2
  echo "  Most likely the paste was truncated." >&2
  exit 2
fi

case "$TOKEN" in
  ghp_*)
    echo "✗ That is a CLASSIC token." >&2
    echo "  It would work -- and that is the problem: it carries push access to" >&2
    echo "  every repository you can reach, for functions that need two scopes" >&2
    echo "  on one repo. Use a FINE-GRAINED PAT with Contents: read and write" >&2
    echo "  and Pull requests: read and write." >&2
    exit 1 ;;
  github_pat_*) ;;
  ghs_*)
    echo "✗ That is a GitHub App INSTALLATION token (ghs_)." >&2
    echo "  Those are short-lived and minted per installation; this script wants a" >&2
    echo "  fine-grained PAT (github_pat_)." >&2
    exit 1 ;;
  gho_*|ghu_*)
    echo "✗ That is an OAuth token, not a personal access token." >&2
    exit 1 ;;
  *)
    # The PREFIX is a type marker, not secret material -- printing the part
    # before the first underscore says what kind of thing arrived without
    # revealing any of it. "Unrecognised prefix" on its own is a dead end.
    SEEN=$(printf '%s' "$TOKEN" | cut -d_ -f1)
    echo "✗ Unrecognised token prefix." >&2
    echo "  Saw something beginning '${SEEN}' of ${#TOKEN} characters; expected github_pat_." >&2
    echo "  If you pasted a fine-grained PAT and see this, the paste picked up stray" >&2
    echo "  characters — try: pbpaste | $0 --stdin" >&2
    exit 1 ;;
esac

OWNER="${GITHUB_OWNER:-vamsiatluri}"
REPO="${GITHUB_REPO:-releasebot-demo}"

# ── Verify the token can do the job BEFORE deploying it ─────────────────────
# A token that deploys cleanly and fails at the first real release is worse
# than one that is rejected here: the failure then happens to somebody else,
# under time pressure, and looks like the bot is broken.
echo "checking the token against $OWNER/${REPO}..."

code() { # code <method> <path> [body]
  if [ -n "${3:-}" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$1" \
      -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' \
      -H 'X-GitHub-Api-Version: 2022-11-28' -d "$3" \
      "https://api.github.com/repos/$OWNER/$REPO$2"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$1" \
      -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' \
      -H 'X-GitHub-Api-Version: 2022-11-28' \
      "https://api.github.com/repos/$OWNER/$REPO$2"
  fi
}

R=$(code GET /git/ref/heads/main)
if [ "$R" != "200" ]; then
  echo "✗ contents: read FAILED (HTTP $R)" >&2
  case "$R" in
    401) echo "  401 means GitHub does not recognise the token at all — expired, revoked," >&2
         echo "  or not actually pasted. It is NOT a permissions problem." >&2 ;;
    403) echo "  403 means the token is valid but lacks Contents access to $OWNER/${REPO}." >&2 ;;
    404) echo "  404 usually means the token is valid but not granted access to THIS repository." >&2 ;;
  esac
  exit 1
fi
echo "  ✓ contents: read"

R=$(code GET "/pulls?state=open&per_page=1")
[ "$R" = "200" ] || { echo "✗ pull requests: read FAILED (HTTP $R)" >&2; exit 1; }
echo "  ✓ pull requests: read"

# ⚠️ WRITE is proven by WRITING -- a permission you have not exercised is a
# permission you are guessing at. Creates a throwaway ref, then deletes it.
SHA=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' \
       "https://api.github.com/repos/$OWNER/$REPO/git/ref/heads/main" \
     | sed -n 's/.*"sha": *"\([^"]*\)".*/\1/p' | head -1)
PROBE="releasebot-permission-probe-$$"
R=$(code POST /git/refs "{\"ref\":\"refs/heads/$PROBE\",\"sha\":\"$SHA\"}")
if [ "$R" = "201" ]; then
  echo "  ✓ contents: write (proved by creating a ref, now removing it)"
  curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$OWNER/$REPO/git/refs/heads/$PROBE"
else
  echo "✗ contents: WRITE failed (HTTP $R) — grant Contents: Read and write" >&2
  exit 1
fi

# ⚠️ Pull-request WRITE is deliberately NOT probed by opening a PR: a stray
# pull request is visible to everyone and someone has to close it. GitHub
# rejects a PR whose head equals its base with 422 -- which only a token that
# IS allowed to open PRs ever gets. A 403 here means the permission is missing.
R=$(code POST /pulls "{\"title\":\"probe\",\"head\":\"main\",\"base\":\"main\"}")
case "$R" in
  422) echo "  ✓ pull requests: write (422 = permitted, request rejected on content)" ;;
  403|404) echo "✗ pull requests: WRITE missing (HTTP $R) — grant Pull requests: Read and write" >&2; exit 1 ;;
  201) echo "⚠ a pull request was actually created — close it manually" >&2 ;;
  *) echo "⚠ unexpected HTTP $R probing pull-request write; continuing" >&2 ;;
esac

# ── Deploy it ───────────────────────────────────────────────────────────────
# ⚠️ Via the ENVIRONMENT, never argv: an argument is visible in `ps` to every
# user on the machine and lands in shell history.
echo "applying to the four functions..."
RB_WRITE="$TOKEN" python3 scripts/_update-stack.py releasebot-lambda infra/20-lambda.yaml \
  GitHubToken=@RB_WRITE

# ── Publish, so the LIVE aliases actually carry it ──────────────────────────
# A published version freezes configuration. Changing the parameter updates
# $LATEST only; without this the aliases keep serving the OLD token and the
# change looks like it did nothing.
for FN in cutRelease releaseAutomationMergeback cutReleaseTest releaseAutomationMergebackTest; do
  aws lambda wait function-updated-v2 --function-name "$FN" --region "$REGION"
  CUR=$(aws lambda get-alias --function-name "$FN" --name live --region "$REGION" \
          --query FunctionVersion --output text)
  SHA=$(aws lambda get-function-configuration --function-name "$FN:$CUR" --region "$REGION" \
          --query Description --output text | sed 's/releasebot //')
  V=$(aws lambda publish-version --function-name "$FN" --region "$REGION" \
        --description "releasebot $SHA" --query Version --output text)
  aws lambda update-alias --function-name "$FN" --name live --region "$REGION" \
    --function-version "$V" >/dev/null
  echo "  $FN v$CUR -> v$V"
done

# ── Prove the deployed functions hold the new token ─────────────────────────
for FN in cutRelease cutReleaseTest; do
  P=$(aws lambda get-function-configuration --function-name "$FN" --region "$REGION" \
        --query 'Environment.Variables.GITHUB_TOKEN' --output text | cut -c1-11)
  [ "$P" = "github_pat_" ] \
    && echo "  ✓ $FN now holds a fine-grained token" \
    || { echo "✗ $FN still holds something else" >&2; exit 1; }
done
echo "✓ done — slackApprove keeps its classic PAT, which is the only thing that can approve a deployment"
