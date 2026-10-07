#!/usr/bin/env bash
# Send a correctly-signed Slack slash command at a running ReleaseBot.
#
#   ./scripts/slash.sh cut   news-app 5.4.0
#   ./scripts/slash.sh merge news-app 5.4.0
#   BAD_SIG=1 ./scripts/slash.sh cut news-app 5.4.0   # prove the 401 path
#   SKEW=600  ./scripts/slash.sh cut news-app 5.4.0   # prove the replay window
set -euo pipefail

ROUTE="${1:?usage: slash.sh <cut|merge> <repo> <version>}"
REPO="${2:?}"
VERSION="${3:?}"

BASE="${BASE:-http://localhost:8080/beta}"
SECRET="${SLACK_SIGNING_SECRET:-local-dev-signing-secret}"
TS=$(( $(date +%s) - ${SKEW:-0} ))

BODY="token=local&team_id=T000&channel_id=C0RELEASE&user_name=${USER_NAME:-vamsi}"
BODY="${BODY}&command=%2F${ROUTE}&text=$(printf '%s %s' "$REPO" "$VERSION" | sed 's/ /+/g')"
BODY="${BODY}&response_url=http%3A%2F%2Flocalhost%3A9099%2F_response_url"

SIG="v0=$(printf 'v0:%s:%s' "$TS" "$BODY" \
  | openssl dgst -sha256 -hmac "$SECRET" -r | cut -d' ' -f1)"
[ -n "${BAD_SIG:-}" ] && SIG="v0=0000000000000000000000000000000000000000000000000000000000000000"

curl -sS -X POST "$BASE/$ROUTE" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -H "X-Slack-Request-Timestamp: $TS" \
  -H "X-Slack-Signature: $SIG" \
  -w '\n-> HTTP %{http_code}\n' \
  --data-raw "$BODY"
