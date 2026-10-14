#!/usr/bin/env bash
# Give the dashboard a ZERO-PERMISSION GitHub token, and prove it is one.
#
#   bash scripts/set-dashboard-token.sh
#
# ⛔ WHY A TOKEN AT ALL, having deliberately removed one.
#
# Earlier this page used the APPROVAL token -- a classic PAT with `repo`, i.e.
# push access to every repository its owner can reach -- held by a page that
# only ever reads. That was removed, and conditional requests (If-None-Match)
# made an idle dashboard cost nothing, because a 304 does not count against
# GitHub's rate limit.
#
# What that did NOT fix: a CHANGED response still costs a request, anonymous
# reads are **60 per hour PER EGRESS IP**, and a Lambda's egress is a SHARED
# NAT address. So an ordinary busy afternoon -- a few deploys and someone
# refreshing a dashboard -- exhausts it, and the panel goes stale mid-demo.
#
# A token raises that to 5,000/hour. Everything this page reads from GitHub is
# PUBLIC, so the token needs no permissions whatsoever: the ONLY thing it buys
# is headroom.
#
# ⛔ WHICH IS WHY THIS SCRIPT REFUSES ANYTHING THAT CAN ACT. A token with any
# permission would hand a read-only status page the ability to change things,
# for zero gain. The checks below are capability tests, not shape tests -- the
# lesson of every other secret in this repository.
#
# ⚠️ A token's permissions can be WIDENED LATER, and nothing would re-check. So
# verify-secrets.sh re-tests this one every time it runs.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export AWS_PAGER=""
REGION="${AWS_REGION:-us-east-1}"
OWNER="${GITHUB_OWNER:-vamsiatluri}"
REPO="${GITHUB_REPO:-releasebot-demo}"
NAME="/releasebot/prod/secrets/dashboard-github-token"

echo "Create the token first, if you have not:"
echo "  https://github.com/settings/personal-access-tokens/new"
echo "  · Fine-grained"
echo "  · Repository access: Public Repositories (read-only)"
echo "  · Repository permissions: NONE — do not grant a single one"
echo "  · Expiry: short. This buys rate limit, nothing else."
echo

TOKEN="${RELEASEBOT_DASHBOARD_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  printf 'paste the token (not echoed): ' >&2
  IFS= read -rs TOKEN < /dev/tty || true
  printf '\n' >&2
fi
# Bracketed paste and stray control characters, same as every other token here.
TOKEN=$(printf '%s' "$TOKEN" | sed -e 's/\x1b\[20[01]~//g' | tr -d '\000-\037')
[ -n "$TOKEN" ] || { echo "no token given" >&2; exit 2; }

fail() { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }

# ── 1. It must NOT be classic ──────────────────────────────────────────────
case "$TOKEN" in
  ghp_*) fail "that is a CLASSIC token. A classic token always carries SCOPES — there is no
    zero-permission classic PAT — and the narrowest scope that reads a repo is
    'repo', which is push access to every repository you can reach. Use a
    fine-grained token with no permissions." ;;
  github_pat_*) ;;
  *) echo "⚠ unexpected prefix; continuing, but a fine-grained token starts github_pat_" >&2 ;;
esac

# ── 2. It must authenticate ────────────────────────────────────────────────
H=$(mktemp); B=$(mktemp); trap 'rm -f "$H" "$B"' EXIT
curl -sS -D "$H" -o "$B" -H "Authorization: Bearer $TOKEN" https://api.github.com/user
CODE=$(awk 'NR==1{print $2}' "$H")
[ "$CODE" = "200" ] || fail "GitHub returned HTTP $CODE for this token"
LOGIN=$(python3 -c "import json;print(json.load(open('$B')).get('login','?'))" 2>/dev/null)
SCOPES=$(grep -i '^x-oauth-scopes:' "$H" | cut -d: -f2- | tr -d '\r' | sed 's/^ *//')
ok "authenticates as $LOGIN"

# ⚠️ A NON-EMPTY scopes header means the token is CLASSIC whatever its prefix.
[ -z "$SCOPES" ] || fail "GitHub reports scopes [$SCOPES] — that is a classic token, which is
    exactly what this page must not hold."
ok "no OAuth scopes — fine-grained, as required"

# ── 3. It must actually raise the limit, or it buys nothing ────────────────
LIMIT=$(curl -sS -H "Authorization: Bearer $TOKEN" https://api.github.com/rate_limit 2>/dev/null \
        | python3 -c "import sys,json;print(json.load(sys.stdin)['resources']['core']['limit'])" 2>/dev/null)
case "${LIMIT:-0}" in
  5000) ok "rate limit 5000/hour (up from 60 anonymous) — this is the entire point" ;;
  60)   fail "the limit is still 60 — the token is not being counted, so it buys nothing" ;;
  *)    echo "  ⚠ unexpected rate limit '${LIMIT:-?}'; continuing" ;;
esac

# ── 4. ⛔ IT MUST NOT BE ABLE TO WRITE. A capability test, not a shape test ──
#
# Attempts to create a ref. 201 means the token can write to the repository and
# must be refused -- and the ref is removed immediately, because a refusal
# should not leave litter behind.
SHA=$(curl -sS -H "Authorization: Bearer $TOKEN" \
       "https://api.github.com/repos/$OWNER/$REPO/git/ref/heads/main" 2>/dev/null \
      | python3 -c "import sys,json;print(json.load(sys.stdin).get('object',{}).get('sha',''))" 2>/dev/null)
if [ -n "$SHA" ]; then
  REF="refs/heads/dashboard-token-permcheck-$$"
  W=$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
        -H "Authorization: Bearer $TOKEN" -H 'Accept: application/vnd.github+json' \
        "https://api.github.com/repos/$OWNER/$REPO/git/refs" \
        -d "{\"ref\":\"$REF\",\"sha\":\"$SHA\"}" 2>/dev/null)
  if [ "$W" = "201" ]; then
    curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" \
      "https://api.github.com/repos/$OWNER/$REPO/git/$REF" 2>/dev/null
    fail "this token CAN WRITE to $OWNER/$REPO (it created a ref, now deleted).
    A read-only status page must not hold a credential that can change the
    repository. Re-create it with NO repository permissions."
  fi
  ok "cannot write to $OWNER/$REPO (ref creation refused with HTTP $W)"
else
  echo "  ⚠ could not read main's sha, so the write test did not run — UNPROVEN, not passing" >&2
fi

# ── 5. Store it, and point the dashboard at the reference ──────────────────
RELEASEBOT_SECRET="$TOKEN" bash scripts/set-secret.sh "$NAME" || exit 1
unset TOKEN

echo
echo "next: deploy the dashboard so it reads the reference —"
echo "  RELEASEBOT_DASHBOARD_TOKEN='ssm:$NAME' bash scripts/deploy-dashboard.sh <your-email>"
