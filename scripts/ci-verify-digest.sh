#!/usr/bin/env bash
# Assert that what is deployed is what we built.
#
#   ci-verify-digest.sh <function> <expected CodeSha256>
#
# A deploy step reporting success is not evidence the right bytes arrived. This
# reads the deployed fingerprint back off the live alias and fails if it differs,
# which is what turns "we shipped what we tested" from a hope into a check.
set -euo pipefail
FN="${1:?function name}"
WANT="${2:?expected digest}"
GOT=$(aws lambda get-function-configuration --function-name "$FN:live" \
        --query CodeSha256 --output text)
if [ "$GOT" != "$WANT" ]; then
  echo "::error::$FN:live is running $GOT, expected $WANT"
  exit 1
fi
echo "ok: $FN:live == $WANT"
