#!/usr/bin/env bash
# Destroy everything in the sandbox account and PROVE it is gone.
#
# The point is the verification, not the deletion. A console that says "deleted"
# is not evidence:
#   - a stack showing a DeletionTime is not a deleted stack
#   - log groups Lambda created implicitly are not owned by any stack and survive
#   - a versioned bucket is not emptied by deleting objects
# So every class of resource is listed back afterwards and the script fails if
# anything remains.
set -uo pipefail
export AWS_PROFILE="${AWS_PROFILE:-releasebot-sandbox}" AWS_REGION="${AWS_REGION:-us-east-1}" AWS_PAGER=""
ACCT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="releasebot-artifacts-$ACCT"

echo "tearing down ReleaseBot in account $ACCT"
echo

# Dependency order: the API references the aliases, the functions reference the
# roles. Deleting the other way round leaves stacks stuck on in-use resources.
# ⛔ THIS LIST WENT STALE AND THE SCRIPT STILL REPORTED SUCCESS. It predated
# the dashboard, approval and network stacks, so a teardown left a CloudFront
# distribution, a Cognito user pool, a VPC with a NAT Gateway, a DynamoDB table
# and an SQS queue running -- and then printed "TEARDOWN VERIFIED: the account
# is empty", because it only verified the classes it already knew about.
# A teardown that cannot be wrong is worse than no teardown.
#
# Order matters: the dashboard owns a CloudFront distribution (slowest), the
# API references the aliases, the functions reference the roles, and the VPC
# cannot go until Lambda has released its ENIs.
STACKS=(releasebot-dashboard releasebot-slack-approve releasebot-observability
        releasebot-edge2 releasebot-edge releasebot-apigw releasebot-api
        releasebot-probe releasebot-lambda releasebot-network releasebot-iam)

for s in "${STACKS[@]}"; do
  if aws cloudformation describe-stacks --stack-name "$s" >/dev/null 2>&1; then
    echo "  deleting stack $s"
    aws cloudformation delete-stack --stack-name "$s"
  fi
done

# Stray functions created outside CloudFormation during debugging. A teardown
# that only deletes stacks leaves these behind, still billable, still holding an
# execution role.
for fn in probeDirect; do
  if aws lambda get-function --function-name "$fn" >/dev/null 2>&1; then
    echo "  deleting stray function $fn"
    aws lambda delete-function --function-name "$fn"
  fi
done

for s in "${STACKS[@]}"; do
  aws cloudformation wait stack-delete-complete --stack-name "$s" 2>/dev/null || true
done

# Log groups nothing in our templates created. API Gateway writes
# /aws/apigateway/welcome the first time an account enables logging; Lambda
# creates /aws/lambda/<fn> implicitly if a template does not declare it, with
# NEVER-EXPIRE retention. Neither belongs to a stack, so neither is removed by
# deleting one, and both keep billing quietly afterwards. This is the single
# most commonly missed residue of a serverless decommission.
for lg in $(aws logs describe-log-groups --query 'logGroups[].logGroupName' --output text 2>/dev/null); do
  echo "  deleting orphan log group $lg"
  aws logs delete-log-group --log-group-name "$lg" 2>/dev/null || true
done

# ⛔ The ledger table carries DeletionPolicy: Retain, so NO stack delete will
# ever remove it -- correct for production, a leak in a sandbox, and the thing
# that silently blocks the next create (see docs/WHAT-BROKE.md §7 #6).
if aws dynamodb describe-table --table-name releasebot-ledger >/dev/null 2>&1; then
  echo "  deleting the RETAINED ledger table (no stack delete removes it)"
  aws dynamodb delete-table --table-name releasebot-ledger >/dev/null 2>&1 || true
  aws dynamodb wait table-not-exists --table-name releasebot-ledger 2>/dev/null || true
fi

# Cognito user pools outlive their stack if the stack delete was ever forced,
# and a pool with a domain blocks recreating that domain prefix.
for POOL in $(aws cognito-idp list-user-pools --max-results 60 \
    --query 'UserPools[?contains(Name,`releasebot`)].Id' --output text 2>/dev/null); do
  echo "  deleting stray Cognito user pool $POOL"
  aws cognito-idp delete-user-pool --user-pool-id "$POOL" 2>/dev/null || true
done

# S3 last: the Lambda stack reads from it, and a bucket with versioning on is not
# emptied by deleting objects -- the delete markers and old versions remain.
if aws s3api head-bucket --bucket "$BUCKET" 2>/dev/null; then
  echo "  emptying and deleting $BUCKET"
  aws s3 rm "s3://$BUCKET" --recursive --only-show-errors || true
  aws s3api delete-bucket --bucket "$BUCKET" || true
fi

echo
echo "=============== VERIFYING ==============="
FAIL=0

check_empty() { # label, command
  local label="$1"; shift
  local out; out=$("$@" 2>/dev/null)
  if [ -z "$out" ] || [ "$out" = "None" ]; then
    echo "  ok      $label: none remain"
  else
    echo "  FAILED  $label still present:"; echo "$out" | sed 's/^/            /'; FAIL=1
  fi
}

# A stack is only gone when describe-stacks FAILS. Any status string -- including
# DELETE_COMPLETE -- means CloudFormation still has a record of it.
for s in "${STACKS[@]}"; do
  if aws cloudformation describe-stacks --stack-name "$s" >/dev/null 2>&1; then
    st=$(aws cloudformation describe-stacks --stack-name "$s" --query 'Stacks[0].StackStatus' --output text)
    echo "  FAILED  stack $s still described (status $st)"; FAIL=1
  else
    echo "  ok      stack $s: describe-stacks fails, it is gone"
  fi
done

check_empty "lambda functions" aws lambda list-functions --query 'Functions[].FunctionName' --output text
check_empty "rest apis"        aws apigateway get-rest-apis --query 'items[].id' --output text
check_empty "log groups"       aws logs describe-log-groups --log-group-name-prefix /aws --query 'logGroups[].logGroupName' --output text
check_empty "alarms"           aws cloudwatch describe-alarms --query 'MetricAlarms[].AlarmName' --output text
check_empty "dashboards"       aws cloudwatch list-dashboards --query 'DashboardEntries[].DashboardName' --output text
check_empty "sns topics"       aws sns list-topics --query 'Topics[?contains(TopicArn,`releasebot`)].TopicArn' --output text
check_empty "iam roles"        aws iam list-roles --query 'Roles[?contains(RoleName,`releasebot`)].RoleName' --output text
check_empty "s3 buckets"       aws s3api list-buckets --query 'Buckets[?contains(Name,`releasebot`)].Name' --output text
check_empty "synthetics"       aws synthetics describe-canaries --query 'Canaries[].Name' --output text
# ── the classes the stale list never checked ────────────────────────────────
check_empty "dynamodb tables"  aws dynamodb list-tables --query 'TableNames' --output text
check_empty "sqs queues"       aws sqs list-queues --query 'QueueUrls' --output text
check_empty "cognito pools"    aws cognito-idp list-user-pools --max-results 60 --query 'UserPools[].Id' --output text
check_empty "cloudfront dists" aws cloudfront list-distributions --query 'DistributionList.Items[].Id' --output text
check_empty "waf web acls"     aws wafv2 list-web-acls --scope REGIONAL --query 'WebACLs[].Name' --output text
check_empty "nat gateways"     aws ec2 describe-nat-gateways --filter Name=state,Values=available,pending --query 'NatGateways[].NatGatewayId' --output text
check_empty "vpc endpoints"    aws ec2 describe-vpc-endpoints --query 'VpcEndpoints[].VpcEndpointId' --output text
check_empty "ssm parameters"   aws ssm describe-parameters --query 'Parameters[?contains(Name,`releasebot`)].Name' --output text
# ⚠️ A non-default VPC left behind costs nothing by itself, but its NAT Gateway
# and interface endpoints do, and they are easy to miss because nothing lists
# them next to the stack that made them.
check_empty "non-default vpcs" aws ec2 describe-vpcs --filters Name=isDefault,Values=false --query 'Vpcs[].VpcId' --output text

echo
if [ "$FAIL" -eq 0 ]; then echo "TEARDOWN VERIFIED: the account is empty."; else echo "TEARDOWN INCOMPLETE — see FAILED lines above."; exit 1; fi
