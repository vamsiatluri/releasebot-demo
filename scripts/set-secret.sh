#!/usr/bin/env bash
# Write ONE secret into Parameter Store as a SecureString.
#
#   bash scripts/set-secret.sh /releasebot/prod/slack-signing-secret
#
# ⛔ WHY THIS SCRIPT EXISTS RATHER THAN A CLOUDFORMATION PARAMETER.
# A NoEcho parameter is masked in describe-stacks and is still a value that
# travelled through a template, a change set, and anything that archives change
# sets. Writing it here means the secret never enters CloudFormation: the stack
# carries the parameter NAME, and the function resolves it at INIT.
#
# ⚠️ The value is read from a SILENT PROMPT or from the environment, NEVER from
# argv. Arguments are visible in `ps` to every process on the machine and land
# in shell history. Same rule as set-approval-token.sh.
#
# ⚠️ And it is never echoed back. A script that prints the secret it just set
# "to confirm" has put it in the terminal scrollback and in any transcript.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAME="${1:-}"
[ -n "$NAME" ] || { echo "usage: $0 /releasebot/<env>/<name>   (value via RELEASEBOT_SECRET or prompt)" >&2; exit 2; }
case "$NAME" in
  /releasebot/*) ;;
  *) echo "refusing: the name must start with /releasebot/ — got '$NAME'" >&2; exit 2 ;;
esac

export AWS_PAGER=""
ACCT=$(aws sts get-caller-identity --query Account --output text) || {
  echo "cannot reach AWS — check AWS_PROFILE" >&2; exit 1; }
REGION="${AWS_REGION:-us-east-1}"

VALUE="${RELEASEBOT_SECRET:-}"
if [ -z "$VALUE" ]; then
  # ⚠️ Read from the TTY, not stdin, so this still prompts when the script is
  # piped. -s so it is not echoed; the trailing newline is printed manually
  # because -s swallows the one the user typed.
  printf 'value for %s (not echoed): ' "$NAME" >&2
  IFS= read -rs VALUE < /dev/tty || true
  printf '\n' >&2
fi
# ⚠️ Bracketed paste wraps a pasted value in ESC[200~ … ESC[201~ and a terminal
# that has it enabled corrupts `read -s` silently. Strip the markers and every
# control character, or a perfectly good secret is stored with invisible bytes
# around it and every signature comparison fails for reasons nothing reports.
VALUE=$(printf '%s' "$VALUE" | sed -e 's/\x1b\[20[01]~//g' | tr -d '\000-\037')
[ -n "$VALUE" ] || { echo "no value given" >&2; exit 2; }

case "$VALUE" in
  *...*|*'<'*|*'>'*|*YOUR*|*REPLACE*|*example*)
    echo "refusing: that looks like a placeholder, not a secret" >&2; exit 2 ;;
esac

# Existing type matters: overwriting a String with --overwrite keeps it a
# String unless the type is restated, so a "migrated" secret can quietly stay
# unencrypted. Read it back and assert instead of trusting the write.
PREV=$(aws ssm get-parameter --name "$NAME" --query 'Parameter.Type' --output text 2>/dev/null || echo ABSENT)

# ⚠️ The value goes via a FILE, not --value on the command line. Same reason as
# argv above. mode 600, removed on exit however the script ends.
TMP=$(mktemp); chmod 600 "$TMP"
trap 'rm -f "$TMP"' EXIT
printf '%s' "$VALUE" > "$TMP"

# alias/aws/ssm is created by AWS on the first SecureString write in an account,
# so this works in a brand-new one -- unlike aws/dynamodb, which does NOT
# self-create and broke the ledger table in the clean-account run. Pass
# RELEASEBOT_KMS_KEY to use a customer-managed key instead, which is what a
# production account should do: a CMK lets kms:Decrypt be scoped by key policy
# and gives per-key CloudTrail.
# ⚠️ NOT an array. macOS ships bash 3.2, where expanding an EMPTY array under
# `set -u` is an unbound-variable error -- "${ARGS[@]}" with no elements aborts
# the script. It works on bash 4+, so it passes everywhere except the laptop it
# will actually be run from. Two explicit calls are uglier and portable.
if [ -n "${RELEASEBOT_KMS_KEY:-}" ]; then
  aws ssm put-parameter --name "$NAME" --type SecureString \
    --value "file://$TMP" --overwrite --key-id "$RELEASEBOT_KMS_KEY" >/dev/null || {
      echo "put-parameter failed" >&2; exit 1; }
else
  aws ssm put-parameter --name "$NAME" --type SecureString \
    --value "file://$TMP" --overwrite >/dev/null || {
      echo "put-parameter failed" >&2; exit 1; }
fi

# ── Verify at the destination ───────────────────────────────────────────────
# Read the TYPE back. A successful write is not proof the parameter is
# encrypted: it can succeed and leave an existing String a String.
NOW=$(aws ssm get-parameter --name "$NAME" --query 'Parameter.Type' --output text 2>/dev/null || echo UNREADABLE)
if [ "$NOW" != "SecureString" ]; then
  echo "✗ $NAME is type $NOW, not SecureString — the value is NOT encrypted" >&2
  exit 1
fi

# And that the stored bytes match, WITHOUT printing either. Comparing hashes is
# the only way to prove the round trip survived without putting the secret on
# screen.
WANT=$(printf '%s' "$VALUE" | openssl dgst -sha256 | awk '{print $NF}')
GOT=$(aws ssm get-parameter --name "$NAME" --with-decryption \
        --query 'Parameter.Value' --output text 2>/dev/null \
      | tr -d '\n' | openssl dgst -sha256 | awk '{print $NF}')
if [ "$WANT" != "$GOT" ]; then
  echo "✗ stored value does not match what was given (hashes differ)" >&2
  echo "  this is usually a trailing newline or a control character" >&2
  exit 1
fi

printf '✓ %s stored as SecureString in %s/%s (was: %s), round trip verified\n' \
  "$NAME" "$ACCT" "$REGION" "$PREV"
printf '  ⚠️  rotate the old value at its source — writing a new one here does NOT revoke it\n'
