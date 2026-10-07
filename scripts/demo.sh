#!/usr/bin/env bash
# End-to-end local proof. Starts the fake GitHub/Slack and both handlers, then
# walks the scenarios the migration's acceptance criteria actually care about.
# No AWS account, no credentials, no cost.
set -uo pipefail
cd "$(dirname "$0")/.."

export GITHUB_API_URL=http://localhost:9099
export SLACK_API_URL=http://localhost:9099
export GITHUB_TOKEN=local-dev-github-token
export SLACK_TOKEN=local-dev-slack-token
export SLACK_SIGNING_SECRET=local-dev-signing-secret
export JIRA_WEBHOOK_SECRET=local-dev-jira-secret
export DEFAULT_OWNER=msnbc
export SLACK_CHANNEL=C0RELEASE
export RARC_ENV=test

cleanup() { kill ${MOCK_PID:-0} ${APP_PID:-0} 2>/dev/null; }
trap cleanup EXIT

go run ./cmd/mockapis >/tmp/releasebot-mocks.log 2>&1 &
MOCK_PID=$!
go run ./cmd/localdev >/tmp/releasebot-app.log 2>&1 &
APP_PID=$!

for _ in $(seq 1 50); do
  curl -sf http://localhost:8080/beta/health >/dev/null 2>&1 && break
  sleep 0.2
done

step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

step "1. health probe (what the deploy workflow calls, cuts nothing)"
curl -sS http://localhost:8080/beta/health; echo

step "2. cut release/5.4.0 in msnbc/news-app"
./scripts/slash.sh cut news-app 5.4.0

step "3. the same cut again — idempotent, must not error"
./scripts/slash.sh cut news-app 5.4.0

step "4. mergeback PR release/5.4.0 -> main"
./scripts/slash.sh merge news-app 5.4.0

step "5. the same mergeback again — reuses the open PR"
./scripts/slash.sh merge news-app 5.4.0

step "6. forged signature — must be 401"
BAD_SIG=1 ./scripts/slash.sh cut news-app 9.9.9

step "7. captured request replayed 10 minutes later — must be 401"
SKEW=600 ./scripts/slash.sh cut news-app 9.9.9

step "8. malformed version — 200 with a usable error, never a Slack 'dispatch_failed'"
./scripts/slash.sh cut news-app 5.4

step "9. mergeback for a branch that was never cut"
./scripts/slash.sh merge news-app 7.7.7

step "10. Jira webhook with a valid shared secret"
./scripts/jira-webhook.sh NEWS-412 6.0.0

step "11. Jira webhook with the wrong secret — must be 401"
JIRA_WEBHOOK_SECRET=wrong ./scripts/jira-webhook.sh NEWS-413 6.0.1

step "12. fake GitHub's final state"
curl -sS http://localhost:9099/_state | python3 -m json.tool 2>/dev/null || curl -sS http://localhost:9099/_state

step "13. structured log lines the way CloudWatch will see them"
grep -E '"level"' /tmp/releasebot-app.log | tail -8
