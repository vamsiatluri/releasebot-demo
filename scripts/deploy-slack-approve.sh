#!/usr/bin/env bash
# Build and deploy the Slack approve endpoint's code.
#
#   scripts/deploy-slack-approve.sh
#
# This exists because the first deploy of this function was done by hand, which
# is how its source came to be running in AWS while absent from the repository.
# A deploy nobody can repeat is a deploy nobody can review.
#
# It touches CODE only. The stack's parameters -- signing secret, approver list,
# GitHub token -- are owned by set-approval-token.sh and the template, so the two
# never fight over the same resource.
set -euo pipefail

: "${AWS_PROFILE:=releasebot-sandbox}"
REGION="${AWS_REGION:-us-east-1}"
FUNCTION="${FUNCTION:-slackApprove}"
export AWS_PROFILE

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

STACK=releasebot-slack-approve
BUCKET=$(aws cloudformation describe-stacks --stack-name "$STACK" --region "$REGION" \
  --query "Stacks[0].Parameters[?ParameterKey=='ArtifactBucket'].ParameterValue" --output text)
KEY=$(aws cloudformation describe-stacks --stack-name "$STACK" --region "$REGION" \
  --query "Stacks[0].Parameters[?ParameterKey=='ArtifactKey'].ParameterValue" --output text)
[ -n "$BUCKET" ] && [ -n "$KEY" ] || { echo "could not read the stack's artifact location" >&2; exit 1; }

go test ./internal/slackapprove/... ./internal/slackverify/...

WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT

# ⚠️ provided.al2 runs a file called exactly `bootstrap` at the ROOT of the zip.
# Wrong name, wrong path, wrong architecture, or a cgo-linked binary all fail at
# INIT with "Couldn't find valid bootstrap(s)" or "exec format error" -- neither
# of which names the real cause.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o "$WORK/bootstrap" ./cmd/slackapprove
( cd "$WORK" && zip -q slackapprove.zip bootstrap )

# Prove the packaging before AWS has to discover it.
unzip -l "$WORK/slackapprove.zip" | grep -qE ' bootstrap$' \
  || { echo "✗ bootstrap is not at the root of the zip" >&2; exit 1; }

aws s3 cp "$WORK/slackapprove.zip" "s3://$BUCKET/$KEY" --only-show-errors
aws lambda update-function-code --function-name "$FUNCTION" --region "$REGION" \
  --s3-bucket "$BUCKET" --s3-key "$KEY" --publish >/dev/null
aws lambda wait function-updated --function-name "$FUNCTION" --region "$REGION"

echo "✓ $FUNCTION updated — $(aws lambda get-function-configuration \
  --function-name "$FUNCTION" --region "$REGION" --query CodeSha256 --output text)"
