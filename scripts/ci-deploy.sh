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

for pair in "${PAIRS[@]}"; do
  FN="${pair%%:*}"; ART="${pair##*:}"
  echo "deploying $FN from $ART.zip"
  aws lambda update-function-code --function-name "$FN" \
    --s3-bucket "$BUCKET" --s3-key "releasebot/$SHA/$ART.zip" \
    --query 'FunctionName' --output text >/dev/null

  # update-function-code returns BEFORE the update is applied. A publish-version
  # or alias update issued immediately afterwards fails with
  # ResourceConflictException. Waiting is not optional.
  aws lambda wait function-updated-v2 --function-name "$FN"

  V=$(aws lambda publish-version --function-name "$FN" --query Version --output text)
  aws lambda update-alias --function-name "$FN" --name live --function-version "$V" >/dev/null
  echo "  -> version $V, alias live repointed"
done
