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
# ⚠️ Same reason as below: a pipe into grep -q under pipefail fails on a match.
unzip -l "$WORK/slackapprove.zip" > "$WORK/zip.listing"
grep -qE ' bootstrap$' "$WORK/zip.listing" \
  || { echo "✗ bootstrap is not at the root of the zip" >&2; exit 1; }

aws s3 cp "$WORK/slackapprove.zip" "s3://$BUCKET/$KEY" --only-show-errors
aws lambda update-function-code --function-name "$FUNCTION" --region "$REGION" \
  --s3-bucket "$BUCKET" --s3-key "$KEY" --publish >/dev/null
aws lambda wait function-updated --function-name "$FUNCTION" --region "$REGION"

# ── Read it back, because printing a hash is not checking one ───────────────
#
# ⛔ The compute stack's deploy compares the live ALIAS's CodeSha256 against the
# artifact it just built, and dies if they differ. That check skipped THIS
# function, because slackApprove has no alias -- so the one safeguard against
# "the deployed bytes are not the bytes you built" did not cover the one
# function that hit exactly that failure.
#
# It is deployed unqualified, so $LATEST is what serves.
WANT=$(openssl dgst -sha256 -binary "$WORK/slackapprove.zip" | openssl base64)
GOT=$(aws lambda get-function-configuration --function-name "$FUNCTION" \
        --region "$REGION" --query CodeSha256 --output text)
if [ "$WANT" != "$GOT" ]; then
  echo "✗ $FUNCTION is NOT running the bytes just built." >&2
  echo "    built  $WANT" >&2
  echo "    serves $GOT" >&2
  exit 1
fi

# ⚠️ And prove the resolver is actually IN the binary. A build that predates
# internal/secretref reads an `ssm:` reference as the literal secret and returns
# 401 to every real Slack request -- which is what happened, for an hour, with
# no error anywhere. One grep closes it.
# ⚠️ EXTRACT, THEN GREP. `unzip -p … | strings | grep -q` under `set -o
# pipefail` fails WHEN IT MATCHES: grep -q exits on the first hit, upstream
# gets SIGPIPE, and the pipeline's status is the failure. This guard reported
# "the binary does not contain the resolver" about a binary that did. Same bug
# as `curl | grep -q`, already in docs/WHAT-BROKE.md -- and the lint rule only
# matched `curl`, so it did not catch this one.
unzip -p "$WORK/slackapprove.zip" bootstrap > "$WORK/bootstrap.extracted"
strings "$WORK/bootstrap.extracted" > "$WORK/bootstrap.strings"
if ! grep -q secretref "$WORK/bootstrap.strings"; then
  echo "✗ the built binary does not contain the secret resolver." >&2
  echo "    Its environment holds an ssm: reference, which this build would" >&2
  echo "    HMAC as if it were the secret. Every Slack signature would fail." >&2
  exit 1
fi

echo "✓ $FUNCTION updated — $GOT"
echo "  verified: the live function serves the bytes just built, and the"
echo "  binary contains the secret resolver"
