#!/usr/bin/env bash
# Rotate a secret and make it ACTUALLY TAKE EFFECT.
#
#   bash scripts/rotate-secret.sh /releasebot/prod/secrets/github-token
#
# ⛔ READ THIS BEFORE BELIEVING "ROTATION NEEDS NO DEPLOY".
#
# Holding a REFERENCE in the function's environment instead of a value buys
# three real things:
#   - the secret is not in the function config, the console, or a change set;
#   - an OLDER VERSION is no longer stranded as a rollback target, because it
#     holds a pointer and reads the current value when it next starts;
#   - rotation never rebuilds or re-uploads the artifact.
#
# What it does NOT buy is instant effect. The value is read ONCE, during INIT.
# A warm execution environment keeps serving the OLD secret until it recycles,
# and there is NO API to force a cold start:
#   - `publish-version` returns the EXISTING version when $LATEST is unchanged,
#     so a config-only republish is a no-op;
#   - idle environments are reclaimed on AWS's schedule, not yours.
#
# So the only deterministic path is to change something, publish, and move the
# alias. This script changes a MARKER variable -- never the secret, which stays
# a reference -- so the artifact is untouched and the diff is one timestamp.
#
# ⚠️ Measured, not assumed: before the recycle a correctly-signed request got
# 401 from a warm environment holding the previous secret. That is the whole
# reason this script exists rather than a sentence in a runbook.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAME="${1:-}"
[ -n "$NAME" ] || { echo "usage: $0 /releasebot/<env>/secrets/<name>   [value via RELEASEBOT_SECRET or prompt]" >&2; exit 2; }
case "$NAME" in /releasebot/*/secrets/*) ;; *) echo "refusing: expected /releasebot/<env>/secrets/<name>" >&2; exit 2 ;; esac

ENV=$(printf '%s' "$NAME" | awk -F/ '{print $3}')
case "$ENV" in prod|test) ;; *) echo "refusing: unknown environment '$ENV'" >&2; exit 2 ;; esac
export AWS_PAGER=""

# 1. Write the new value. set-secret.sh verifies the type and the round trip.
bash scripts/set-secret.sh "$NAME" || exit 1

# 2. Recycle every function that reads this environment's secrets. Which
#    functions those are is derived from the environment, not hardcoded per
#    secret -- a new secret under the same prefix needs no change here.
if [ "$ENV" = prod ]; then
  FUNCS="cutRelease releaseAutomationMergeback slackApprove"
else
  FUNCS="cutReleaseTest releaseAutomationMergebackTest"
fi

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
echo
echo "recycling so the new value is actually read (INIT-time read):"
for F in $FUNCS; do
  aws lambda get-function-configuration --function-name "$F" >/dev/null 2>&1 || {
    echo "  - $F  absent, skipped"; continue; }

  # ⚠️ Merge into the EXISTING variables. --environment REPLACES the whole map,
  # so building it from scratch silently drops every other variable -- which on
  # this function means RARC_ENV, and a function without RARC_ENV defaults to
  # the wrong environment.
  NEWENV=$(aws lambda get-function-configuration --function-name "$F" \
             --query 'Environment.Variables' --output json \
           | STAMP="$STAMP" python3 -c '
import sys, json, os
v = json.load(sys.stdin)
v["SECRET_GENERATION"] = os.environ["STAMP"]
print(json.dumps({"Variables": v}))')

  aws lambda update-function-configuration --function-name "$F" \
      --environment "$NEWENV" >/dev/null || { echo "  ✗ $F  update failed"; continue; }
  aws lambda wait function-updated --function-name "$F" 2>/dev/null

  # slackApprove is invoked unqualified; the release functions run behind an
  # alias. Only the aliased ones need a version published and the pointer moved.
  if aws lambda get-alias --function-name "$F" --name live >/dev/null 2>&1; then
    OLD=$(aws lambda get-alias --function-name "$F" --name live --query FunctionVersion --output text)
    NEW=$(aws lambda publish-version --function-name "$F" \
            --description "secret rotation $STAMP" --query Version --output text) || {
              echo "  ✗ $F  publish failed"; continue; }
    if [ "$NEW" = "$OLD" ]; then
      echo "  ✗ $F  publish returned the SAME version ($NEW) — nothing changed, so the"
      echo "        old secret is still being served. This is the trap this script exists for."
      continue
    fi
    aws lambda update-alias --function-name "$F" --name live --function-version "$NEW" >/dev/null
    echo "  ✓ $F  live v$OLD -> v$NEW"
  else
    echo "  ✓ $F  \$LATEST recycled (no alias)"
  fi
done

echo
echo "⚠️  rotate the value at its SOURCE too — writing a new one here does not"
echo "    revoke the old one at Slack, GitHub or Jira."
echo "next: bash scripts/prove-slash.sh $ENV"
