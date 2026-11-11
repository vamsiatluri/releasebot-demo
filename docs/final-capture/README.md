# Final capture — taken immediately before teardown, 2026-11-11

The two demo accounts were torn down after this. **This directory is what makes
a rebuild possible, and what makes a rebuild provable.**

| file | what it is |
|---|---|
| `prod-demo.json` · `prod-assess.json` | full `-estate` inventory of each account at its final state |
| `*-review.txt` | graded findings — 1 blocker each |
| `*-teardown.md` | the ordered decommission plan derived from the capture |
| `parity.txt` | the two accounts compared — 27 differences, 10 blocking |

## To rebuild

The templates are the source of truth, not these files:

```bash
SLACK_SIGNING_SECRET=... JIRA_WEBHOOK_SECRET=... \
  AWS_PROFILE=<profile> scripts/sandbox-up.sh
```

⛔ **No secret is stored here and none should be.** `sandbox-up.sh` refuses to
run without them, deliberately: a literal in this repository is a literal anyone
can read, and a signing secret next to a public URL lets anyone forge a valid
request.

★ **Then prove the rebuild actually matches**, rather than assuming it:

```bash
baseline capture -estate -profile <profile> -region us-east-1 -label rebuilt -out rebuilt.json
baseline compare -a docs/final-capture/prod-demo.json -b rebuilt.json
```

Differences that remain should be ones you intended. That comparison is the
whole reason these captures are worth keeping — without them a rebuild can only
be declared successful, never shown to be.

## What these files do NOT contain

Environment variable **names**, never values. Parameter Store **names and
types**, never contents. The capture tool has no code path that reads a secret,
which is why this directory is safe in a public repository — verified by scan
before commit.

## ⚠️ What the teardown found that nothing else would have

40 resources in prod-demo (30 in prod-assess) **owned by no CloudFormation
stack**. Nothing deletes those for you:

- a CloudFront distribution
- `/aws/apigateway/welcome` — created by AWS itself the first time the account
  enabled API Gateway logging, owned by nothing, removed by no stack delete
- four orphan probe log groups left behind by debugging runs
- 31 SSM parameters written by `bootstrap.sh` with `put-parameter`, deliberately,
  so a published Lambda version freezes a POINTER to its config rather than the
  values — which also means no stack owns them
- the artifacts bucket

That list is the answer to "what keeps billing after the project is done".
