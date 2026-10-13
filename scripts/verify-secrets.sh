#!/usr/bin/env bash
# Prove every secret can do the thing it exists for.
#
#   bash scripts/verify-secrets.sh [prod|test]
#
# ⛔ WHY THIS EXISTS, and it is the most expensive lesson in this repository.
#
# Three times in one day a secret or artifact was "verified" by checking that a
# COPY matched its SOURCE, which says nothing about whether the source was
# right:
#
#   - a content-addressed digest matched the previous one -- correctly, because
#     the bytes really were the old bytes. The deployed function ran code from
#     before the secret resolver existed.
#   - a restored signing secret "matched version 1 exactly". It did. Version 1
#     held a value Slack does not sign with, so every click returned 401.
#   - a restored approval token matched version 1 too. Version 1 held a
#     FINE-GRAINED token, which can never approve a deployment, so the click
#     returned 403 -- a message that reads like a missing permission and is not.
#
# ★ The common error: VERIFYING THE TRANSFER INSTEAD OF THE CAPABILITY. A hash
# comparison proves a copy. Only exercising the credential proves the value.
#
# So every check below calls the API the secret is FOR, and a check that cannot
# be performed is reported as UNPROVEN -- never as passing.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV="${1:-prod}"
export AWS_PAGER=""
REGION="${AWS_REGION:-us-east-1}"
OWNER="${GITHUB_OWNER:-vamsiatluri}"
REPO="${GITHUB_REPO:-releasebot-demo}"

ok()      { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad()     { printf '  \033[31m✗\033[0m %s\n' "$*"; FAIL=1; }
unproven(){ printf '  \033[33m?\033[0m %s\n' "$*"; UNPROVEN=1; }
FAIL=0; UNPROVEN=0

read_secret() { # $1 = short name; prints the value, empty if absent
  aws ssm get-parameter --name "/releasebot/$ENV/secrets/$1" --with-decryption \
      --region "$REGION" --query 'Parameter.Value' --output text 2>/dev/null
}

printf '\nsecrets for /releasebot/%s/secrets/\n\n' "$ENV"

# ── 1. the approval token ───────────────────────────────────────────────────
# ⛔ The ONLY check that matters here is "is it a classic token with repo".
# Reading the repo proves nothing: a fine-grained token reads a PUBLIC repo
# with no permissions at all, so "it can read the repo" is exactly the check
# that let the wrong credential through.
T=$(read_secret github-approval-token)
if [ -z "$T" ] || [ "$T" = "None" ]; then
  unproven "github-approval-token: not set"
else
  case "$T" in
    github_pat_*) bad "github-approval-token is FINE-GRAINED — can never approve a deployment, with any permission set" ;;
    *)
      H=$(mktemp); curl -sS -D "$H" -o /dev/null -H "Authorization: Bearer $T" \
            https://api.github.com/user 2>/dev/null
      CODE=$(awk 'NR==1{print $2}' "$H")
      SC=$(grep -i '^x-oauth-scopes:' "$H" | cut -d: -f2- | tr -d '\r' | sed 's/^ *//')
      rm -f "$H"
      if [ "$CODE" != "200" ]; then
        bad "github-approval-token: GitHub returned HTTP $CODE (expired or revoked?)"
      elif [ -z "$SC" ]; then
        # ⚠️ The decisive test. A classic token ALWAYS reports its scopes; an
        # empty header means it is not classic whatever its prefix says.
        bad "github-approval-token reports NO scopes — it is not a classic token, and the approve endpoint will 403"
      else
        case "$SC" in
          *repo*) ok "github-approval-token: classic, scopes [$SC]" ;;
          *) bad "github-approval-token lacks 'repo' (has [$SC]); a public repo still needs repo, not public_repo" ;;
        esac
      fi ;;
  esac
fi

# ── 2. the write token ──────────────────────────────────────────────────────
# ⚠️ Proven BY WRITING. A fine-grained token reads a public repo without the
# Contents permission, so a successful read says nothing about write.
W=$(read_secret github-token)
if [ -z "$W" ] || [ "$W" = "None" ]; then
  unproven "github-token: not set"
else
  REF="refs/heads/releasebot-writecheck-$$"
  SHA=$(curl -sS -H "Authorization: Bearer $W" \
         "https://api.github.com/repos/$OWNER/$REPO/git/ref/heads/main" 2>/dev/null \
        | python3 -c 'import sys,json;print(json.load(sys.stdin).get("object",{}).get("sha",""))' 2>/dev/null)
  if [ -z "$SHA" ]; then
    bad "github-token cannot read $OWNER/$REPO@main"
  else
    C=$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
         -H "Authorization: Bearer $W" -H 'Accept: application/vnd.github+json' \
         "https://api.github.com/repos/$OWNER/$REPO/git/refs" \
         -d "{\"ref\":\"$REF\",\"sha\":\"$SHA\"}" 2>/dev/null)
    if [ "$C" = "201" ]; then
      curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $W" \
        "https://api.github.com/repos/$OWNER/$REPO/git/$REF" 2>/dev/null
      ok "github-token: contents WRITE proven (created and deleted a throwaway ref)"
    else
      bad "github-token cannot write (HTTP $C) — grant Contents: Read and write"
    fi
  fi
fi

# ── 3. the signing secret, proven against the RUNNING function ─────────────
# ⚠️ Not "is it 32 hex characters" -- the wrong 32 hex characters look
# identical. The only proof is a request the deployed function accepts.
S=$(read_secret slack-signing-secret)
API=$(aws apigateway get-rest-apis --region "$REGION" \
        --query "items[?name=='releasebot'].id | [0]" --output text 2>/dev/null)
if [ -z "$S" ] || [ "$S" = "None" ]; then
  unproven "slack-signing-secret: not set"
elif [ -z "$API" ] || [ "$API" = "None" ]; then
  unproven "slack-signing-secret: no 'releasebot' API in this account to test against"
else
  TS=$(date +%s)
  BODY='token=unused&command=%2Fcut&text=&user_id=U0VERIFY&user_name=verify'
  SIG=$(printf 'v0:%s:%s' "$TS" "$BODY" \
        | openssl dgst -sha256 -hmac "$S" -hex | awk '{print "v0="$NF}')
  C=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 -X POST \
       "https://$API.execute-api.$REGION.amazonaws.com/$ENV/beta/cut" \
       -H 'Content-Type: application/x-www-form-urlencoded' \
       -H "X-Slack-Request-Timestamp: $TS" -H "X-Slack-Signature: $SIG" \
       --data-raw "$BODY" 2>/dev/null)
  # 200 = the signature was ACCEPTED (the empty command is then refused in the
  # body, which is fine -- we are testing the secret, not the arguments).
  case "$C" in
    200) ok "slack-signing-secret: the deployed /$ENV function ACCEPTS a signature made with it" ;;
    401) bad "slack-signing-secret: the deployed /$ENV function REJECTS a signature made with it — the stored value is not what the function resolves, or the environment is warm with an older one" ;;
    *)   unproven "slack-signing-secret: /$ENV returned HTTP $C, cannot conclude" ;;
  esac
fi

printf '\n'
if [ "$FAIL" = 1 ]; then
  printf '\033[31mat least one secret cannot do its job\033[0m\n'; exit 1
elif [ "$UNPROVEN" = 1 ]; then
  printf '\033[33msome secrets could not be proven — unproven is NOT passing\033[0m\n'; exit 2
fi
printf '\033[32mevery secret proven by exercising it\033[0m\n'
