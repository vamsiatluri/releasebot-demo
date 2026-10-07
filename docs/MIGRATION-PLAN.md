# Migration plan

The SOW's seven phases, with the failure mode each one exists to prevent. The
phase names are theirs; the "what actually goes wrong here" column is the work.

---

## Phase 1 — Access and baseline

**Their activity:** confirm `msnbc` credentials with STS; inventory current
roles, variables, workflows, integrations and monitoring.

**What actually goes wrong here:** the inventory is taken from the
CloudFormation templates instead of from the running account, and misses
everything that was changed by hand. On a six-year-old release bot that is
usually: an environment variable someone set in the console during an incident,
an inline IAM policy, a log group with no retention, and at least one secret
nobody can explain.

**Baseline to capture, from the live account, as files in the repo:**

```bash
aws lambda get-function-configuration --function-name cutRelease      > baseline/cutRelease.json
aws lambda get-policy                 --function-name cutRelease      > baseline/cutRelease-policy.json
aws apigateway get-rest-apis                                          > baseline/apis.json
aws apigateway get-stages --rest-api-id "$API"                        > baseline/stages.json
aws iam get-role --role-name "$EXEC_ROLE"                             > baseline/exec-role.json
aws iam list-role-policies --role-name "$EXEC_ROLE"                   > baseline/exec-inline.json
aws logs describe-log-groups --log-group-name-prefix /aws/lambda/cut  > baseline/log-groups.json
```

Diff the production account against these at the end of Phase 2. **That diff is
the deliverable**, not the templates.

Also inventory the things that are not in AWS at all, because these are what the
cutover actually consists of:

- the Slack app's slash command Request URL and interactivity URL
- the Jira webhook URL and its shared secret
- which GitHub identity the token belongs to, and its scopes
- the Datadog integration's AWS account mapping
- anything with the invoke URL hardcoded in it

**Deliverable:** approved baseline, and a signed list of every integration that
holds the current URL.

---

## Phase 2 — AWS infrastructure

**Their activity:** four execution roles and four Lambda functions on the
`provided.al2` Go custom runtime with a bootstrap handler.

**What actually goes wrong here:** the packaging. `provided.*` requires an
executable named exactly `bootstrap` at the **root** of the zip, built
`linux/amd64`, `CGO_ENABLED=0`. Get any of those wrong and the function fails at
INIT with `fork/exec /var/task/bootstrap: exec format error` or
`Couldn't find valid bootstrap(s)` and nothing more useful. The Makefile's
`verify-zip` target fails the build for all three cases rather than discovering
them in AWS.

Second thing: **do not let Lambda create its own log groups.** Implicit log
groups are created with *never expire* retention and are not owned by any stack,
so they survive Phase 7 and keep billing. `infra/20-lambda.yaml` declares them.

**Deliverable:** both the production and test Lambda foundations, plus the
Phase 1 diff showing the production account matches the baseline.

---

## Phase 3 — API and configuration

**Their activity:** API Gateway routes and stages; `GITHUB_TOKEN`,
`SLACK_TOKEN`, Jira credentials, `RARC_ENV`, `RARC_CONFIG_URL`, the signing
secret, Datadog variables.

**What actually goes wrong here:** the new API gets a new `apiId`, so the invoke
URL changes. That is the whole cutover. Write the new URL down in Phase 3 and
start socialising it immediately — do not discover it in the cutover window.

The configuration trap: copying the dev account's secret *values* into the
production account's function configuration by hand. Phase 3 is the cheapest
moment to put them in Secrets Manager instead (DESIGN-PROPOSALS #2) because
nothing is live yet.

**Deliverable:** operational API stack; the new URL circulated; secrets resolved
by reference and never printed in a workflow log, a ticket or this repo.

---

## Phase 4 — CI/CD parallel run

**Their activity:** add production-account repository secrets and secondary
deploy jobs to all four workflows, while retaining dev deployment.

**What actually goes wrong here:** **split brain.** Two deployed bots, both
holding a GitHub token with branch-write rights, both reachable. If both are
wired to Slack or Jira, one release cut produces two actions on real
repositories.

Three controls, all in `.github/workflows/deploy.yml`:

1. **Build once, deploy many.** One artifact, one checksum, both accounts. Then
   compare the deployed `CodeSha256` against the built digest in each account —
   "both accounts run identical code" becomes a verified fact rather than a
   claim.
2. **Exactly one account is the system of operation at any moment**, and the
   pipeline says which one in its run summary.
3. **The production copy is validated by direct invoke and its own test stage
   only** until cutover. It is never the target of a Slack or Jira URL.

Also: `fail-fast: false` on the matrix. A production deploy failure must not
cancel the dev deploy that is still serving the team.

**Deliverable:** dual-account deployment capability, with the split-brain
boundary written into the pipeline rather than into someone's memory.

---

## Phase 5 — Validation

**Their activity:** smoke-test `/beta/cut` and `/beta/merge`; verify Slack
notifications, Jira webhook handling, GitHub configuration access, logs, metrics
and alerts.

**What actually goes wrong here:** the smoke test proves the function is alive
but not that it is *correct*, because the only honest test of a release bot is
cutting a release — and nobody wants to cut a real one from an unvalidated
account.

Resolve it with a **shadow repository**: an empty repo in the same GitHub
organisation, with the same branch protection, that the production account's
test stage drives through a full release cycle. Real GitHub API, real token,
real Slack channel, real Jira project — zero production impact.

Evidence worth having at acceptance:

| Check | Why |
|---|---|
| Cut, then cut the same version again | Idempotency: the retry path is the one a real incident will take |
| Mergeback for a branch that was never cut | Error path returns something a human can act on |
| Forged Slack signature | 401. This is the only thing standing between a public URL and your `main` branch |
| Replay a captured request 10 minutes later | 401. Proves the timestamp window, not just the HMAC |
| Old signing secret after rotation | 401. Proves the rotation actually landed |
| Jira webhook with the wrong secret | 401 |
| Jira webhook with no `fixVersion` | **200**, ignored. A non-2xx makes Jira retry and eventually disable the webhook |
| Alarm fires end-to-end | An alarm nobody has seen fire is not monitoring |

`make demo` runs all of these locally, in order. The same list run against the
production account's test stage is the Phase 5 evidence package.

**Deliverable:** production-readiness evidence — outputs, not assertions.

---

## Phase 6 — Cutover

**Their activity:** update production and test Slack request URLs; replace
canonical AWS secrets; remove parallel jobs; run a full release cycle.

See **[CUTOVER.md](CUTOVER.md)**. The short version: the cutover is a handful of
URL fields in other people's systems, each one is a single-field edit, and each
one is therefore also a single-field rollback — **until** the dev stack is
deleted. That is why decommission is a separate phase with its own gate.

---

## Phase 7 — Decommission

**Their activity:** delete legacy Lambdas and API Gateway; revoke or rotate dev
deployment credentials; update runbooks; complete handoff.

**What actually goes wrong here:** "deleted" is assumed rather than verified.

- A CloudFormation stack showing a `DeletionTime` is not a deleted stack.
  `DELETE_COMPLETE`, or `describe-stacks` failing with "does not exist", is the
  only proof.
- Implicitly-created log groups are not stack-owned and survive deletion.
- An IAM role can be deleted while a *user's* access key still works.
- An S3 artifact bucket with versioning on is not emptied by deleting objects.
- **The GitHub token is the one that actually matters.** It is not an AWS
  resource and no stack deletion touches it. If the production account got a
  copy of the dev token, the dev token is now a credential with production reach
  sitting in an account nobody is watching. Rotate it, and verify the old one is
  rejected.

**Gate:** do not start Phase 7 until production acceptance is signed. Deleting
the dev stack is the moment the rollback stops existing.

**Deliverable:** no ReleaseBot dependency in `msnbc-dev`, each deletion
evidenced, and every credential that ever had production reach rotated and
proven dead.
