#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/.."
export AWS_PROFILE=releasebot-sandbox AWS_PAGER=""
SRC=us-east-1; TGT=us-west-2; ACCT=$(aws sts get-caller-identity --query Account --output text); OUT=build/loop
BUCKET="releasebot-artifacts-$ACCT-$TGT"
# ⛔ No secret defaults. A literal here is a literal in the published repository,
# and a signing secret in source next to a public URL lets anyone sign a valid
# request. The SOW is explicit about this: "do not expose secret values in
# source, logs, tickets, or documentation."
: "${SLACK_SIGNING_SECRET:?set SLACK_SIGNING_SECRET before running}"
: "${JIRA_WEBHOOK_SECRET:?set JIRA_WEBHOOK_SECRET before running}"

record() {
  python3 - "$OUT/iterations.json" "$1" "$2" "$3" "$4" "$5" "$6" <<'PY'
import json,sys,os,datetime
path,n,title,status,detail,total,blocking = sys.argv[1:8]
data = json.load(open(path)) if os.path.exists(path) else []
data = [d for d in data if d["n"] != int(n)]
data.append({"n":int(n),"title":title,"status":status,"detail":detail,
             "diffs_total":int(total),"diffs_blocking":int(blocking),
             "at":datetime.datetime.now(datetime.timezone.utc).isoformat()})
data.sort(key=lambda d:d["n"]); json.dump(data,open(path,"w"),indent=2)
PY
}
deploy() { # template, stackname, extra params...
  local tpl="$1" stack="$2"; shift 2
  aws cloudformation deploy --region $TGT --template-file "$tpl" --stack-name "$stack" \
    --capabilities CAPABILITY_NAMED_IAM --parameter-overrides "$@" 2>&1 | tail -3
}
measure() { # label -> echoes "total blocking"
  ./build/baseline capture -profile releasebot-sandbox -region $TGT -label "target ($TGT)" \
    -out "$OUT/baseline-target.json" >/dev/null 2>&1 || true
  ./build/baseline compare -a "$OUT/baseline-source.json" -b "$OUT/baseline-target.json" \
    > "$OUT/compare-$1.txt" 2>&1
  local t b
  t=$(grep -cE '^  \[' "$OUT/compare-$1.txt" || true)
  b=$(grep -cE '^  \[BLOCKER\]' "$OUT/compare-$1.txt" || true)
  echo "$t $b"
}
step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

ITER="${1:-1}"

if [ "$ITER" = 1 ]; then
  step "ITERATION 1 — deploy the generated template exactly as it came out"
  echo "  Expectation: this should NOT work cleanly. That is the point of the loop."
  OUT1=$(deploy "$OUT/generated-v1.yaml" releasebot-generated \
    ArtifactBucket="$BUCKET" \
    GithubTokenParam=target-placeholder-token \
    SlackTokenParam=target-placeholder-token \
    SlackSigningSecretParam="$SLACK_SIGNING_SECRET" \
    JiraWebhookSecretParam="$JIRA_WEBHOOK_SECRET" 2>&1) || true
  echo "$OUT1" | sed 's/^/  /'
  REASON=$(aws cloudformation describe-stack-events --stack-name releasebot-generated --region $TGT \
    --query 'StackEvents[?ResourceStatus==`CREATE_FAILED`].ResourceStatusReason' --output text 2>/dev/null | head -1)
  echo "  first failure: ${REASON:-none}"
  read -r T B <<< "$(measure 1)"
  record 1 "Deploy the generated template as-is" "failed" \
    "${REASON:-deploy failed}" "$T" "$B"
  echo "  differences against source: $T total, $B blocking"
fi

if [ "$ITER" = 2 ]; then
  step "ITERATION 2 — edit the template, redeploy"
  python3 - <<'PY'
import re
src = open("build/loop/generated-v1.yaml").read()
# EDIT 1: IAM is global. The source account's roles already exist, so the target
# must REFERENCE them rather than create a second copy with the same name. In a
# real account-to-account migration the roles would not collide -- this is an
# artefact of using two regions in one account -- but the edit is the right shape
# either way: a generated template proposes, a human decides.
src = re.sub(r"  ReleasebotExec\w+:\n    Type: AWS::IAM::Role\n(?:.*\n)*?(?=\n  \w|\nOutputs:)", "", src)
src = src.replace("Role: !GetAtt ReleasebotExecCutrelease.Arn",
                  "Role: !Sub 'arn:aws:iam::${AWS::AccountId}:role/releasebot-exec-cutrelease'")
src = src.replace("Role: !GetAtt ReleasebotExecMergeback.Arn",
                  "Role: !Sub 'arn:aws:iam::${AWS::AccountId}:role/releasebot-exec-mergeback'")
# EDIT 2: match the source's retention rather than the generator's guess of 30.
src = src.replace("RetentionInDays: 30   # TODO agree a retention period", "RetentionInDays: 7")
open("build/loop/generated-v2.yaml","w").write(src)
print("  edited: roles referenced not recreated; log retention matched to source")
PY
  deploy "$OUT/generated-v2.yaml" releasebot-generated \
    ArtifactBucket="$BUCKET" \
    GithubTokenParam=target-placeholder-token \
    SlackTokenParam=target-placeholder-token \
    SlackSigningSecretParam="$SLACK_SIGNING_SECRET" \
    JiraWebhookSecretParam="$JIRA_WEBHOOK_SECRET" | sed 's/^/  /'
  read -r T B <<< "$(measure 2)"
  record 2 "Reference existing roles; match log retention" "deployed" \
    "Compute deployed. The API is still absent -- the generator deliberately refuses to emit it, because its integration URIs embed the source account and its invoke URL embeds the API id." "$T" "$B"
  echo "  differences against source: $T total, $B blocking"
fi

if [ "$ITER" = 3 ]; then
  step "ITERATION 3 — hand-write the API against the captured specification"
  deploy infra/30-apigw.yaml releasebot-generated-edge \
    "CutReleaseAliasArn=arn:aws:lambda:$TGT:$ACCT:function:cutRelease:live" \
    "CutReleaseTestAliasArn=arn:aws:lambda:$TGT:$ACCT:function:cutReleaseTest:live" \
    "MergebackAliasArn=arn:aws:lambda:$TGT:$ACCT:function:releaseAutomationMergeback:live" \
    "MergebackTestAliasArn=arn:aws:lambda:$TGT:$ACCT:function:releaseAutomationMergebackTest:live" | sed 's/^/  /'
  read -r T B <<< "$(measure 3)"
  record 3 "Hand-write the API from the captured spec" "deployed" \
    "The API is built from the facts the capture recorded -- routes, stages, stage variables, throttling -- rather than copied." "$T" "$B"
  echo "  differences against source: $T total, $B blocking"
  cat "$OUT/compare-3.txt"
fi
