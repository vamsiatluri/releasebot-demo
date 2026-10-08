#!/usr/bin/env bash
# Phase 1 in one command: read a running AWS account, say what you are
# inheriting, and draft the template that would recreate it elsewhere.
#
#   scripts/assess.sh <profile> <region> [options]
#
#   # Day one. What is actually in msnbc-dev?
#   scripts/assess.sh msnbc-dev us-east-1 --prefix release
#
#   # Phase 4/5. Do the two accounts agree?
#   scripts/assess.sh msnbc-dev us-east-1 --against msnbc-prod us-east-1
#
# Options
#   --against <profile> <region>   capture a second account and diff the two
#   --prefix <string>              only resources whose name contains this
#   --label <string>               name used in the reports (default: profile)
#   --out <dir>                    output directory (default: build/assess/<date>-<label>)
#
# Exit codes, so this can gate a pipeline:
#   0  nothing blocking
#   1  at least one BLOCKER, or a blocking difference between the two accounts
#   2  warnings only
#   3  the tool itself failed
#
# ⚠️ This reads. It creates nothing, changes nothing and deletes nothing. It is
# safe to point at production, and that is the whole idea -- the inventory you
# can trust is the one taken from the running system rather than from the
# templates somebody believes are current.
#
# ⚠️ It never reads a secret VALUE. Environment variable NAMES are recorded so
# the review can say "this holds a secret in plain text"; the values are not
# read, not stored and not printed.
set -uo pipefail
cd "$(dirname "$0")/.."

die()  { printf '\033[31m✗ %s\033[0m\n' "$1" >&2; exit 3; }
note() { printf '\n\033[1m%s\033[0m\n' "$1"; }

command -v aws >/dev/null     || die "the AWS CLI is not on PATH"
command -v go  >/dev/null     || die "go is not on PATH"

[ $# -ge 2 ] || { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 3; }
PROFILE="$1"; REGION="$2"; shift 2

AGAINST_PROFILE=""; AGAINST_REGION=""; PREFIX=""; LABEL=""; OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --against)
      # Two values, so a missing second one must fail loudly rather than
      # silently capturing the same region twice and reporting "no differences".
      [ $# -ge 3 ] || die "--against needs a profile AND a region"
      AGAINST_PROFILE="$2"; AGAINST_REGION="$3"; shift 3 ;;
    --prefix) [ $# -ge 2 ] || die "--prefix needs a value"; PREFIX="$2"; shift 2 ;;
    --label)  [ $# -ge 2 ] || die "--label needs a value";  LABEL="$2";  shift 2 ;;
    --out)    [ $# -ge 2 ] || die "--out needs a value";    OUT="$2";    shift 2 ;;
    *) die "unknown option: $1" ;;
  esac
done

LABEL="${LABEL:-$PROFILE}"
OUT="${OUT:-build/assess/$(date +%Y%m%d-%H%M)-$LABEL}"
mkdir -p "$OUT" || die "cannot write to $OUT"

# ── Pre-flight ───────────────────────────────────────────────────────────────
# Resolve and PRINT the account id before reading anything. A profile name tells
# you what someone intended; sts tells you where you actually are. Pointing an
# assessment at the wrong account wastes an afternoon and, worse, produces a
# confident report about the wrong system.
acct_of() {
  aws sts get-caller-identity --profile "$1" --region "$2" \
      --query Account --output text 2>/dev/null
}

SRC_ACCT=$(acct_of "$PROFILE" "$REGION")
[ -n "$SRC_ACCT" ] || die "profile '$PROFILE' cannot authenticate in $REGION"
note "source   $PROFILE  $REGION  → account $SRC_ACCT"

if [ -n "$AGAINST_PROFILE" ]; then
  TGT_ACCT=$(acct_of "$AGAINST_PROFILE" "$AGAINST_REGION")
  [ -n "$TGT_ACCT" ] || die "profile '$AGAINST_PROFILE' cannot authenticate in $AGAINST_REGION"
  note "target   $AGAINST_PROFILE  $AGAINST_REGION  → account $TGT_ACCT"
  if [ "$SRC_ACCT" = "$TGT_ACCT" ] && [ "$REGION" = "$AGAINST_REGION" ]; then
    die "both sides are the same account AND region — the diff would be empty by construction"
  fi
fi

# Build rather than `go run`: one binary, used for every call below, so a
# mid-run rebuild cannot change the tool between the capture and the compare.
BIN="$(mktemp -d)/baseline"
trap 'rm -rf "$(dirname "$BIN")"' EXIT
go build -o "$BIN" ./cmd/baseline || die "could not build cmd/baseline"

worst=0
bump() { [ "$1" -gt "$worst" ] && worst="$1"; return 0; }

# ── 1. Read the source account ───────────────────────────────────────────────
note "1/4  reading $LABEL"
"$BIN" capture -profile "$PROFILE" -region "$REGION" -prefix "$PREFIX" \
       -label "$LABEL" -out "$OUT/source.json"
bump $?

# ── 2. Grade it ──────────────────────────────────────────────────────────────
note "2/4  reviewing $LABEL"
"$BIN" review -in "$OUT/source.json" | tee "$OUT/source-review.txt"
bump "${PIPESTATUS[0]}"

# ── 3. Draft the template ────────────────────────────────────────────────────
# ⚠️ A STARTING POINT, not a deployable artifact. It faithfully reproduces
# whatever the source account accumulated by hand. Read it, delete what should
# not come across, then deploy to a sandbox and compare until the only
# differences left are intended ones.
note "3/4  drafting a template from what is actually there"
"$BIN" generate -in "$OUT/source.json" -out "$OUT/generated.yaml" \
  || die "generate failed"
printf '     %s  (%s lines — read it, do not apply it)\n' \
  "$OUT/generated.yaml" "$(wc -l < "$OUT/generated.yaml" | tr -d ' ')"

# ── 4. Compare, when there is something to compare against ───────────────────
if [ -n "$AGAINST_PROFILE" ]; then
  note "4/4  reading the target and diffing"
  "$BIN" capture -profile "$AGAINST_PROFILE" -region "$AGAINST_REGION" \
         -prefix "$PREFIX" -label "${LABEL}-target" -out "$OUT/target.json"
  bump $?
  "$BIN" review -in "$OUT/target.json" > "$OUT/target-review.txt"
  bump $?
  "$BIN" compare -a "$OUT/source.json" -b "$OUT/target.json" | tee "$OUT/compare.txt"
  bump "${PIPESTATUS[0]}"
else
  note "4/4  no --against given, so nothing to diff"
fi

# ── The thing a person reads first ───────────────────────────────────────────
{
  echo "# Account assessment — $LABEL"
  echo
  echo "| | |"
  echo "| --- | --- |"
  echo "| Source | \`$PROFILE\` · $REGION · account $SRC_ACCT |"
  [ -n "$AGAINST_PROFILE" ] && \
  echo "| Target | \`$AGAINST_PROFILE\` · $AGAINST_REGION · account $TGT_ACCT |"
  echo "| Name filter | ${PREFIX:-_(everything)_} |"
  echo "| Taken | $(date -u '+%Y-%m-%d %H:%M UTC') |"
  echo
  echo '```'
  # The header block only. An arbitrary line count used to slice a finding in
  # half, which reads as a corrupt report.
  sed -n '1,4p' "$OUT/source-review.txt"
  grep -E '^ +[0-9]+ blockers,' "$OUT/source-review.txt" || true
  [ -f "$OUT/compare.txt" ] && grep -E '^ +[0-9]+ differences' "$OUT/compare.txt"
  echo '```'
  echo
  echo "## Files"
  echo
  echo "- \`source.json\` — what is actually running (no secret values)"
  echo "- \`source-review.txt\` — what to look at before copying any of it"
  echo "- \`generated.yaml\` — a starting-point template. **Read it, do not apply it.**"
  [ -f "$OUT/compare.txt" ] && echo "- \`compare.txt\` — differences between the two accounts"
  echo
  echo "## Exit code: $worst"
  echo
  case "$worst" in
    0) echo "Nothing blocking." ;;
    1) echo "**At least one blocker.** Migrating as-is would cause an incident." ;;
    2) echo "Warnings only — decide each one deliberately before carrying it over." ;;
    *) echo "The tool itself failed; the report above is incomplete." ;;
  esac
} > "$OUT/SUMMARY.md"

note "done — $OUT/SUMMARY.md"
exit "$worst"
