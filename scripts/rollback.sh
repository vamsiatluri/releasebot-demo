#!/usr/bin/env bash
# Move an environment's `live` aliases back to a previously published version.
#
#   scripts/rollback.sh test                 # list what you could roll back to
#   scripts/rollback.sh prod b4cad2f         # roll BOTH prod functions to that commit
#   scripts/rollback.sh prod --to-version 19 # escape hatch: one function, by number
#
# ─────────────────────────────────────────────────────────────────────────────
# WHY THIS ROLLS BACK TO A **COMMIT**, NOT A VERSION NUMBER
#
# An environment is two functions, and their version numbers do not line up --
# cutRelease is on v21 while releaseAutomationMergeback is on v19, and nothing
# in AWS records that those two are the same release. Rolling back "to v19"
# therefore means something different for each function and is how you end up
# with half an environment on one release and half on another: it passes health
# checks, it serves traffic, and it is wrong in a way nothing reports.
#
# So the unit of rollback is the commit. Each function resolves its own version
# from the description written at publish time.
#
# ⚠️ WHY THERE IS A CONFIG PRE-FLIGHT
#
# A published version freezes CONFIGURATION as well as code. If a secret was
# rotated after a version was published, that version still carries the OLD
# secret -- and rolling back to it fails during INIT: HTTP 500, and nothing
# useful in the logs, because the function died before it could log. The target
# therefore has its environment compared against what is current, and a
# mismatch on a secret-bearing key REFUSES the rollback rather than discovering
# it in production.
#
# ⚠️ WHY A PROD ROLLBACK IS NOT GATED BEHIND AN APPROVAL
#
# Deploying new code to production needs a human because nobody has seen it run
# there. Rolling BACK to a version that was already approved and already ran in
# production is the opposite situation, and an approval queue in front of the
# thing you reach for at 2am is a control that costs more than it protects.
# Rollback is therefore immediate, loud, and recorded -- it announces itself to
# Slack with who ran it. Rolling forward to a version that was never promoted is
# refused outright; that is a deploy, and deploys have a gate.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

: "${AWS_PROFILE:=releasebot-sandbox}"
REGION="${AWS_REGION:-us-east-1}"
export AWS_PROFILE AWS_PAGER=""

ENVIRONMENT="${1:-}"
TARGET="${2:-}"
case "$ENVIRONMENT" in
  test) FNS=(cutReleaseTest releaseAutomationMergebackTest) ;;
  prod) FNS=(cutRelease releaseAutomationMergeback) ;;
  *)    echo "usage: $0 <test|prod> [commit]" >&2; exit 2 ;;
esac

# Secret-bearing keys. A difference in ANY of these between the target version
# and the function's current configuration means the target cannot start.
SECRET_KEYS="GITHUB_TOKEN SLACK_TOKEN SLACK_SIGNING_SECRET JIRA_WEBHOOK_SECRET"

cfgof() {  # cfgof <function>[:<version>]  -> "KEY=sha256(value)" lines, secrets only
  aws lambda get-function-configuration --function-name "$1" --region "$REGION" \
    --query 'Environment.Variables' --output json 2>/dev/null \
  | python3 -c '
import hashlib, json, os, sys
keys = os.environ["SECRET_KEYS"].split()
try:
    v = json.load(sys.stdin) or {}
except Exception:
    v = {}
for k in keys:
    val = v.get(k)
    # Hashed, never printed. The question is "did this change", not "what is it".
    print(k + "=" + (hashlib.sha256(val.encode()).hexdigest()[:12] if val else "-"))'
}
export SECRET_KEYS

versions_of() {  # versions_of <function> -> "<version>\t<commit>\t<modified>"
  aws lambda list-versions-by-function --function-name "$1" --region "$REGION" \
    --output json --query 'Versions[?Version!=`$LATEST`].[Version,Description,LastModified]' \
  | python3 -c '
import json, sys
for v, d, m in json.load(sys.stdin)[-12:]:
    d = (d or "").strip()
    # Only a description this pipeline wrote names a commit. Anything else is
    # from a hand-run or from Lambda itself, and guessing a commit out of it
    # would print a false record -- an em dash is the honest rendering.
    c = d[len("releasebot "):].strip() if d.startswith("releasebot ") else "\u2014"
    when = m[:19].replace("T", " ")
    print(f"{v}\t{c[:10]}\t{when}")'
}

# ── no target: show what is reachable, and whether it is actually usable ─────
if [ -z "$TARGET" ]; then
  echo "rollback targets for $ENVIRONMENT  (account $(aws sts get-caller-identity --query Account --output text))"
  for FN in "${FNS[@]}"; do
    LIVE=$(aws lambda get-alias --function-name "$FN" --name live --region "$REGION" \
             --query FunctionVersion --output text)
    CUR=$(cfgof "$FN")
    echo ""
    echo "  $FN   (live -> v$LIVE)"
    printf '    %-7s %-10s %-20s %s\n' VERSION COMMIT PUBLISHED STATE
    while IFS=$'\t' read -r V C M; do
      STATE="rollback target"
      [ "$V" = "$LIVE" ] && STATE="← LIVE NOW"
      if [ "$V" != "$LIVE" ] && [ "$(cfgof "$FN:$V")" != "$CUR" ]; then
        # The single most useful line this script prints.
        STATE="⚠ STALE CONFIG — a secret rotated since; this version cannot start"
      fi
      printf '    v%-6s %-10s %-20s %s\n' "$V" "$C" "$M" "$STATE"
    done < <(versions_of "$FN")
  done
  echo ""
  echo "roll back with:  $0 $ENVIRONMENT <commit>"
  exit 0
fi

# ── resolve the commit to a version PER FUNCTION, and refuse early ───────────
echo "rolling $ENVIRONMENT back to commit $TARGET"
declare -a PLAN=()
for FN in "${FNS[@]}"; do
  V=$(versions_of "$FN" | awk -F'\t' -v c="$TARGET" '$2==c{v=$1} END{print v}')
  [ -n "$V" ] || { echo "✗ $FN has no published version carrying commit $TARGET" >&2
                   echo "  run '$0 $ENVIRONMENT' to see what is available" >&2; exit 1; }

  if [ "$(cfgof "$FN:$V")" != "$(cfgof "$FN")" ]; then
    echo "✗ REFUSED: $FN v$V carries a secret that has since been rotated." >&2
    echo "  Rolling back to it would fail during INIT with an HTTP 500 and an empty log." >&2
    echo "  Roll back to a version published after the most recent rotation instead." >&2
    exit 1
  fi
  PLAN+=("$FN:$V")
  echo "  $FN -> v$V  (config matches current)"
done

# ── move them together ───────────────────────────────────────────────────────
# Resolved for BOTH functions before either moves. A rollback that aborts
# halfway leaves exactly the split-environment state this script exists to
# prevent.
for item in "${PLAN[@]}"; do
  FN="${item%%:*}"; V="${item##*:}"
  WAS=$(aws lambda get-alias --function-name "$FN" --name live --region "$REGION" \
          --query FunctionVersion --output text)
  aws lambda update-alias --function-name "$FN" --name live --region "$REGION" \
    --function-version "$V" --query FunctionVersion --output text >/dev/null
  echo "  ✓ $FN  v$WAS -> v$V"
done

# ── verify by reading the alias BACK, not by trusting the call ───────────────
FAIL=0
for item in "${PLAN[@]}"; do
  FN="${item%%:*}"; V="${item##*:}"
  GOT=$(aws lambda get-alias --function-name "$FN" --name live --region "$REGION" \
          --query FunctionVersion --output text)
  [ "$GOT" = "$V" ] || { echo "✗ $FN: alias reads v$GOT, expected v$V" >&2; FAIL=1; }
done
[ "$FAIL" = 0 ] || exit 1
echo "✓ both aliases verified"

# ── and prove the endpoint actually answers on the rolled-back code ──────────
BASE=$(aws cloudformation describe-stacks --stack-name releasebot-edge --region "$REGION" \
         --query "Stacks[0].Outputs[?OutputKey=='ApiBaseUrl'].OutputValue" --output text 2>/dev/null)
if [ -n "$BASE" ] && [ "$BASE" != "None" ]; then
  H=$(curl -fsS "${BASE%/}/$ENVIRONMENT/beta/health" 2>/dev/null || echo '{}')
  COMMIT=$(printf '%s' "$H" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("commit","?"))
except Exception: print("?")')
  echo "  /health on $ENVIRONMENT now reports commit: $COMMIT"
  [ "$COMMIT" = "$TARGET" ] || echo "  ⚠ that is not $TARGET — investigate before walking away" >&2
fi

if [ -n "${SLACK_WEBHOOK_URL:-}" ]; then
  bash scripts/ci-slack-notify.sh rolledback "$TARGET" "${GITHUB_ACTOR:-$(whoami)}" \
    "${GITHUB_SERVER_URL:-}" 2>/dev/null || true
fi
