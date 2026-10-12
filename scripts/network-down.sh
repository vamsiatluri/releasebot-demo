#!/usr/bin/env bash
# Detach the functions from the VPC and delete the network stack.
#
#   scripts/network-down.sh
#
# ⚠️ RUN THIS WHEN THE VPC IS NO LONGER NEEDED. The NAT Gateway is ~$32/month
# plus data and the two interface endpoints are ~$15/month between them, and
# none of it stops costing money because nothing is using it.
#
# ⚠️ ORDER MATTERS, and getting it wrong wastes 20 minutes. A VPC cannot be
# deleted while a Lambda ENI is attached to its subnets, and Lambda does not
# release those ENIs the moment you detach the function -- it can take several
# minutes. So: detach the functions FIRST, publish versions so the live aliases
# stop serving VPC-attached code, wait for the ENIs to actually disappear, and
# only then delete the stack. A delete attempted too early fails with
# "DependencyViolation" naming a network interface, which reads like a stuck
# resource rather than a timing problem.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

: "${AWS_PROFILE:=releasebot-sandbox}"
REGION="${AWS_REGION:-us-east-1}"
export AWS_PROFILE AWS_PAGER=""

VPC=$(aws cloudformation describe-stacks --stack-name releasebot-network --region "$REGION" \
        --query "Stacks[0].Outputs[?OutputKey=='VpcId'].OutputValue" --output text 2>/dev/null || true)
[ -n "$VPC" ] && [ "$VPC" != "None" ] || { echo "no releasebot-network stack; nothing to do"; exit 0; }
echo "tearing down $VPC"

# ── 1. Detach every function ────────────────────────────────────────────────
echo "── detaching the functions"
python3 scripts/_update-stack.py releasebot-lambda infra/20-lambda.yaml \
  VpcSubnetIds= VpcSecurityGroupIds=

# ── 2. Move the aliases off the VPC-attached versions ───────────────────────
# A published version freezes VpcConfig. Detaching changes $LATEST only, so the
# live aliases keep serving VPC-attached code and keep the ENIs in use.
echo "── publishing non-VPC versions and moving the aliases"
SHA=$(git rev-parse --short HEAD)
for FN in cutRelease cutReleaseTest releaseAutomationMergeback releaseAutomationMergebackTest; do
  aws lambda wait function-updated-v2 --function-name "$FN" --region "$REGION"
  V=$(aws lambda publish-version --function-name "$FN" --region "$REGION" \
        --description "releasebot $SHA" --query Version --output text)
  aws lambda update-alias --function-name "$FN" --name live --region "$REGION" \
    --function-version "$V" >/dev/null
  echo "   $FN -> v$V (no VpcConfig)"
done

# ── 3. Wait for Lambda to actually release its ENIs ─────────────────────────
echo "── waiting for Lambda to release its ENIs (this is the slow part)"
for i in $(seq 1 40); do
  N=$(aws ec2 describe-network-interfaces --region "$REGION" \
        --filters "Name=vpc-id,Values=$VPC" "Name=description,Values=AWS Lambda VPC ENI*" \
        --query 'length(NetworkInterfaces)' --output text)
  echo "   ENIs still attached: $N"
  [ "$N" = "0" ] && break
  [ "$i" = 40 ] && { echo "   ⚠ ENIs did not clear; delete will likely fail with DependencyViolation" >&2; }
  sleep 30
done

# ── 4. Delete, then PROVE it ────────────────────────────────────────────────
echo "── deleting the network stack"
aws cloudformation delete-stack --stack-name releasebot-network --region "$REGION"
aws cloudformation wait stack-delete-complete --stack-name releasebot-network --region "$REGION" || true

# ⚠️ A delete-stack that exits 0 proves nothing -- the stack can sit in
# DELETE_FAILED. The only acceptable evidence is describe-stacks FAILING by
# name, and every resource class listing back empty.
echo "── verifying, because a teardown that reports success is not a teardown"
if aws cloudformation describe-stacks --stack-name releasebot-network --region "$REGION" >/dev/null 2>&1; then
  echo "✗ the stack still exists" >&2
  aws cloudformation describe-stack-events --stack-name releasebot-network --region "$REGION" \
    --query "StackEvents[?ResourceStatus=='DELETE_FAILED'].[LogicalResourceId,ResourceStatusReason]" \
    --output text | head -5 >&2
  exit 1
fi
echo "   stack: gone (describe-stacks fails by name)"
for q in \
  "vpcs|aws ec2 describe-vpcs --region $REGION --vpc-ids $VPC" \
  ; do :; done
aws ec2 describe-vpcs --region "$REGION" --vpc-ids "$VPC" >/dev/null 2>&1 \
  && { echo "✗ the VPC still exists" >&2; exit 1; } || echo "   vpc: gone"
NAT=$(aws ec2 describe-nat-gateways --region "$REGION" \
        --filter "Name=vpc-id,Values=$VPC" \
        --query "length(NatGateways[?State!='deleted'])" --output text 2>/dev/null || echo 0)
echo "   nat gateways not deleted: $NAT"
EIP=$(aws ec2 describe-addresses --region "$REGION" \
        --query "length(Addresses[?AssociationId==null && Tags[?Value=='releasebot']])" --output text)
echo "   unassociated elastic IPs tagged releasebot: $EIP  (an EIP costs money while it sits idle)"
echo "✓ teardown verified"
