#!/usr/bin/env bash
# Prove the Slack slash-command path end to end, with a REAL signature.
#
#   bash scripts/prove-slash.sh [test|prod]
#
# ⚠️ Defaults to the TEST stage, whose functions run RELEASEBOT_DRY_RUN=1 --
# so this exercises signature verification and the whole handler without
# creating a branch in anyone's repository. Pass prod only when you intend a
# real release.
#
# ⛔ The secret is read from Parameter Store into a variable in THIS script and
# never printed, never passed as an argument, and never written to a file that
# outlives the run. The signature is computed locally; the secret does not
# travel over the wire.
#
# ★ WHY A POSITIVE *AND* A NEGATIVE CONTROL. A 200 alone does not prove the
# signature was checked -- it could mean verification is disabled. The pair
# does: a correct signature must be accepted and a wrong one must be rejected.
# Without the negative control this script would pass against a function that
# verifies nothing.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGE="${1:-test}"
export AWS_PAGER=""
REGION="${AWS_REGION:-us-east-1}"

API=$(aws apigateway get-rest-apis --query "items[?name=='releasebot'].id | [0]" --output text) || exit 1
[ -n "$API" ] && [ "$API" != "None" ] || { echo "no 'releasebot' API in this account" >&2; exit 1; }
BASE="https://$API.execute-api.$REGION.amazonaws.com/$STAGE"

SECRET=$(aws ssm get-parameter --name "/releasebot/$STAGE/secrets/slack-signing-secret" \
          --with-decryption --query 'Parameter.Value' --output text 2>/dev/null)
[ -n "$SECRET" ] && [ "$SECRET" != "None" ] || {
  echo "could not read /releasebot/$STAGE/secrets/slack-signing-secret" >&2; exit 1; }

# A real Slack slash-command body: form-encoded, exactly these fields.
TS=$(date +%s)
# ⚠️ The argument order is `<repo> <version>`, not the reverse. Getting it
# backwards still returns 200 -- the signature verified and the handler then
# refused the arguments -- which proves verification but NOT the command path,
# and reads as a pass if you only check the status code.
BODY="token=unused&team_id=T0DEMO&channel_id=C0RELEASE&user_id=${PROVE_USER:-U0DEMO}&user_name=demo&command=%2Fcut&text=${PROVE_REPO:-releasebot-demo}+${PROVE_VERSION:-4.99.0}&response_url=https%3A%2F%2Fexample.invalid%2Fnope"

sign() { # $1=timestamp $2=body  -> v0=<hex>
  printf 'v0:%s:%s' "$1" "$2" \
    | openssl dgst -sha256 -hmac "$SECRET" -hex \
    | awk '{print "v0="$NF}'
}

call() { # $1=signature $2=timestamp -> prints "HTTP <code>|<body>"
  curl -sS -o /tmp/rb-slash.$$ -w '%{http_code}' --max-time 20 \
    -X POST "$BASE/beta/cut" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    -H "X-Slack-Request-Timestamp: $2" \
    -H "X-Slack-Signature: $1" \
    --data-raw "$BODY"
}

echo "stage   $STAGE"
echo "url     $BASE/beta/cut"
echo

FAIL=0

# ── 1. POSITIVE: a correct signature must be ACCEPTED ───────────────────────
CODE=$(call "$(sign "$TS" "$BODY")" "$TS")
RESP=$(cat /tmp/rb-slash.$$ 2>/dev/null); rm -f /tmp/rb-slash.$$
# ⛔ ASSERT THE SUCCESS SIGNAL, do not enumerate failures.
# This check used to list refusal phrases ("not a valid", "unauthorized",
# "error") and pass anything else. The moment a NEW refusal was added -- "You
# are not permitted to cut releases" -- the prover printed "command accepted"
# for a request that had been refused. A denylist of failures cannot anticipate
# the next failure; the acknowledgement is a single known string, so match THAT.
if [ "$CODE" = "200" ] && printf '%s' "$RESP" | grep -q "hourglass_flowing_sand"; then
  echo "✓ correctly signed  -> HTTP 200, command accepted"
  echo "    $(printf '%s' "$RESP" | head -c 300)"
elif [ "$CODE" = "200" ]; then
  # ⛔ 200 with a refusal in the body is NOT a pass. The signature verified and
  # the handler then rejected the arguments -- checking only the status code
  # would report this as success and prove half of what it claims.
  echo "✗ correctly signed  -> HTTP 200 but the command was REFUSED (no acknowledgement in the body)"
  echo "    $(printf '%s' "$RESP" | head -c 300)"
  FAIL=1
else
  echo "✗ correctly signed  -> HTTP $CODE  (expected 200)"
  echo "    $(printf '%s' "$RESP" | head -c 220)"
  echo "    ⚠️ 401 here with the right secret in Parameter Store means the"
  echo "       function is still serving a WARM environment that resolved the"
  echo "       OLD value at INIT. Rotation takes effect on the next COLD START."
  FAIL=1
fi
echo

# ── 2. NEGATIVE: a wrong signature must be REJECTED ─────────────────────────
CODE=$(call "v0=$(printf '0%.0s' $(seq 64))" "$TS")
rm -f /tmp/rb-slash.$$
if [ "$CODE" = "401" ]; then
  echo "✓ wrong signature   -> HTTP 401"
else
  echo "✗ wrong signature   -> HTTP $CODE  (expected 401 — verification may be OFF)"
  FAIL=1
fi

# ── 3. NEGATIVE: a stale timestamp must be REJECTED even if correctly signed ─
OLD=$((TS - 3600))
CODE=$(call "$(sign "$OLD" "$BODY")" "$OLD")
rm -f /tmp/rb-slash.$$
if [ "$CODE" = "401" ]; then
  echo "✓ valid sig, 1h old -> HTTP 401 (replay window enforced)"
else
  echo "✗ valid sig, 1h old -> HTTP $CODE (expected 401 — a captured request could be replayed)"
  FAIL=1
fi

echo
[ "$FAIL" = 0 ] && echo "SLASH COMMAND PROVEN on /$STAGE" || { echo "NOT PROVEN"; exit 1; }
