# Teardown plan — prod-assess

account 450315222669 · region us-east-1

⚠️ A PLAN, NOT A SCRIPT. This is the one irreversible phase of the
migration — the moment the rollback stops existing. Read every step,
confirm no release is in flight, and have a named rollback owner awake.

## 30 resources NO STACK OWNS

Nothing deletes these for you. This is the list that keeps billing.

  - cloudfront/Robbie App Onboarding
  - logs//aws/apigateway/welcome
  - logs/API-Gateway-Execution-Logs_u4f22c74mi/prod
  - logs/API-Gateway-Execution-Logs_u4f22c74mi/test
  - s3/releasebot-artifacts-450315222669
  - ssm//releasebot/prod/ack-only
  - ssm//releasebot/prod/datadog-service
  - ssm//releasebot/prod/datadog-site
  - ssm//releasebot/prod/default-branch
  - ssm//releasebot/prod/default-owner
  - ssm//releasebot/prod/github-api-url
  - ssm//releasebot/prod/properties
  - ssm//releasebot/prod/secrets/dashboard-github-token
  - ssm//releasebot/prod/secrets/github-token
  - ssm//releasebot/prod/secrets/jira-webhook-secret
  - ssm//releasebot/prod/secrets/slack-signing-secret
  - ssm//releasebot/prod/secrets/slack-token
  - ssm//releasebot/prod/slack-channel
  - ssm//releasebot/test/ack-only
  - ssm//releasebot/test/datadog-service
  - ssm//releasebot/test/datadog-site
  - ssm//releasebot/test/default-branch
  - ssm//releasebot/test/default-owner
  - ssm//releasebot/test/github-api-url
  - ssm//releasebot/test/properties
  - ssm//releasebot/test/secrets/github-token
  - ssm//releasebot/test/secrets/jira-webhook-secret
  - ssm//releasebot/test/secrets/slack-signing-secret
  - ssm//releasebot/test/secrets/slack-token
  - ssm//releasebot/test/slack-channel

## 1. Make it inert, not gone

This is the only irreversible phase in the migration -- it is the moment the
rollback stops existing. Repointing the triggers makes the old account
unreachable while leaving it fully intact, which IS the rollback. Nothing
here deletes anything.

    Repoint the Slack slash commands (/cut, /merge) at the new account
    Repoint the Slack interactivity URL -- separately, and it is the one people forget
    Repoint the Jira webhook
    Soak. Agree the length in writing; this window is the rollback.

  ⚠️  A slash command works while buttons silently do nothing when the
      interactivity URL is still pointed at the old account.

## 2. Revoke the credentials -- starting OUTSIDE AWS

The GitHub token is not an AWS resource, so nothing you delete in AWS
touches it. If the new account was given a copy of the old token, that is a
credential with production reach living in an account nobody watches any
more.

    Rotate the GitHub token at GitHub -- deleting an AWS role does not revoke it
    Rotate the Slack signing secret, and PROVE an old-secret signature is now rejected
    Rotate the Jira webhook shared secret

  ⚠️  Rotation that leaves the old secret valid is not rotation. Verify the
      refusal.

## 3. Empty the buckets -- BEFORE the stacks, not after

CloudFormation never empties a bucket for you; it fails the stack delete
with 'The bucket you tried to delete is not empty (409)'. So emptying has to
happen before the delete, which puts it here.

    scripts/_empty-bucket.py releasebot-artifacts-450315222669
    scripts/_empty-bucket.py releasebot-dashboard-site-450315222669

## 4. Disable the CloudFront distributions

A distribution must be DISABLED and fully propagated before it can be
deleted, and propagation takes 15-20 minutes. That is a scheduling fact, so
it starts here rather than being discovered at the end of the window.

    disable E2L06Q25B0QLUO (releasebot-dashboard dashboard) and WAIT for Deployed  [enabled=true status=Deployed]
    disable E3SEZU6QQCDBGM (Robbie App Onboarding) and WAIT for Deployed  [enabled=true status=Deployed]

## 5. Delete the stacks, deepest dependency first

Everything a stack owns goes with the stack. The order is ranked by what
each stack OWNS -- APIs and distributions first, then compute, then the IAM
roles and VPCs everything else sits on. It is a starting order, not a
dependency graph: read it before you run it.

    aws cloudformation delete-stack --stack-name releasebot-dashboard   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-edge   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-lambda   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-observability   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-slack-approve   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-iam   then WAIT for DELETE_COMPLETE
    aws cloudformation delete-stack --stack-name releasebot-network   then WAIT for DELETE_COMPLETE

  ⚠️  EXPECT one or more network deletes to fail here. That is step 6, not a
      problem.
  ⚠️  A failed create leaves ROLLBACK_COMPLETE, which can be neither updated nor
      created -- it must be deleted before any retry.

## 6. Reap the detached Lambda ENIs, then RETRY what failed

A Lambda's hyperplane ENI is in-use while its function exists and only
becomes `available` AFTER the function is gone. So reaping before the stacks
finds every one attached and skips them all -- this step cannot move earlier
in the order.

    aws ec2 describe-network-interfaces --filters Name=status,Values=available \
      --query "NetworkInterfaces[?starts_with(Description,'AWS Lambda VPC ENI')].NetworkInterfaceId"
    delete each one, then RE-RUN the stack deletes that failed in step 5

  ⚠️  ONLY status=available. An in-use ENI means something really is attached.
  ⚠️  No error message mentions an ENI. It says 'the subnet has dependencies and
      cannot be deleted', and CloudFormation spins on it ~40 minutes before
      giving up.
  ⚠️  Lambda reaps these itself WHILE the function exists. Once the function is
      deleted nothing is left to do the reaping, so waiting never resolves it.

## 7. Sweep what NO STACK OWNS -- where the money hides

A stack delete removes what a stack owns. Everything below was created by
hand, by a bootstrap script, or by AWS itself, so no stack delete will ever
touch it. This list came from the capture, not from anyone's memory.

    delete cloudfront/Robbie App Onboarding
    delete logs//aws/apigateway/welcome
    delete logs/API-Gateway-Execution-Logs_u4f22c74mi/prod
    delete logs/API-Gateway-Execution-Logs_u4f22c74mi/test
    delete s3/releasebot-artifacts-450315222669
    delete ssm//releasebot/prod/ack-only
    delete ssm//releasebot/prod/datadog-service
    delete ssm//releasebot/prod/datadog-site
    delete ssm//releasebot/prod/default-branch
    delete ssm//releasebot/prod/default-owner
    delete ssm//releasebot/prod/github-api-url
    delete ssm//releasebot/prod/properties
    delete ssm//releasebot/prod/secrets/dashboard-github-token
    delete ssm//releasebot/prod/secrets/github-token
    delete ssm//releasebot/prod/secrets/jira-webhook-secret
    delete ssm//releasebot/prod/secrets/slack-signing-secret
    delete ssm//releasebot/prod/secrets/slack-token
    delete ssm//releasebot/prod/slack-channel
    delete ssm//releasebot/test/ack-only
    delete ssm//releasebot/test/datadog-service
    delete ssm//releasebot/test/datadog-site
    delete ssm//releasebot/test/default-branch
    delete ssm//releasebot/test/default-owner
    delete ssm//releasebot/test/github-api-url
    delete ssm//releasebot/test/properties
    delete ssm//releasebot/test/secrets/github-token
    delete ssm//releasebot/test/secrets/jira-webhook-secret
    delete ssm//releasebot/test/secrets/slack-signing-secret
    delete ssm//releasebot/test/secrets/slack-token
    delete ssm//releasebot/test/slack-channel
    ⚠️ ssm delete-parameters takes at most 10 names per call

  ⚠️  /aws/apigateway/welcome is created by AWS the first time an account
      enables API Gateway logging. It is owned by nothing, never expires, and
      survives every delete.

## 8. Verify by RE-QUERYING, not by reading the script's own report

Decommissioning is a verification job, not a delete job. A console showing a
deletion time is not proof: only DELETE_COMPLETE, or describe-stacks
actually failing, proves a stack is gone.

    aws cloudformation list-stacks  (INCLUDE DELETE_FAILED and DELETE_IN_PROGRESS)
    aws lambda list-functions
    aws ec2 describe-vpcs  (non-default)
    aws ec2 describe-network-interfaces
    aws s3api list-buckets
    aws dynamodb list-tables
    aws ssm describe-parameters
    aws logs describe-log-groups
    aws cloudfront list-distributions · aws cognito-idp list-user-pools
    Re-run: baseline capture -estate  →  the inventory should be empty

  ⚠️  QUIET ONE: these tables are stack-owned -- releasebot-ledger. If the
      template carries DeletionPolicy: Retain (correct for an audit record) then
      NO stack delete removes them, the teardown reports success, and the orphan
      then blocks the NEXT create with
      AWS::EarlyValidation::ResourceExistenceCheck and no resource-level event
      naming it. Read the template; the API cannot tell you.

