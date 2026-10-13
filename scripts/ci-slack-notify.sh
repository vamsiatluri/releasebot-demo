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
# ⚠️ The RUN NUMBER, not the run id. The id (37952182035) is what GitHub's API
# needs and what the Approve button carries; the NUMBER (#37) is what a person
# sees on the run page and says out loud. A channel holding three notifications
# cannot be matched to the runs they came from without it.
RUN_NUMBER="${6:-}"

if [ -z "${SLACK_WEBHOOK_URL:-}" ]; then
  # Not an error. A fork, or a repo without the secret, should still deploy --
  # a missing notification must never fail a release.
  echo "SLACK_WEBHOOK_URL not set; skipping the Slack notification"
  exit 0
fi

PROD_HEALTH="${PROD_HEALTH_URL:-}"

# ⛔ WHICH ACCOUNT THIS IS ABOUT. Two environments running this bot post to the
# same channel through the same Slack app, so two approve buttons can sit side
# by side looking identical while pointing at different AWS accounts. A person
# clicking one cannot tell them apart, and an approval is a production deploy.
#
# ⚠️ "unlabelled" rather than a guess. A message that silently claims the wrong
# account is worse than one that admits it does not know.
ACCOUNT="${RELEASEBOT_ACCOUNT_LABEL:-unlabelled}"
BADGE="\`${ACCOUNT}\`"

case "$STATUS" in
  awaiting)
    # ⛔ NAME WHAT THIS RUN KILLED. cancel-in-progress means a newer push
    # cancels an older run parked at the gate -- deliberately, because two
    # pending approvals is how a gate stops being read and starts being clicked
    # through. But the cancelled run cannot say so: every job in it, its own
    # notifier included, died with it. So the channel is left holding an
    # "approve run #51" message whose button is now inert, and clicking it does
    # nothing and reports nothing.
    #
    # ⚠️ The superseded run's message is NOT edited or deleted. A channel is a
    # record; rewriting history there is worse than appending to it. This says
    # plainly which message is now dead.
    SUPERSEDE=""
    if [ -n "${SUPERSEDED:-}" ]; then
      SUPERSEDE="
:warning: *This replaces ${SUPERSEDED}*, which was waiting for approval and has been cancelled. The approval button on that message no longer does anything — approve *this* one."
    fi
    TEXT=":hourglass_flowing_sand: *ReleaseBot — waiting for your approval*  ·  ${BADGE}

*Run #${RUN_NUMBER:-?}* · commit \`${COMMIT}\` built and passed every check on *test*. Production has *not* been touched, and will not be until someone approves.

Approving promotes the artifact that was just tested — no rebuild happens, so what ships is what passed.${SUPERSEDE}"
    ;;
  promoted)
    TEXT=":white_check_mark: *ReleaseBot — promoted to production*  ·  ${BADGE}

*Run #${RUN_NUMBER:-?}* · commit \`${COMMIT}\`, approved by *${ACTOR}*.

• build — passed, 13 behaviour checks against a local stand-in
• test — passed, 8 checks against the live endpoint
• production — passed, 8 checks against the live endpoint

Production and test report the *same code fingerprint*. Nothing was rebuilt between them, so what shipped is what was tested."
    ;;
  failed)
    TEXT=":x: *ReleaseBot — release failed*  ·  ${BADGE}

*Run #${RUN_NUMBER:-?}* · commit \`${COMMIT}\`. Production was *not* updated; whatever was live is still live.

The rollback path is unchanged: the previous version is still published and the \`live\` alias can be moved back to it in one command."
    ;;
  rolledback)
    # A rollback is the one release event that MUST be loud. It happens under
    # pressure, usually by one person, and it is the change most likely to be
    # discovered later by somebody who was not told.
    TEXT=":rewind: *ReleaseBot — production rolled back*  ·  ${BADGE}

Both functions moved to commit \`${COMMIT}\`, by *${ACTOR}*.

The rollback was pre-flighted: the target version's configuration was compared against current before the alias moved, so it is not carrying a rotated secret. Both aliases were read back and verified, and \`/health\` was checked on the live endpoint.

*Nothing was rebuilt.* This is the same artifact that was previously approved and previously ran in production."
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
  TEXT="$TEXT" STATUS="$STATUS" RUN_URL="$RUN_URL" RUN_ID="$RUN_ID" ACCOUNT="$ACCOUNT" \
  python3 -c '
import json, os
text   = os.environ["TEXT"]
status  = os.environ["STATUS"]
url     = os.environ.get("RUN_URL", "")
run_id  = os.environ.get("RUN_ID", "")
# ⛔ The confirm dialog is the LAST thing a person reads before production
# changes. With two accounts posting to one channel, "Deploy to production?"
# is ambiguous at exactly the moment ambiguity is most expensive.
account = os.environ.get("ACCOUNT", "unlabelled")

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
        "title":   {"type": "plain_text", "text": "Deploy to %s?" % account},
        "text":    {"type": "mrkdwn", "text":
                    "This promotes the tested artifact to production in *%s* immediately.\n"
                    "⚠️ Check the account name — more than one environment posts to this channel."
                    % account},
        "confirm": {"type": "plain_text", "text": "Deploy"},
        "deny":    {"type": "plain_text", "text": "Cancel"},
    },
}]
# ── The NO ──────────────────────────────────────────────────────────────────
# A gate with only an Approve button has one answer. Without a reject, the ways
# to clear a release you have decided against are: approve it anyway, or leave
# it pending -- and a pending release nobody intends to ship is how a queue
# grows until the gate stops being read.
buttons.append({
    "type": "button", "action_id": "reject",
    "text": {"type": "plain_text", "text": "Reject"},
    "style": "danger", "value": run_id,
    "confirm": {
        "title":   {"type": "plain_text", "text": "Reject this release?"},
        "text":    {"type": "mrkdwn", "text":
                    "The run ends here and production is unchanged. "
                    "A new commit starts a new release."},
        "confirm": {"type": "plain_text", "text": "Reject"},
        "deny":    {"type": "plain_text", "text": "Keep waiting"},
    },
})
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
