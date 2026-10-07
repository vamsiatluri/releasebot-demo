# Design proposals

Each one states the change, the argument for it, **the argument against it**, and
the phase it belongs in. The SOW says *"preserve existing function names and
application behavior unless a change is required and approved"* — so none of
these are decisions to make unilaterally during a migration. The move is to
land the migration like-for-like, and arrive with these written down.

Phases: **P0** = do it as part of the migration · **P1** = propose for the
quarter after acceptance · **P2** = raise, do not push.

> **On a three-month contract, P1 and P2 are not a roadmap — they are the
> handoff.** You will not be there for the quarter after acceptance, so pitching
> these as "what I'd do next" is pitching work you won't do. Each one is written
> here with the argument *for*, the argument *against*, and the trap, so the
> team can decide once you've gone. That is the deliverable. See
> [90-DAY-PLAN.md](90-DAY-PLAN.md).

---

## 1. Collapse the four functions into two, with aliases — **P1**

Today: `cutRelease` / `cutReleaseTest` and `releaseAutomationMergeback` /
`releaseAutomationMergebackTest`. Four functions, two codebases, four
deployments, four sets of environment variables that can drift.

Proposed: `cutRelease` and `releaseAutomationMergeback`, each with a `prod` and
a `test` alias on published versions. API Gateway's prod stage points at the
`prod` alias, the test stage at `test`, through a stage variable.

**For**

- **Promotion stops being a rebuild.** Today "ship test, then ship prod" means
  deploying the same artifact twice and trusting that it is the same artifact.
  With aliases, test runs version 41, and promotion is `update-alias --name prod
  --function-version 41`. The bytes that were tested are the bytes that go live,
  by construction.
- **Rollback is one API call with a bounded blast radius.** `update-alias
  --function-version 40` is instantaneous and does not touch code, S3, or
  CloudFormation.
- **Canary is free once aliases exist.** `RoutingConfig` splits traffic by
  weight between two versions on one alias — 10% of release cuts on the new
  version for a day. Today there is no mechanism for that at all.
- **Half the surface to configure.** Four sets of secrets, env vars and alarms
  becomes two.

**Against — and this is the part that matters**

- **Lambda environment variables are pinned to a VERSION, not an alias.** A
  published version has one immutable environment. Two aliases on the same
  version therefore see *identical* env vars, so `RARC_ENV=prod` vs
  `RARC_ENV=test` cannot be expressed this way. This is the single most common
  thing people get wrong about aliases. The workarounds, in order of preference:
  1. Read the alias from `Lambda-Runtime-Invoked-Function-Arn` — the invoked ARN
     is qualified (`...:function:cutRelease:prod`) and the alias is the last
     segment. Implemented here in `internal/runtime.Invocation.Alias()` and
     `config.WithAlias()`.
  2. Pass the environment in through an API Gateway stage variable, so the
     *stage* carries it rather than the function.
  3. Keep configuration in Parameter Store under a per-alias path.
- **Function names appear in dashboards, alarms, IAM policies, saved CloudWatch
  queries, and people's muscle memory.** Deleting `cutReleaseTest` breaks all of
  them silently.
- **CloudWatch log groups are per FUNCTION, not per alias.** Prod and test
  invocations land in the same log stream. Fixable with a log field, but it is a
  real loss of separation and it will surprise whoever is on call.
- **It is a behaviour change during a migration.** The SOW forbids exactly this
  without approval, and it is the right call: if something misbehaves after the
  move, you want the only variable to have been the account.

**Recommendation.** Migrate four functions, like for like. But put the `live`
alias on each one *during* the migration — aliases are additive, they break
nothing, and they give the rollback story immediately. Then propose the collapse
as its own change with its own test window. `infra/20-lambda.yaml` does exactly
this, and `Invocation.Alias()` is already wired so the later change is small.

---

## 2. Secrets out of Lambda environment variables — **P0**

The SOW lists `GITHUB_TOKEN`, `SLACK_TOKEN`, Jira credentials and a signing
secret as Lambda environment variables.

A Lambda environment variable is readable by anyone holding
`lambda:GetFunctionConfiguration` — a much wider group than the one that should
hold a GitHub token with branch-write access on the news organisation's
repositories. It is rendered in the console, returned by the CLI, and it lands
in CloudFormation's stored template and in drift-detection output.

Proposed: environment variables hold a **Secrets Manager name prefix**; the
function resolves the actual values during INIT. The execution role is scoped to
`releasebot/*` and nothing else — see `infra/10-iam.yaml`.

**Against.** It adds a cold-start API call (mitigated: INIT is a one-time 10s
budget, and resolution is cached for the life of the sandbox) and a new failure
mode (mitigated: a secret that will not resolve fails INIT loudly rather than
failing on the first real release cut). The cost is a few dollars a month.

**This is a migration-time change, not a follow-up**, because the alternative is
copying production secrets into a new account's function configuration by hand,
which is the thing we would be trying to avoid later anyway.

### ★ The argument that actually wins this one: it is what makes rollback work

Security is the obvious case. The operational case is stronger, and it only shows
up when you try a rollback on a real deployment.

**A Lambda version freezes code AND configuration together.** So an alias
rollback rolls back both. Usually that is exactly what you want -- what shipped is
what was tested, environment included. But it means:

> **After you rotate a secret, every previously published version becomes an
> unusable rollback target**, because each one still carries the old value.

Proven in the sandbox on 2026-10-07: rolling `cutRelease`'s `live` alias from
version 5 to version 4 returned HTTP 500 immediately. Version 4 was published
before the signing secret was set, so it had none, and the function failed during
startup. Code rollback, configuration rollback, dead service.

Now put that on their cutover. Steps 6, 7 and 8 rotate the Slack signing secret
and the GitHub token. **Under env-var secrets, the moment that rotation lands,
the rollback plan silently stops working** -- and nobody finds out until the
rollback is needed.

Hold a *reference* in the environment instead and the version freezes the
reference, not the value. Rolling the code back leaves the secret where it is,
and the rollback plan survives the rotation that the cutover requires.

**Say it in the interview:** *"Config is frozen into the version, so an alias
rollback rolls config back too. That means rotating a secret quietly invalidates
every older version as a rollback target. Putting a reference in the environment
instead of the value fixes it -- which matters most at exactly the moment your
plan rotates credentials."*

---

## 3. Acknowledge Slack in under three seconds — **P0 if it is not already true**

Slack's slash command contract: respond `200` within **3 seconds**, or the user
sees `operation_timeout` and your work is invisible to them. A release cut that
calls GitHub three times will not reliably finish in three seconds, and
*already* will not when GitHub is slow — which is exactly when you want the bot
to be legible.

The correct shape is ACK immediately, then post the outcome to the
`response_url`, which stays valid for 30 minutes and 5 uses.

This repo implements that behind `RELEASEBOT_ACK_ONLY`. **The goroutine in
`handler.finish` is a placeholder, and deliberately a visible one**: in Lambda,
work started in a goroutine after the handler returns is frozen with the
execution environment and may never resume. The production shape is an async
self-invoke (`InvokeAsync`) or an SQS hop with a DLQ.

**Against.** If the current bot is synchronous and the team has lived with it,
this is a behaviour change and belongs behind approval — raise it with the
timing data from the dev account's existing CloudWatch duration metrics rather
than as an opinion.

---

## 4. OIDC instead of production-account repository secrets — **P0**

The SOW says *"Add production-account repository secrets ... to all four GitHub
Actions workflows."* That means minting IAM access keys for the production
account and storing them in GitHub.

Proposed: a GitHub OIDC provider and a deploy role per account, with the trust
policy pinned to this repository and `refs/heads/main` (or the `production`
environment). No long-lived key exists, so none can leak, and none has to be
rotated at step 7.

**The trust-policy detail that matters:** the `sub` condition must name the ref
or environment. `repo:org/releasebot:*` with `StringLike` lets *any* ref —
including a pull request branch pushed by anyone with write access — assume a
role that can rewrite production Lambdas. See `infra/10-iam.yaml`.

**Against.** It is a change to the deployment mechanism in the middle of a
migration, and a misconfigured trust policy blocks the pipeline. Mitigation:
stand it up in the production account *first*, while dev is still the system of
operation, so a failure costs a pipeline run and not a release.

---

## 5. A custom domain for the API — **P1, and the one with the best long-run payoff**

The invoke URL is `https://{apiId}.execute-api.{region}.amazonaws.com/{stage}`.
The `apiId` is account-specific, so **the URL necessarily changes at cutover**.
Everything holding that URL has to be edited in the cutover window: the Slack
slash command Request URL, the Slack interactivity URL, the Jira webhook, and
every runbook, bookmark and script that nobody has inventoried.

With `releasebot.<a domain we own>` in front, the cutover is a base-path mapping
change, the rollback is the same change in reverse, and nothing downstream knows
a migration happened. It also makes the *next* account move nearly free.

**Against.** A certificate, a DNS record and a Route 53 zone the CloudOps team
has to own. Not worth blocking the migration on — but it should be the first
thing proposed after it, because this migration will have just finished paying
the cost of not having it.

---

## 6. Idempotency on every GitHub write — **P0**

During the parallel-run window two live copies of the bot hold write credentials
to the same repositories. Slack retries. Jira retries aggressively and disables
webhooks that return non-2xx. Any of those can produce a duplicate action.

`CreateBranch` here treats GitHub's 422 "Reference already exists" as success
**only when the existing ref points at the same SHA**, and as a hard error
otherwise — a branch of that name at a different commit means two cuts raced,
and silently accepting it would hide a real problem. `OpenPR` reuses an open PR
rather than failing. Scenarios 3 and 5 of `make demo` are exactly this.

---

## 7. Graviton (`arm64`) — **P2, raise and drop**

~20% cheaper per GB-second and usually faster for Go. The change is one line in
the Makefile and one in the template.

**Against.** It changes the deployment artifact's architecture during a
migration whose entire premise is that the artifact does not change. Mention it,
cost it, and do it the quarter after. The Makefile keeps `GOARCH` as a variable
so the change stays a one-liner.

---

## 8. `provided.al2` → `provided.al2023` — **P1**

`provided.al2` is the previous generation. `al2023` is the current one, same
bootstrap contract, and `al2` will eventually reach end of support. The template
takes it as an allowed parameter value so the bump is an explicit, separately
approved change rather than something bundled invisibly into the migration.

**Against.** Different base image, different glibc. With `CGO_ENABLED=0` the Go
binary is static and this is a non-event — but "is a non-event" is a claim to
*test*, in the test stage, after acceptance, not during.
