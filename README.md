# ReleaseBot (Robbie) — migration reference environment

A working model of the NBC News CloudOps project: move ReleaseBot from the
`msnbc-dev` AWS account to the `msnbc` production account, validate both in
parallel, cut over, and decommission the dev resources.

It is not a guess at NBC's source code. It is a **faithful reconstruction of the
system the SOW describes** — four Go Lambdas on a `provided.al2` custom runtime
behind two API Gateway routes, driven by Slack slash commands and Jira webhooks,
with GitHub as the thing being acted on — built so that every decision in the
migration plan can be demonstrated rather than asserted.

Everything here runs locally. **No AWS account is touched, nothing is deployed,
nothing costs money.**

```bash
make test        # unit tests, including the signature-verification cases
make package     # builds linux/amd64 bootstrap zips and verifies the packaging
make mocks       # terminal 1: fake GitHub + Slack
make local       # terminal 2: both handlers behind a local API Gateway
make demo        # or: the whole thing, 13 scenarios, start to finish
```

## What ReleaseBot does

The SOW does not say, but its scope of work pins it down: two routes named
`/beta/cut` and `/beta/merge`, functions named `cutRelease` and
`releaseAutomationMergeback`, a `GITHUB_TOKEN`, a `SLACK_TOKEN`, Jira
credentials, and Slack slash commands. That is a release bot that cuts a release
branch off the trunk on command and later opens the mergeback pull request —
the two moments in a release train where a human doing it by hand gets it wrong.

This implementation does exactly that, and the point of the exercise is that the
**behaviour is the part the migration must not change**. The SOW says so
explicitly: *"preserve existing function names and application behavior unless a
change is required and approved."*

## Layout

| Path | What it is |
|---|---|
| `internal/runtime` | The Lambda Runtime API loop. On `provided.*` **you** own this — it is what `bootstrap` is. |
| `internal/slackverify` | Slack request signing: HMAC-SHA256, constant-time compare, 5-minute replay window. |
| `internal/release` | The actual behaviour: cut, mergeback. No AWS imports, so it can be pinned by tests that run identically against both accounts' artifacts. |
| `internal/handler` | API Gateway proxy adapter shared by all four functions. |
| `infra/10-iam.yaml` | GitHub OIDC deploy role + least-privilege execution roles. |
| `infra/20-lambda.yaml` | The four functions, versions, `live` aliases, log groups, alarms. |
| `infra/30-apigw.yaml` | REST API, two stages wired to aliases through stage variables, access logging, custom domain. |
| `.github/workflows/deploy.yml` | Build once, deploy to both accounts, verify the deployed bytes, smoke test. |
| `docs/` | The migration plan, the cutover checklist, the rollback, the runbook, and the design proposals. |

## Start with these

- **[docs/DESIGN-PROPOSALS.md](docs/DESIGN-PROPOSALS.md)** — eight changes worth
  proposing, each with the argument *against* it, and each marked for the phase
  it belongs in. Lambda aliases are #1.
- **[docs/MIGRATION-PLAN.md](docs/MIGRATION-PLAN.md)** — the SOW's seven phases
  with the failure mode each one is actually guarding against.
- **[docs/CUTOVER.md](docs/CUTOVER.md)** — the checklist, and the rollback that
  stays valid until acceptance.

## The three things this repo is really arguing

1. **A migration's risk is not in the Lambdas. It is in the integrations.**
   Four functions and two routes move in an afternoon. The Slack request URL,
   the Jira webhook, the GitHub token's identity in the audit log, and the
   invoke URL embedded in other people's runbooks are what make it a project.

2. **"Parallel run" is a split-brain risk, not a safety net.** Two deployed
   copies of a bot holding write credentials to the same repositories can both
   act on the same event. The validation plan has to say which one is the system
   of operation at every moment, and the deploy pipeline has to make that
   visible.

3. **Decommission is a verification step, not a delete step.** A deleted stack
   that still has an implicit log group, a still-valid access key, or an
   un-rotated token is not decommissioned. See `docs/CUTOVER.md`.
