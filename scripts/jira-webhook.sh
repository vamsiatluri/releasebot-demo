#!/usr/bin/env bash
# Replay a Jira "version released" webhook at ReleaseBot.
#   ./scripts/jira-webhook.sh NEWS-412 5.4.0
set -euo pipefail
KEY="${1:?usage: jira-webhook.sh <ISSUE-KEY> <fixVersion>}"
VERSION="${2:?}"
BASE="${BASE:-http://localhost:8080/beta}"
SECRET="${JIRA_WEBHOOK_SECRET:-local-dev-jira-secret}"

curl -sS -X POST "$BASE/cut" \
  -H 'Content-Type: application/json' \
  -H "X-Releasebot-Jira-Secret: $SECRET" \
  -w '\n-> HTTP %{http_code}\n' \
  -d "{\"webhookEvent\":\"jira:issue_updated\",\"user\":{\"displayName\":\"Release Manager\"},
       \"issue\":{\"key\":\"$KEY\",\"fields\":{\"summary\":\"Cut $VERSION\",
       \"project\":{\"key\":\"NEWS-APP\"},\"fixVersions\":[{\"name\":\"$VERSION\"}]}}}"
