#!/usr/bin/env bash
# Deploy the already-built artifact to one environment's functions.
#
#   ci-deploy.sh <test|prod> <sha>
#
# Publishes an immutable version and repoints the `live` alias at it. Rollback is
# then one update-alias call against the previous version -- no rebuild, no
# pipeline run, no CloudFormation.
set -euo pipefail
ENVIRONMENT="${1:?test or prod}"
SHA="${2:?commit sha}"
ACCT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="releasebot-artifacts-$ACCT"

case "$ENVIRONMENT" in
  test) PAIRS=("cutReleaseTest:cutrelease" "releaseAutomationMergebackTest:mergeback") ;;
  prod) PAIRS=("cutRelease:cutrelease" "releaseAutomationMergeback:mergeback") ;;
  *)    echo "unknown environment: $ENVIRONMENT" >&2; exit 2 ;;
esac

# The run page should say what MOVED, not just that a script exited 0. Anyone
# reading a release afterwards wants two numbers per function -- where the alias
# was and where it is now -- because those two numbers are the rollback target
# and the thing to roll back.
summary() { [ -n "${GITHUB_STEP_SUMMARY:-}" ] && printf '%s\n' "$*" >> "$GITHUB_STEP_SUMMARY"; return 0; }
summary "### Deployed to \`$ENVIRONMENT\` — commit \`$SHA\`"
summary ""
summary "| function | alias was | alias now | rollback with |"
summary "|---|---|---|---|"

for pair in "${PAIRS[@]}"; do
  FN="${pair%%:*}"; ART="${pair##*:}"
  echo "deploying $FN from $ART.zip"

  # Captured BEFORE the deploy: once the alias moves, the previous target is not
  # recoverable from anything the deploy wrote down.
  PREV=$(aws lambda get-alias --function-name "$FN" --name live \
           --query FunctionVersion --output text 2>/dev/null || echo "-")
  aws lambda update-function-code --function-name "$FN" \
    --s3-bucket "$BUCKET" --s3-key "releasebot/$SHA/$ART.zip" \
    --query 'FunctionName' --output text >/dev/null

  # update-function-code returns BEFORE the update is applied. A publish-version
  # or alias update issued immediately afterwards fails with
  # ResourceConflictException. Waiting is not optional.
  aws lambda wait function-updated-v2 --function-name "$FN"

  # ⚠️ The description carries the COMMIT, and it is the only durable link
  # between a version number and what is in it. Without it, rollback is a
  # choice between integers: cutRelease v19 and mergeback v17 can be the same
  # release, and nothing in AWS says so. With it, you roll back to a COMMIT and
  # each function resolves its own version.
  V=$(aws lambda publish-version --function-name "$FN" \
        --description "releasebot $SHA" --query Version --output text)
  aws lambda update-alias --function-name "$FN" --name live --function-version "$V" >/dev/null
  echo "  -> version $V, alias live repointed (was $PREV)"
  summary "| \`$FN\` | $PREV | **$V** | \`aws lambda update-alias --function-name $FN --name live --function-version $PREV\` |"
done

summary ""
summary "Both functions of the environment moved together. No rebuild occurred."
