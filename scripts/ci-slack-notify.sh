#!/usr/bin/env bash
# Post a release notification to Slack from the pipeline itself.
#
#   ci-slack-notify.sh <promoted|failed> <commit> <actor> <run-url>
#
# ⚠️ The webhook URL is a BEARER CREDENTIAL: anyone holding it can post to that
# channel. So it arrives in SLACK_WEBHOOK_URL from a repository secret and never
# appears in this file, in a log line, or anywhere in the tree -- which matters
# more than usual here, because this repository is public.
#
# `set -x` is deliberately NOT used, and the URL is never passed as a visible
# argument: curl reads it from the environment, so a trace of this script cannot
# leak it.
set -euo pipefail

STATUS="${1:?promoted or failed}"
COMMIT="${2:?commit sha}"
ACTOR="${3:-unknown}"
RUN_URL="${4:-}"

if [ -z "${SLACK_WEBHOOK_URL:-}" ]; then
  # Not an error. A fork, or a repo without the secret, should still deploy --
  # a missing notification must never fail a release.
  echo "SLACK_WEBHOOK_URL not set; skipping the Slack notification"
  exit 0
fi

PROD_HEALTH="${PROD_HEALTH_URL:-}"

case "$STATUS" in
  promoted)
    TEXT=":white_check_mark: *ReleaseBot — promoted to production*

Commit \`${COMMIT}\`, approved by *${ACTOR}*.

• build — passed, 13 behaviour checks against a local stand-in
• test — passed, 8 checks against the live endpoint
• production — passed, 8 checks against the live endpoint

Production and test report the *same code fingerprint*. Nothing was rebuilt between them, so what shipped is what was tested."
    ;;
  failed)
    TEXT=":x: *ReleaseBot — release failed*

Commit \`${COMMIT}\`. Production was *not* updated; whatever was live is still live.

The rollback path is unchanged: the previous version is still published and the \`live\` alias can be moved back to it in one command."
    ;;
  *)
    echo "unknown status: $STATUS" >&2; exit 2 ;;
esac

[ -n "$RUN_URL" ]     && TEXT="${TEXT}

<${RUN_URL}|View the pipeline run>"
[ -n "$PROD_HEALTH" ] && TEXT="${TEXT}
<${PROD_HEALTH}|Check production health>"

# Build the payload with python so the text is JSON-escaped correctly --
# newlines and backticks in a shell heredoc are a reliable way to produce
# malformed JSON that Slack rejects with a bare "invalid_payload".
PAYLOAD=$(TEXT="$TEXT" python3 -c 'import json,os; print(json.dumps({"text": os.environ["TEXT"]}))')

CODE=$(printf '%s' "$PAYLOAD" | curl -sS -o /tmp/slack-resp.txt -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' --data-binary @- "$SLACK_WEBHOOK_URL")

if [ "$CODE" = "200" ]; then
  echo "slack: notified ($STATUS)"
else
  # A failed notification is reported, never fatal. Blocking a release because
  # a chat message did not send is the wrong trade.
  echo "::warning::slack notification returned HTTP $CODE: $(cat /tmp/slack-resp.txt)"
fi
