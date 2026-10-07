# ReleaseBot runbook

Operational ownership: CloudOps / News Group Digital.

## What it is

Two actions, four Lambdas, one Slack app, one Jira webhook, one GitHub token.

| Function | Route | Does |
|---|---|---|
| `cutRelease` | `POST /prod/beta/cut` | Creates `release/<version>` at the tip of `main` |
| `cutReleaseTest` | `POST /test/beta/cut` | Same, test configuration |
| `releaseAutomationMergeback` | `POST /prod/beta/merge` | Opens `release/<version>` → `main` PR |
| `releaseAutomationMergebackTest` | `POST /test/beta/merge` | Same, test configuration |

Each function has a `live` alias. **API Gateway invokes the alias, never
`$LATEST`.** Rollback is therefore always `update-alias`.

## First five minutes of any incident

```bash
# 1. Which version is actually live?
aws lambda get-alias --function-name cutRelease --name live \
  --query '{version:FunctionVersion,modified:AliasArn}'

# 2. What are the errors?
aws logs tail /aws/lambda/cutRelease --since 30m --filter-pattern '{ $.level = "error" }'

# 3. Is it us or GitHub?
aws logs tail /aws/lambda/cutRelease --since 30m --filter-pattern 'github:'

# 4. Is API Gateway even reaching us?
aws logs tail /aws/apigateway/releasebot --since 30m
```

If step 4 is empty and step 2 is empty, the request never arrived — the problem
is the Slack app's Request URL or Slack itself, not the function.

## Symptoms

### "The bot doesn't respond in Slack"

In order of likelihood:

1. **It responded and timed out.** Slack gives 3 seconds. Check the function's
   `Duration` metric. If p99 is near or over 3s, the fix is the async ACK
   (DESIGN-PROPOSALS #3), not a timeout increase.
2. **401.** Check for `rejected unsigned or stale Slack request` in the logs.
   Causes: the signing secret was rotated on one side only; clock skew; the
   Request URL points at an account whose signing secret differs.
3. **Wrong account.** During and shortly after the migration, confirm which
   account the Slack Request URL points at before anything else.

### "It cut the branch twice" / "two PRs"

Expected to be impossible: `CreateBranch` and `OpenPR` are idempotent. If it
happened, the two calls went to **different accounts** — a split brain. Check
whether both the dev and production copies are wired to the same Slack app, and
fix that before fixing anything else.

### "Branch exists at a different commit"

The error is deliberate and the bot is correct to refuse. Someone cut the same
version twice with `main` having moved in between. A human decides which commit
the release is; the bot must not pick.

### 403 or 404 from GitHub

The token. Either expired, rotated on one side, or missing the scope for that
repository. `aws logs tail ... --filter-pattern 'github:'` shows the status code
and path. Never log the token itself, and never paste one into a ticket.

### Jira stopped sending webhooks

Jira disables a webhook after repeated non-2xx responses. The handler returns
**200** for anything it chooses to ignore for exactly this reason. Check the
webhook's status in Jira admin before assuming the function is broken.

## Rotating the Slack signing secret

1. Rotate in the Slack app configuration.
2. Update the Secrets Manager entry (`releasebot/<env>/slack-signing-secret`).
3. Force a cold start so INIT re-resolves: publish a new version and move the
   alias, or update an unrelated environment variable.
4. **Verify the old secret is rejected.** A rotation that leaves the old secret
   working has not happened.

## Deploy and rollback

Deploy: push to `main`, or dispatch `deploy` with a `migration_phase`. The
workflow builds once, deploys to each account, and fails if the deployed
`CodeSha256` is not the digest it built.

Rollback:

```bash
aws lambda list-versions-by-function --function-name cutRelease \
  --query 'Versions[-5:].[Version,LastModified]' --output table
aws lambda update-alias --function-name cutRelease --name live --function-version <N>
```

Takes effect immediately. No redeploy, no CloudFormation, no S3.

⚠️ **A version freezes configuration as well as code**, so rolling the alias back
also rolls back that version's environment. Check before you commit to a target:

```bash
aws lambda get-function-configuration --function-name cutRelease:<N> \
  --query 'Environment.Variables'
```

If a secret has been rotated since version `<N>` was published, that version
carries the OLD value and rolling back to it will fail at startup -- it returns
HTTP 500 with nothing useful in the logs, because the failure is during init.
Roll back to a version published *after* the most recent rotation, or resolve
secrets by reference rather than by value (DESIGN-PROPOSALS #2).

## Escalation

| | |
|---|---|
| Owner | CloudOps / News Group Digital |
| AWS account (prod) | `msnbc` |
| Alarms | `releasebot-cutRelease-errors`, `releasebot-cutRelease-throttles`, `releasebot-unauthorized-spike` |
| Dashboards | CloudWatch `releasebot`; Datadog service `releasebot` |
| Source | this repository |
