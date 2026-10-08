# What was built, component by component

Every item below is deployed and was read back from the live account, not written
from memory. Where something is configured but not yet proven end to end, it says
so.

Sandbox account `826653639065`, `us-east-1`. Source: `github.com/vamsiatluri/releasebot-demo`.

---

## 1. Why a separate AWS account at all

The whole exercise is a rehearsal for moving an application between accounts. Doing
that rehearsal *inside* an account that already holds real systems would have
defeated the point twice over: the blast radius would be wrong, and "the target
account is empty" — the condition the migration actually starts from — would never
have been true.

So: a new member account under the existing organisation. It also made the teardown
test meaningful, because "is the account empty" is a question you can answer.

> ⚠️ An AWS account cannot be deleted, only **closed**, and closure has a 90-day
> tail. Creating one is close to one-way. Worth saying out loud before doing it.

---

## 2. Identity — five roles, each with one job

### `releasebot-github-deploy-826653639065` — the deploy identity

This is the one with the interesting design. **No AWS access key exists anywhere.**
GitHub proves its own identity at deploy time and receives credentials that expire
within the hour.

The trust policy is the part worth reading:

```
token.actions.githubusercontent.com:sub ∈ {
  repo:vamsiatluri@38706016/releasebot-demo@1410742872:ref:refs/heads/main
  repo:vamsiatluri@38706016/releasebot-demo@1410742872:environment:test
  repo:vamsiatluri@38706016/releasebot-demo@1410742872:environment:production
  …plus the three legacy-format equivalents
}
```

Three things in there are deliberate:

**It names the branch and the environments, not a wildcard.** `repo:owner/repo:*`
would let *any* ref — including a pull-request branch pushed by anyone with write
access — assume a role that can rewrite production.

**Both subject formats are trusted.** GitHub is migrating accounts to an
immutable-ID subject (`owner@ownerId/repo@repoId`). A policy written for the old
shape denies every assume-role on a migrated account, with an error that never says
why. This account is already on the new form.

**Every environment is listed.** A job that names a GitHub environment gets
`…:environment:<name>` rather than the ref form. Miss one and that single job's
assume-role is denied while the others succeed — which reads like a flaky pipeline
rather than a policy gap.

Its permissions are narrow by resource, and two of them matter:

```
iam:PassRole        → conditioned on iam:PassedToService = lambda.amazonaws.com
cloudwatch:PutMetricData → Resource "*" (the API takes no ARN)
                           conditioned on cloudwatch:namespace = ReleaseBot
```

`PassRole` without a service condition is a quiet privilege-escalation path: it lets
the deploy role attach *any* role it can name to something it controls.
`PutMetricData` genuinely cannot take a resource ARN — the namespace condition is
what keeps `"*"` from meaning "forge datapoints into any namespace in the account,
including the ones other teams alarm on".

### `releasebot-exec-cutrelease` and `releasebot-exec-mergeback` — the function identities

One per function family. Each can read its own secrets path and write its own logs.
Nothing else. If one were compromised, that list is the blast radius.

**These two roles are the reason the four-function layout is not redundant.** See §5.

### `ApiGatewayCloudWatchRole` ×2 — a per-region prerequisite

API Gateway request logging is gated on an **account-level** CloudWatch role — and
it turns out to be account-level *per region*. The first region had been configured
for weeks, so when the second region failed with *"CloudWatch Logs role ARN must be
set in account settings"* it read like a problem with the thing just built. It
wasn't.

That produced `infra/05-region-bootstrap.yaml`, split out of the global IAM stack,
because `AWS::ApiGateway::Account` is a per-account-per-region singleton while IAM
roles are global. Bundling them breaks the second region.

### `releasebot-slack-approve-ExecutionRole` — the approval endpoint's identity

Basic execution only. The GitHub token it uses is configuration, not an AWS
permission.

---

## 3. Compute — four functions, as specified

```
cutRelease                      provided.al2  512MB  30s  live → v8
cutReleaseTest                  provided.al2  512MB  30s  live → v9
releaseAutomationMergeback      provided.al2  512MB  30s  live → v7
releaseAutomationMergebackTest  provided.al2  512MB  30s  live → v9
slackApprove                    provided.al2  256MB  15s  (no alias — see §8)
```

Named exactly as their scope of work specifies. **Preserving the names was a
constraint, not an oversight** — their plan says preserve function names and
behaviour unless a change is approved, and that is the right call: if something
misbehaves after the move, you want the only variable to have been the account.

**`provided.al2` is a custom runtime**, which means the deployment artifact owns the
Lambda Runtime API loop itself — that is what `bootstrap` *is*. Two things about it
are easy to get wrong and hard to diagnose:

- The executable must be named exactly `bootstrap`, at the **root** of the zip,
  `linux/amd64`, CGO off. Any of those wrong gives `exec format error` or
  `Couldn't find valid bootstrap(s)` at INIT and nothing more useful. The build has
  a `verify-zip` target that fails locally rather than in AWS.
- The `/next` long-poll must have **no client timeout**. Lambda freezes the
  execution environment between invokes; a 30-second HTTP timeout on that call turns
  an idle function into a crash loop the moment traffic is sparse.

**Log groups are declared, not implicit.** A log group Lambda creates for itself is
set to *never expire* and is owned by no stack — so it survives the decommission and
keeps billing after the project is "done". This is the single most commonly missed
residue of a serverless teardown.

**Reserved concurrency is set.** A release-cutting bot should never be able to run a
thousand copies of itself; it also caps what a retry storm from Jira can cost.

---

## 4. Edge — one API, two stages

```
/beta/cut              POST   → stage variable cutFn
/beta/merge            POST   → stage variable mergeFn
/beta/health           GET    → stage variable cutFn
/beta/slack/interact   POST   → slackApprove (fixed)

prod stage: cutFn=cutRelease:live        mergeFn=releaseAutomationMergeback:live
test stage: cutFn=cutReleaseTest:live    mergeFn=releaseAutomationMergebackTest:live
```

### Why stages, and not two APIs

Their SOW specifies production and test **stages**, and that is the better design:
one deployment, one set of routes, and the environment carried by the stage. Two
APIs would mean two sets of routes that can silently drift apart, and twice the
surface to configure.

The stage variable is what makes it work. Note what it holds — `cutRelease:live`,
a function name and alias, **not an ARN**:

> ⚠️ API Gateway will not accept a stage variable holding a whole ARN. The URI must
> be a literal, well-formed ARN with the variable substituting only the
> function-name component. A bare `${stageVariables.cutArn}` is rejected at method
> creation with "Invalid lambda function ARN" — CloudFormation validates fine, and
> the API refuses it.

### The thing that makes this a migration project

```
https://0aohv0kx5k.execute-api.us-east-1.amazonaws.com/prod/beta/cut
        ^^^^^^^^^^ the API's own id
```

**The invoke URL contains the API id, which is account-specific.** Move accounts and
the address necessarily changes — so every system holding it has to move the same
day: the Slack slash-command URL, the Slack *interactivity* URL (a separate setting,
and the one people forget), the Jira webhook, and every runbook nobody inventoried.

I hit this three times in this build alone: the address changed on a rebuild, and
again in the second region.

**The recommendation that follows:** a custom domain in front of the API. Then the
cutover is a base-path remap, the rollback is the same remap reversed, and the next
account move is nearly free. That is written up as a post-acceptance proposal, not
something to bundle into the migration.

### The trap that cost the most time here

Adding `/beta/slack/interact` returned `Missing Authentication Token` — which sounds
like an auth failure and is not. **It is what API Gateway says when the route does
not exist on that stage.** A new resource creates a new deployment; existing stages
keep pointing at the old one until redeployed. `test-invoke-method` on the method
itself returned a correct `401` the whole time.

---

## 5. Aliases — what they buy, and why we kept both flows

Every function is fronted by an alias named `live`, pointing at a published,
immutable version.

```
deploy    = upload code → publish version N → point `live` at N
rollback  = point `live` at N-1        ← one API call, seconds, no rebuild
```

The API invokes **the alias**, never the bare function. Granting permission on the
unqualified ARN would let the API reach `$LATEST` — the one version no deploy gate
has ever looked at.

### The bigger idea, and why it stays a proposal

`cutRelease` and `cutReleaseTest` run **byte-identical code**. The obvious
simplification is one function with `prod` and `test` aliases: promotion becomes
`update-alias` to the version test already validated, canary comes free via weighted
routing, and there is half as much surface to configure.

The configuration half of that genuinely works, and it was proven in this account:

```
one function, version 5, two aliases
  alias prod → RARC_ENV reads "test"   ← identical, necessarily
  alias test → RARC_ENV reads "test"

invoked through :prod  →  {"ok":true,"env":"prod"}
invoked through :test  →  {"ok":true,"env":"test"}
```

Environment variables are pinned to a published **version**, not an alias, so two
aliases on one version see identical variables. The function resolves its own
environment from the invoked ARN instead — the ARN is qualified, and the alias is
its last segment.

**The permission half does not work, and that is the real argument against it:**

```
cutReleaseTest:prod → role releasebot-exec-cutrelease
cutReleaseTest:test → role releasebot-exec-cutrelease
```

A function has exactly one execution role whichever alias you came through, and no
IAM condition can see the alias. Collapse the pairs and that single role must read
**both** environments' secrets — the test path gains reach into the production
GitHub token. **Four functions with four roles give that boundary in IAM for free,
and the collapse spends it.**

### So both flows are supported, deliberately

| | Their plan (shipped) | The proposal (documented) |
|---|---|---|
| Functions | four, names preserved | two, prod/test aliases |
| Promotion | same artifact deployed twice, digests compared | move an alias |
| Permission boundary | **in IAM, per function** | in code, one role sees both |
| Status | **deployed and running** | proven in config, held for approval |

The code supports both: `runtime.Invocation.Alias()` and `config.WithAlias()` are
already in place and are a no-op in the four-function topology. Nothing has to be
rewritten if the collapse is later approved — and nothing deviates from their plan
if it is not.

> **The honest framing:** this is a security trade, not a free cleanup, and the
> person who owns the posture should make the call rather than the person who owns
> the pipeline.

---

## 6. Configuration and secrets

```
/releasebot/prod/default-owner        /releasebot/test/default-owner
/releasebot/prod/default-branch       /releasebot/test/default-branch
/releasebot/prod/slack-channel        /releasebot/test/slack-channel
/releasebot/prod/ack-only             /releasebot/test/ack-only
/releasebot/prod/github-token-ref     /releasebot/test/github-token-ref
```

The layering:

| What | Where | Why there |
|---|---|---|
| Which environment am I | the invoked ARN's alias | the only signal that can differ between two aliases on one version |
| Settings identical everywhere | environment variable on the version | frozen with the code — what shipped is what was tested |
| Settings that differ per environment | Parameter Store, per-environment path | outside the version, so a change needs no redeploy |
| **Secrets** | a *reference*, never the value | see below |
| Permissions | the function's execution role | cannot differ by alias |

### The finding that matters most in this whole project

A Lambda version freezes **configuration as well as code**. So an alias rollback
rolls the configuration back too. Proven here: rolling `cutRelease` from v5 to v4
returned HTTP 500 immediately, because v4 predated the secrets being set.

Which means:

> **Rotating a credential silently invalidates every older version as a rollback
> target**, because each one still carries the old value.

Now look at their cutover plan: steps 6, 7 and 8 rotate the Slack signing secret and
the GitHub token. **Under env-var secrets, the rollback plan stops working on the
exact day it is most needed**, and nobody finds out until they need it.

Holding a reference instead means the version freezes the reference, not the value.
Rolling the code back leaves the secret where it is, and the rollback survives the
rotation the cutover requires. That is a far stronger argument than "environment
variables are visible", though they are.

---

## 7. The pipeline

```
build ──► test ──► ⏸ approval ──► prod
  │         │                       │
  │         └── behaviour suite ────┘
  └── unit tests, vet, 13 behaviour checks locally
```

**Build once, deploy many.** The prod job has **no build step**. It deploys the
artifact the test job already exercised, and both jobs read the deployed
`CodeSha256` back and fail if it differs from what the build published.

Proven:

```
cutRelease      v8  rO9l+tiQLTDp1kjYNd5viQ1zGAh8sI/BilcX0JJJDJw=
cutReleaseTest  v9  rO9l+tiQLTDp1kjYNd5viQ1zGAh8sI/BilcX0JJJDJw=
                    ^^ byte-identical
```

"We shipped what we tested" is a check, not a hope.

**Dual-account capability exists** (`.github/workflows/deploy.yml`) for the
parallel-run phase, with the split-brain risk written into the pipeline rather than
into someone's memory — `fail-fast: false` so a production failure cannot cancel the
dev deploy still serving traffic, and a job whose only purpose is to state loudly
which account is the system of operation.

---

## 8. Approvals — two routes, one gate

### Route A — GitHub required reviewer (the primary)

```
environment: production
  rule: required_reviewers → [vamsiatluri]
```

The run **stops**. GitHub marks the job waiting, emails the reviewer, and the deploy
sits there until a person clicks approve. Nothing in the pipeline can route around
it: the gate is enforced by GitHub, not by a condition inside a workflow that anyone
who can edit the workflow could remove.

> ⚠️ Required reviewers need **Team/Enterprise on a private repo**. Pro is not
> enough — creating the rule is refused with a billing error. The repo is public,
> which is what makes the rule available. On a private repo without Team, the
> nearest equivalent is gating the job on a deliberate `workflow_dispatch` input:
> still a human decision, but enforced by the workflow rather than by GitHub.

### Route B — a Slack button (optional, and a deliberate weakening)

`POST /beta/slack/interact` → `slackApprove` → GitHub's pending-deployments API.

**This is not the same control, and it should not be described as one.** GitHub's
rule says *only these named people, signed in to GitHub*. A Slack button says
*anyone who can click in this channel triggers an approval, which GitHub records as
whoever owns the token this service holds*. Three consequences:

- the identity check moves from GitHub into a Lambda
- it defaults to delegating approval to Slack channel membership
- GitHub's audit log stops saying **who** decided

Three mitigations, none optional:

1. **Slack signature verified on every request** — HMAC-SHA256 over the exact bytes,
   constant-time compare, five-minute replay window. The endpoint is public; without
   this, anyone who learns the URL can deploy to production.
2. **The clicking user is checked against an explicit allowlist.** Channel
   membership is not authorisation. **An empty allowlist permits nobody** — read as
   "allow all", a misconfiguration would become an open door.
3. **Every attempt is audited** — who clicked, whether permitted, what happened.
   Never the signature, the body, or the token.

```
AUDIT {"event":"click","slack_user":"U0C7UFTGKNW","detail":"name=vamsi.d.atluri"}
AUDIT {"event":"rejected.not_approver","detail":"allowlist has 0 entries"}
```

It is a **separate CloudFormation stack** on purpose. Deleting that one stack
removes the alternate path and leaves GitHub's gate intact — far easier to reason
about, and to reverse, than a flag buried in the main API stack. If a reviewer
objects to the button, the answer is "delete that stack".

### Status, and the trap at the very last step

Clicking the real button in Slack now produces this:

```
AUDIT {"event":"click","run_id":"37826933854","slack_user":"U0C7UFTGKNW",
       "detail":"name=vamsi.d.atluri"}
AUDIT {"event":"approve.failed","run_id":"37826933854",
       "detail":"GitHub returned 403: Resource not accessible by personal access token"}
```

So every link in the chain we built works: Slack delivered the click, the signature
verified, the allowlist passed, the right run id travelled in the button, and the
GitHub API was called. **The last step is refused by GitHub itself.**

> ⚠️ **A fine-grained personal access token cannot review pending deployments.**
> The endpoint supports only OAuth app tokens and **classic** tokens with the `repo`
> scope — fine-grained PATs are not on the list.
>
> This one is nasty for two reasons. The error says *"Resource not accessible by
> personal access token"*, which reads as a permission you forgot to grant. And the
> token **already had** `Actions: read and write`, the permission you would reach
> for. The token is not under-scoped; it is **the wrong kind of token**, and no
> amount of adding permissions to it will ever work.

**Remaining work is one credential**, and deliberately not one I minted: a classic
token with `repo`, swapped in via `GitHubApprovalToken` on the `releasebot-slack-approve`
stack. The account's `gh` CLI token would work — it carries `repo` — but it also
carries `admin:org`, `delete_repo` and `admin:enterprise`, so putting it in a Lambda
environment variable to save a minute would be a bad trade.

**Proven:** Slack delivery, signature verification, replay window, allowlist refusal,
allowlist acceptance, correct run targeting, audit logging.
**Not proven:** the final GitHub call, pending the right token type.

### Why Socket Mode had to be turned off

Slack hid the Request URL field entirely, stating *"Socket Mode is enabled. You
won't need to specify a Request URL."* Socket Mode delivers interactions over a
persistent WebSocket to a long-running client — which a Lambda fundamentally cannot
be. It is the right choice behind a firewall or in local development, and the wrong
one the moment you want serverless.

---

## 9. Observability

**Logs** — structured JSON to CloudWatch. Secrets are redacted **by key name**, not
masked: a masked value still tells you its length. API Gateway stays on `INFO`, never
`DataTraceEnabled`, because that writes request bodies to CloudWatch and these
request bodies carry Slack tokens.

**Metrics** — the behaviour suite publishes its own results. Two series, deliberately:

> ⚠️ A CloudWatch alarm that names **no dimensions** matches only the metric
> published with no dimensions at all — it does not aggregate across them. The
> staleness alarm was written without dimensions against a metric only ever
> published *with* them: it could never fire and never clear. It sat red through
> four passing runs. The per-environment series drives the dashboard; a dimensionless
> rollup drives the alarms.

**Alarms** — three, and the third is the one people forget:

| | |
|---|---|
| `releasebot-contract-critical-failure` | a critical behaviour check failed |
| `releasebot-contract-not-reporting` | **nothing has checked in three hours** |
| unauthorised-request spike | the public endpoint being probed, or a half-landed rotation |

The staleness alarm uses `TreatMissingData: breaching`, because **a verification
pipeline that stops running looks exactly like a healthy system** — every other
dashboard stays green and the absence is invisible.

**Dashboard** — one CloudWatch screen, behaviour on the left, availability and
infrastructure on the right.

**Canary** — one Synthetics canary on the health endpoint, `EnableCanary: false` by
default. A canary is a *monitor*, not a test suite, and it bills per run forever.

---

## 10. The verification layer

**The behaviour suite** — thirteen checks, one definition, three targets: free on a
laptop with no AWS, as the gate after every deploy, and on a schedule. Every check
carries a *why*, so the report reads to a non-engineer.

Four of them are not obvious: a forged signature must be refused; a replayed request
must be refused; the same release cut twice must succeed quietly; and a Jira event
that isn't relevant must return **200**, because Jira disables a webhook that keeps
erroring.

**The baseline tool** (`cmd/baseline`) — reads a *running* account into JSON,
reviews it, diffs two accounts, and generates a starting-point template. It never
reads a secret value. The classifier deliberately over-redacts.

`compare` is the point: during the parallel run the evidence you want is not
"production passed" but **"production and dev gave identical answers"**.

**The parity run** — the full loop executed end to end, differences closing
**15 → 8**, blockers **7 → 0**, with all eight remaining ones deliberate. Three of
five passes failed before creating anything, which is the loop working.

---

## 11. Their seven phases, and where each stands

| Phase | What it asks | Status here |
|---|---|---|
| **1 · Access and baseline** | inventory the running account | `cmd/baseline` captures and reviews the live account, not its templates |
| **2 · AWS infrastructure** | roles and functions | four roles, four functions, declared log groups, packaging check |
| **3 · API and configuration** | routes, stages, config, secrets | both routes, both stages, Parameter Store, secrets as references |
| **4 · CI/CD parallel run** | dual-account deploy | built; build-once-deploy-many with digest verification, split-brain guard |
| **5 · Validation** | production-readiness evidence | behaviour suite ×3 targets, metrics, alarms, cross-account compare |
| **6 · Cutover** | switch and rotate | checklist written; rotation/rollback trap identified |
| **7 · Decommission** | delete and verify | `sandbox-down.sh` — deletes, then *proves* it; caught an orphan log group |

---

## 12. Proven vs not

**Proven, by running it:**
build-once-deploy-many with identical fingerprints · OIDC with no stored key ·
the approval gate holding a real deploy · the behaviour suite against four live
endpoints · teardown verified empty · rebuild from empty in one command · the
capture/compare/generate loop closing to zero-unexplained · secret rotation making
an old version unusable · Slack notification from the pipeline · Slack signature
verification and allowlist refusal.

**Not proven:**
the final GitHub call in the Slack approval path — refused by GitHub because a
fine-grained token cannot review pending deployments; everything before it is proven ·
the mutating behaviour checks
against a real GitHub repository (they run against a local stand-in; the real
version needs a shadow repo) · anything at NBC's scale, on their account, with
their integrations.

That last line is the honest one. This is a faithful model built from their scope
of work — it is not their system, and the first week of the real engagement would be
spent finding out where the model is wrong.
