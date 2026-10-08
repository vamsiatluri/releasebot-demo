#!/usr/bin/env bash
# Run the capture -> generate -> edit -> deploy -> compare loop end to end, and
# record every iteration as JSON for the visualisation.
#
# SOURCE  us-east-1  (stands in for msnbc-dev)
# TARGET  us-west-2  (stands in for the msnbc production account)
#
# Two regions in one account rather than two accounts, because the source has to
# stay up the whole time -- which is exactly what the real parallel run does. The
# one place the analogy leaks is IAM, which is global rather than regional, and
# iteration 1 below walks straight into that on purpose.
set -uo pipefail
cd "$(dirname "$0")/.."
export AWS_PROFILE=releasebot-sandbox AWS_PAGER=""
SRC=us-east-1
TGT=us-west-2
ACCT=$(aws sts get-caller-identity --query Account --output text)
OUT=build/loop
mkdir -p "$OUT"

note() { printf '\n\033[1m%s\033[0m\n' "$1"; }

record() { # iteration, title, status, detail, diffs_total, diffs_blocking
  python3 - "$OUT/iterations.json" "$1" "$2" "$3" "$4" "$5" "$6" <<'PY'
import json,sys,os,datetime
path,n,title,status,detail,total,blocking = sys.argv[1:8]
data = json.load(open(path)) if os.path.exists(path) else []
data = [d for d in data if d["n"] != int(n)]
data.append({"n":int(n),"title":title,"status":status,"detail":detail,
             "diffs_total":int(total),"diffs_blocking":int(blocking),
             "at":datetime.datetime.now(datetime.timezone.utc).isoformat()})
data.sort(key=lambda d:d["n"])
json.dump(data,open(path,"w"),indent=2)
PY
}

diffcount() { # baseline_a baseline_b -> "total blocking"
  ./build/baseline compare -a "$1" -b "$2" >"$OUT/compare-$3.txt" 2>&1
  local t b
  t=$(grep -cE '^  \[(BLOCKER|WARN   |INFO   )\]' "$OUT/compare-$3.txt" || true)
  b=$(grep -cE '^  \[BLOCKER\]' "$OUT/compare-$3.txt" || true)
  echo "$t $b"
}

go build -o build/baseline ./cmd/baseline

note "STEP 1  capture the source account as it actually runs"
./build/baseline capture -profile releasebot-sandbox -region $SRC -label "source (us-east-1)" \
  -out "$OUT/baseline-source.json" >/dev/null 2>&1 || true
python3 -c "
import json;d=json.load(open('$OUT/baseline-source.json'))
print(f\"  {len(d['functions'])} functions, {len(d['roles'])} roles, {len(d['apis'])} APIs, {len(d['log_groups'])} log groups\")"
./build/baseline review -in "$OUT/baseline-source.json" > "$OUT/review-source.txt" 2>&1 || true
tail -1 "$OUT/review-source.txt"
record 0 "Capture the source" "done" "Read the running account, not its templates. $(tail -1 "$OUT/review-source.txt" | xargs)" 0 0

note "STEP 2  generate a starting-point template from the capture"
./build/baseline generate -in "$OUT/baseline-source.json" -out "$OUT/generated-v1.yaml" >/dev/null
echo "  $(wc -l < "$OUT/generated-v1.yaml") lines, $(grep -c 'TODO\|BLOCKER\|WARN' "$OUT/generated-v1.yaml") review markers for a human to resolve"

note "STEP 3  prepare the target region"
aws s3api head-bucket --bucket "releasebot-artifacts-$ACCT-$TGT" --region $TGT 2>/dev/null || \
  aws s3api create-bucket --bucket "releasebot-artifacts-$ACCT-$TGT" --region $TGT \
    --create-bucket-configuration LocationConstraint=$TGT >/dev/null
aws s3 cp build/cutrelease.zip "s3://releasebot-artifacts-$ACCT-$TGT/releasebot/cutRelease.zip" --region $TGT --only-show-errors
aws s3 cp build/cutrelease.zip "s3://releasebot-artifacts-$ACCT-$TGT/releasebot/cutReleaseTest.zip" --region $TGT --only-show-errors
aws s3 cp build/mergeback.zip  "s3://releasebot-artifacts-$ACCT-$TGT/releasebot/releaseAutomationMergeback.zip" --region $TGT --only-show-errors
aws s3 cp build/mergeback.zip  "s3://releasebot-artifacts-$ACCT-$TGT/releasebot/releaseAutomationMergebackTest.zip" --region $TGT --only-show-errors
echo "  artifacts staged in $TGT"
