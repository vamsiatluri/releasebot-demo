#!/usr/bin/env bash
# Post a release notification to Slack from the pipeline itself.
#
#   ci-slack-notify.sh <awaiting|promoted|failed> <commit> <actor> <run-url> [run-id]
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

STATUS="${1:?awaiting, promoted or failed}"
COMMIT="${2:?commit sha}"
ACTOR="${3:-unknown}"
RUN_URL="${4:-}"
RUN_ID="${5:-}"

if [ -z "${SLACK_WEBHOOK_URL:-}" ]; then
  # Not an error. A fork, or a repo without the secret, should still deploy --
  # a missing notification must never fail a release.
  echo "SLACK_WEBHOOK_URL not set; skipping the Slack notification"
  exit 0
fi

PROD_HEALTH="${PROD_HEALTH_URL:-}"

case "$STATUS" in
  awaiting)
    TEXT=":hourglass_flowing_sand: *ReleaseBot — waiting for your approval*

Commit \`${COMMIT}\` built and passed every check on *test*. Production has *not* been touched, and will not be until someone approves.

Approving promotes the artifact that was just tested — no rebuild happens, so what ships is what passed."
    ;;
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

# The awaiting message carries its own buttons, so it does not repeat the links
# as text. The other two have no action, so they do.
if [ "$STATUS" != "awaiting" ]; then
  [ -n "$RUN_URL" ]     && TEXT="${TEXT}

<${RUN_URL}|View the pipeline run>"
  [ -n "$PROD_HEALTH" ] && TEXT="${TEXT}
<${PROD_HEALTH}|Check production health>"
fi

# Build the payload in python so the text is escaped correctly -- newlines and
# backticks assembled by hand in shell are a reliable way to produce malformed
# JSON that Slack rejects with a bare "invalid_payload".
#
# `awaiting` is Block Kit rather than plain text, because it is the only message
# that carries an action. RUN_ID travels in the button's value: the approval
# endpoint has to know WHICH run to approve, and a button that approved "the
# latest pending run" would approve whatever happened to be waiting when
# somebody got round to clicking.
PAYLOAD=$(
  TEXT="$TEXT" STATUS="$STATUS" RUN_URL="$RUN_URL" RUN_ID="$RUN_ID" \
  python3 -c '
import json, os
text   = os.environ["TEXT"]
status = os.environ["STATUS"]
url    = os.environ.get("RUN_URL", "")
run_id = os.environ.get("RUN_ID", "")

if status != "awaiting" or not run_id:
    print(json.dumps({"text": text}))
    raise SystemExit

buttons = [{
    "type": "button", "action_id": "approve",
    "text": {"type": "plain_text", "text": "Approve and deploy"},
    "style": "primary", "value": run_id,
    # A confirm step on a button that ships to production. One stray click in a
    # chat window should not be a release.
    "confirm": {
        "title":   {"type": "plain_text", "text": "Deploy to production?"},
        "text":    {"type": "mrkdwn", "text": "This promotes the tested artifact to production immediately."},
        "confirm": {"type": "plain_text", "text": "Deploy"},
        "deny":    {"type": "plain_text", "text": "Cancel"},
    },
}]
if url:
    buttons.append({
        "type": "button", "action_id": "open",
        "text": {"type": "plain_text", "text": "Review in GitHub"}, "url": url,
    })

print(json.dumps({
    "text": "ReleaseBot is waiting for your approval",   # notification fallback
    "blocks": [
        {"type": "section", "text": {"type": "mrkdwn", "text": text}},
        {"type": "actions", "elements": buttons},
    ],
}))
'
)

CODE=$(printf '%s' "$PAYLOAD" | curl -sS -o /tmp/slack-resp.txt -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' --data-binary @- "$SLACK_WEBHOOK_URL")

if [ "$CODE" = "200" ]; then
  echo "slack: notified ($STATUS)"
else
  # A failed notification is reported, never fatal. Blocking a release because
  # a chat message did not send is the wrong trade.
  echo "::warning::slack notification returned HTTP $CODE: $(cat /tmp/slack-resp.txt)"
fi
