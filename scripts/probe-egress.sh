#!/usr/bin/env bash
# Prove what a Lambda on these subnets can actually reach. Then delete itself.
#
#   SUBNETS=subnet-a,subnet-b SG=sg-x bash scripts/probe-egress.sh
#   bash scripts/probe-egress.sh                 # reads them from releasebot-network
#
# ⚠️ RUN THIS BEFORE BELIEVING A ROUTE TABLE. On this build a production cut
# timed out reaching api.github.com and the routes, the NAT, the NACLs and the
# security group were all textbook-correct. A probe exactly like this one got
# HTTP 200 in 39ms and ended the theory in a single run -- the fault was
# somewhere else entirely. Four wrong theories were each disproved in two
# minutes by a throwaway function; none of them would have been settled by
# reading configuration.
#
# It creates a function, invokes it, prints a per-stage table, and deletes
# itself on exit even if interrupted.
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

: "${AWS_PROFILE:?set AWS_PROFILE to the target account}"
REGION="${AWS_REGION:-us-east-1}"
export AWS_PROFILE AWS_PAGER=""
NAME="releasebot-egress-probe-$$"

SUBNETS="${SUBNETS:-}"; SG="${SG:-}"
if [ -z "$SUBNETS" ]; then
  SUBNETS=$(aws cloudformation describe-stacks --stack-name releasebot-network --region "$REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='PrivateSubnetIds'].OutputValue" --output text 2>/dev/null)
  SG=$(aws cloudformation describe-stacks --stack-name releasebot-network --region "$REGION" \
    --query "Stacks[0].Outputs[?OutputKey=='LambdaSecurityGroup'].OutputValue" --output text 2>/dev/null)
fi
[ -n "$SUBNETS" ] && [ "$SUBNETS" != "None" ] || {
  echo "set SUBNETS=subnet-a,subnet-b and SG=sg-x (or deploy releasebot-network first)" >&2; exit 2; }
[ -n "$SG" ] && [ "$SG" != "None" ] || { echo "set SG=sg-xxxx" >&2; exit 2; }

# ⚠️ The role needs AWSLambdaVPCAccessExecutionRole or the CREATE is rejected
# naming CreateNetworkInterface but not the policy that grants it.
ROLE_NAME="$NAME-role"
cleanup() {
  aws lambda delete-function --function-name "$NAME" --region "$REGION" >/dev/null 2>&1
  aws iam detach-role-policy --role-name "$ROLE_NAME" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole >/dev/null 2>&1
  aws iam detach-role-policy --role-name "$ROLE_NAME" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole >/dev/null 2>&1
  aws iam delete-role --role-name "$ROLE_NAME" >/dev/null 2>&1
  rm -rf "${WORK:-/nonexistent}"
}
trap cleanup EXIT INT TERM

echo "building the probe…"
WORK=$(mktemp -d)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$WORK/bootstrap" ./cmd/egressprobe || exit 1
( cd "$WORK" && zip -q p.zip bootstrap )

ROLE_ARN=$(aws iam create-role --role-name "$ROLE_NAME" --region "$REGION" \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}' \
  --query Role.Arn --output text) || exit 1
for P in AWSLambdaBasicExecutionRole AWSLambdaVPCAccessExecutionRole; do
  aws iam attach-role-policy --role-name "$ROLE_NAME" \
    --policy-arn "arn:aws:iam::aws:policy/service-role/$P" >/dev/null
done
# IAM is eventually consistent; retry rather than sleep a guess.
echo "creating the probe in $SUBNETS …"
for i in $(seq 1 10); do
  if aws lambda create-function --function-name "$NAME" --region "$REGION" \
      --runtime provided.al2 --handler bootstrap --role "$ROLE_ARN" \
      --zip-file "fileb://$WORK/p.zip" --timeout 120 --memory-size 512 --architectures x86_64 \
      --vpc-config "SubnetIds=$SUBNETS,SecurityGroupIds=$SG" \
      --query FunctionName --output text >/dev/null 2>&1; then
    break
  fi
  [ "$i" = 10 ] && { echo "could not create the probe (IAM not propagated, or bad subnet/SG)" >&2; exit 1; }
  sleep 6
done
aws lambda wait function-active-v2 --function-name "$NAME" --region "$REGION"

echo "probing…"
OUT=$(mktemp)
aws lambda invoke --function-name "$NAME" --region "$REGION" --cli-read-timeout 180 \
  --payload '{}' "$OUT" --query StatusCode --output text >/dev/null
python3 - "$OUT" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
host = None
bad = 0
for r in rows:
    h, st = r["stage"].split(" · ")
    if h != host:
        host = h
        print(f"\n  {h}")
    if r.get("err"):
        bad += 1
        print(f"    {st:<6} {r['ms']:>6}ms  ✗ {r['err'][:88]}")
    else:
        print(f"    {st:<6} {r['ms']:>6}ms  ✓ {r['ok'][:60]}")
print()
if bad:
    print(f"  {bad} unreachable. Read the STAGE that failed, not the word 'timeout':")
    print("    dns fails  -> no resolver route, or DNS not enabled on the VPC")
    print("    tcp hangs  -> no egress path (no NAT / no endpoint), or the SG blocks 443")
    print("    tls fails  -> an inspecting proxy without its CA trusted")
    print("    https 4xx  -> REACHABLE. An HTTP status is a successful connection.")
    sys.exit(1)
print("  everything ReleaseBot needs is reachable from these subnets.")
PY
RC=$?
rm -f "$OUT"
exit $RC
