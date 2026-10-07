# Interview prep — Sr. DevOps Engineer, CloudOps / News Group Digital

Grounded in the master resume. **Nothing in here claims something you cannot
defend under one follow-up question.** Where the req asks for something you do
not have, the script says the adjacent true thing and then moves to evidence.

---

## 0. Read the document you were sent

This is not a job description. It is a **statement of work for one project**,
with a technical implementation plan, deliverables, and acceptance criteria
already written. That tells you three things:

1. **The work is scoped and funded.** They are not hiring to explore a problem;
   they are hiring to close a ticket that has been open long enough to get its
   own project plan. "Final application identified for migration" — this is the
   last item on a migration programme that is nearly done, and somebody wants it
   finished.
2. **They have already thought about it.** Showing up with a different plan is a
   mistake. Showing up having read *their* plan, agreeing with it, and knowing
   which two phases are the risky ones is the winning move.
3. **It may well be a contract or SOW-based engagement.** "Resource Profile",
   "Delivery leadership", "operational handoff" is vendor language. Ask early
   and without hedging: *"Is this a contract engagement scoped to the migration,
   or a permanent CloudOps role where this is the first project?"* The answer
   changes what you optimise for, and asking it signals you read the document.

**The single highest-value thing you can say in the first ten minutes:**

> "I read the implementation plan. I'd run it in your phase order. The two
> phases I'd spend my planning time on are 4 and 6 — the parallel run, because
> two deployed bots with write access to the same repos is a split-brain risk
> rather than a safety net; and the cutover, because the invoke URL changes when
> the API id changes, so the cutover is really a list of URLs in Slack, Jira,
> and other people's runbooks. The Lambdas are the easy part."

That one paragraph tells them you read it, you have done it before, and you
know where it goes wrong.

---

## 1. Your 60-second opening

> "I've spent about twenty years on the delivery side — build, release, and the
> infrastructure underneath it. The last five were at Fidelity National
> Financial as DevOps/Cloud Architect, where I owned delivery automation and
> cloud architecture for two product lines: thirty-plus applications, five AWS
> accounts, ten-plus environment tiers. I replaced the legacy build system,
> moved the estate off Travis onto GitHub Actions, and built the blue/green
> deployment pattern we released on. I also ran the security programme that got
> long-lived AWS keys out of every pipeline — twenty-plus repos moved to OIDC
> federation with per-environment roles.
>
> That role ended in August, and since then I've been building a product of my
> own on a fully serverless AWS stack — Lambda, API Gateway, DynamoDB, Cognito —
> which is live on both app stores. Same discipline, modern stack, no team.
>
> This project is squarely the kind of work I do: account migration, Lambda and
> API Gateway, GitHub Actions, and a cutover that has to be reversible."

Then stop. Do not keep talking.

---

## 2. Req → your evidence

| They ask for | What you say — all defensible |
|---|---|
| AWS Lambda, API Gateway, IAM, **account migrations** | Five AWS accounts at FNF across two product lines; built the QA2/DR environment in a second region from the ground up, including Transit Gateway with the landing-zone team. CoachArc runs three isolated accounts (dev/qa/prod). |
| AWS CLI / profile tooling | Daily. Replaced AWS PowerShell modules with the AWS CLI in Windows EC2 bootstrap to fix chronic userdata init failures. |
| CloudWatch, security controls, least privilege | 40+ ALB hardening programme; OIDC with per-environment IAM roles; narrowed wildcard IAM actions to environment-scoped resources in a structured security audit. |
| GitHub Actions, repo secrets, pipelines, release management | Travis → GitHub Actions across 30+ applications; standardized pipelines; self-hosted Windows and Linux runners moved from public to private subnets. |
| **Rollback planning** | Build decoupled from deploy — a release promotes a tested artifact by ID instead of rebuilding. Blue/green that inspects the live traffic split and targets the idle stack. |
| Infrastructure automation / IaC | CloudFormation nested stacks (VPC, ASGs, launch templates, ECS, ALB, RDS, Cognito, Secrets Manager) proven through repeated deploy/destroy cycles in a purpose-built sandbox account; Terraform modules for Elastic Beanstalk. |
| **Slack applications and slash commands** | Slack-integrated deployment approvals for ECS blue/green at Cascade; Slack approval gates for the DisclosureSource Storefront at FNF. *(Adjacent — see §4.)* |
| **HMAC request verification, token auth** | Constant-time HMAC verification on public webhooks and Cognito JWT authorization on every authenticated route in CoachArc. This is a direct match — lead with it. |
| GitHub API / token access | Rulesets restricting production deploy rights; purged and archived a repo after finding committed keys. |
| **Go custom runtime, bootstrap, linux/amd64 packaging** | *See §4 — this is the gap. Handle it in the first ten minutes.* |
| Datadog | Evaluated, selected, installed and configured Datadog as the monitoring solution at William Hill. Sumo Logic SIEM across the AWS estate at Cascade. *(Adjacent — not Lambda extension depth.)* |
| Incident-ready runbooks, post-cutover support | 25+ approved change requests under formal change control; ran the monthly release train coordinating dev, QA, security and offshore teams. |
| Delivery leadership, stakeholder communication | Previously Director of DevOps. Weekly delivery reporting to leadership. |

---

## 3. The repo is your proof

You built `releasebot-migration` — a working model of their system. Use it, but
**frame it correctly**:

> "I didn't want to walk in with opinions, so I built a model of what the SOW
> describes — four Go Lambdas on a custom runtime, two API Gateway routes, Slack
> slash commands with signature verification, a Jira webhook, the CloudFormation
> for all of it, and the dual-account deploy workflow. It runs locally against
> fake GitHub and Slack so I could actually exercise the failure cases rather
> than reason about them."

Then, if they want detail, the three things it taught you:

1. **Packaging is where the time goes.** `provided.al2` wants an executable
   named exactly `bootstrap` at the root of the zip, `linux/amd64`, CGO off.
   Any of those wrong and you get `exec format error` at INIT and nothing more
   useful. There is a `verify-zip` make target that fails the build instead of
   discovering it in AWS.
2. **The signature check is the only thing between a public URL and `main`.**
   The three ways to get it wrong — comparing with `==`, verifying a
   re-serialised body, and no timestamp window — all pass a happy-path test.
   There are unit tests for each.
3. **The cutover is URLs, not infrastructure.** The invoke URL carries the API
   id, which is account-specific, so it necessarily changes.

**Do not oversell it.** If asked how long it took: say so plainly. It is a model
you built to prepare, not production code, and saying that makes everything else
you say more credible.

---

## 4. The gaps — scripts

### Go (the big one)

**Never claim to be a Go developer.** Your hands-on development is Python and
TypeScript, and one follow-up question would establish that.

> "I'm not a Go developer — my hands-on development is Python and TypeScript.
> What I've done for twenty years is own the build, packaging and release path
> for languages I don't author: Java and .NET enterprise applications, Node,
> and now Go. For this project, the Go surface is the runtime contract and the
> packaging, not feature development — the SOW says preserve existing behaviour,
> so the code shouldn't be changing. I did write a working model in Go to make
> sure I understood the bootstrap contract before talking about it, and the
> thing that surprised me is that on `provided.*` you own the runtime loop
> yourself: the long-poll against the Runtime API must have no client timeout,
> or an idle function crash-loops. If the work does turn into Go feature
> development, I'd want to be straight with you about ramp time."

That last sentence is why they will believe the rest of it.

### Slack apps and slash commands

> "I've built Slack into deployment approvals twice — ECS blue/green approvals
> at Cascade, and approval gates on the DisclosureSource Storefront at FNF. What
> I hadn't owned before is a Slack *app* with slash commands and the signing
> secret, so I built one to understand it. The three-second ACK rule is the part
> that shapes the design: a release cut that calls GitHub three times won't
> reliably finish inside it, so you acknowledge immediately and post the outcome
> to the `response_url`."

### Jira webhooks

> "I've lived on the Jira side of change control — twenty-five-plus CRs through
> formal CAB at FNF — but inbound Jira webhooks were new to me. The thing worth
> knowing is that Jira disables a webhook after repeated non-2xx responses, so
> 'not for me' has to return 200 and be ignored, not 400."

### Datadog depth

> "I selected and stood up Datadog at William Hill and ran Sumo Logic as SIEM at
> Cascade, so I'm comfortable with the platform. What I haven't run is the
> Datadog Lambda extension specifically. The approach I'd take on this migration
> is to not make observability depend on the layer resolving — structured JSON
> to CloudWatch regardless, with the extension on top."

### Degree

Do not raise it. If asked: *"Some college — no degree. I've been doing this
since 1998; the AWS Solutions Architect certification and twenty years of
delivery is what I bring."* Then move on. Do not apologise.

### "What have you been doing since August?"

There is no gap to explain — FNF ended 2026-08-26.

> "FNF ended in August when the role was eliminated. Since then I've been
> building and shipping a product of my own on a serverless AWS stack — it's
> live on the App Store and Google Play — and I published an AWS baseline audit
> tool. It's kept me hands-on with exactly this stack."

Do not pitch CoachArc as a business. **No revenue, users, or growth language** —
it is "a product I built to stay hands-on with a modern stack."

---

## 5. Technical questions you will get

### "How would you move a Lambda between AWS accounts?"

> "The function is the easy part — build the artifact once, put it in an S3
> bucket in the target account, `update-function-code`, publish a version, point
> an alias at it. What makes it a project is everything attached to it: the
> execution role and whatever inline policies grew on it over the years, the
> secrets, the log groups, and the API Gateway in front of it, which gets a new
> API id — so the invoke URL changes and every integration holding that URL has
> to move with it.
>
> The sequence I'd use is the one in your plan: baseline from the *live*
> account, not from the templates, because six years of console edits won't be
> in the templates. Build the new account. Deploy both in parallel from one
> artifact. Validate. Switch the URLs. Then — separately, behind an acceptance
> gate — decommission."

### "What breaks that people forget?"

Four, in order:

1. **The invoke URL.** Account-specific. It is the cutover.
2. **The Slack interactivity URL**, separately from the slash command URL. The
   command works, buttons silently do nothing.
3. **Implicitly-created CloudWatch log groups.** If Lambda creates them, they
   default to *never expire* and are not owned by the stack — so they survive
   the decommission and keep billing. Declare them.
4. **The GitHub token's identity.** It is not an AWS resource, so no stack
   deletion touches it. If the production account got a copy of the dev token,
   the dev token is now a credential with production reach in an account nobody
   is watching.

### "Walk me through Lambda versions and aliases." — *your opening to lead*

> "`$LATEST` is mutable; publishing a version freezes the code and configuration
> immutably. An alias is a named pointer at a version. The value is that your
> integration — API Gateway, in this case — points at the alias, so a deploy is
> publish-then-repoint and a rollback is repoint-to-the-previous-version. One
> API call, seconds, no rebuild.
>
> For this project I'd put a `live` alias on all four functions during the
> migration — it's additive, breaks nothing, and gives you the rollback
> immediately.
>
> The bigger idea I'd *propose* rather than do is collapsing `cutRelease` and
> `cutReleaseTest` into one function with `prod` and `test` aliases. Promotion
> becomes `update-alias` to the version test already validated, so the bytes
> that were tested are the bytes that go live, by construction — and weighted
> aliases give you canary for free.
>
> But there's a real catch, and it's why I'd propose it rather than just do it:
> **environment variables are pinned to a version, not an alias.** Two aliases
> on the same version see identical env vars, so `RARC_ENV=prod` versus `test`
> can't be expressed that way. You'd read the alias out of the invoked function
> ARN instead — the ARN is qualified, the alias is the last segment — or carry
> it in an API Gateway stage variable. And log groups are per function, so prod
> and test invocations land in the same group.
>
> On top of that, your SOW says preserve function names and behaviour unless
> approved — which I agree with. If something misbehaves after the move, you
> want the only variable to have been the account."

**This is your strongest single answer. It is technically correct, it shows you
know the non-obvious trap, and it ends by deferring to their constraint — which
is what separates a senior engineer from a clever one.**

### "How do you validate a Slack request?"

> "HMAC-SHA256 over `v0:timestamp:rawBody` with the signing secret, compared
> against the `X-Slack-Signature` header. Three things people get wrong, and all
> three pass a happy-path test: comparing with `==` instead of a constant-time
> compare, which leaks the prefix through timing; verifying a *re-serialised*
> body instead of the exact bytes Slack sent, so you must parse the form after
> verifying, never before; and omitting the timestamp window, which makes a
> captured request replayable forever — Slack's guidance is five minutes, and
> you want the absolute value so a future-dated capture doesn't slip through."

### "How do you keep secrets out of logs and tickets?"

> "Secrets don't go in Lambda environment variables if I can help it — anyone
> with `GetFunctionConfiguration` can read them, and they land in the stored
> CloudFormation template and in drift output. The env var holds a Secrets
> Manager reference; the function resolves it during INIT and the execution role
> is scoped to that one path. In logs I redact by key name rather than masking —
> a masked value still tells you the length. And API Gateway stays on `INFO`
> logging, never `DataTraceEnabled`, because that writes request bodies to
> CloudWatch and these request bodies carry Slack tokens."

### "What's the risk in the parallel run?"

> "Split brain. You have two deployed bots, both holding a GitHub token with
> branch-write rights, both reachable. If both are wired to Slack or Jira, one
> release cut produces two actions on real repositories. So the plan has to name
> which account is the system of operation at every moment, and the production
> copy is validated by direct invoke and its own test stage only, until cutover.
>
> I'd back that with three things: build once and deploy many, so the artifact
> is identical by construction; verify the deployed `CodeSha256` against the
> digest you built in each account, so 'both accounts run the same code' is a
> verified fact rather than a claim; and make every GitHub write idempotent, so
> a retry can't double-act."

### "How do you know the decommission is actually done?"

> "By verifying, not by deleting. A CloudFormation stack showing a
> `DeletionTime` is not a deleted stack — `DELETE_COMPLETE`, or `describe-stacks`
> failing with 'does not exist', is the only proof. I've been caught by that
> one. Then: implicit log groups aren't stack-owned and survive. An IAM role can
> be deleted while a user's access key still works. A versioned S3 bucket isn't
> emptied by deleting objects. And the credential that matters most — the GitHub
> token — isn't an AWS resource at all, so you rotate it and confirm GitHub
> rejects the old one."

---

## 6. Behavioural — use these, they are real

### "Tell me about a time a deployment went wrong." *(best story on the resume)*

> "On my own product, release 5.2.3 shipped to both app stores with zero tests
> having run — on a green pipeline.
>
> A harness-contract check sat above the unit tests in a sequential job. It
> returned a false positive and aborted the run before the tests, and because
> the job's exit status was clean, the pipeline was green. Green pipeline, no
> verification, released.
>
> The fix wasn't the false positive — that was a one-line bug. The structural
> problem was gate *ordering*: product verification has to run ahead of contract
> and hygiene checks, so a check about the harness can't mask a defect in the
> product. And I moved the full regression from a push trigger to a promotion
> gate, so a release can't reach production on a pipeline that skipped its own
> tests.
>
> What I took from it is that 'the pipeline was green' is a statement about the
> pipeline, not about the software."

That is the strongest thing you can say to a team whose whole job is release
automation.

### "Tell me about something you broke."

> "I took `www` down. I deleted a stack in a development account, and that stack
> owned production DNS records — it had been created there years earlier and
> nobody had written down what it owned. The site was unreachable until I
> recreated the records.
>
> The lesson wasn't 'be careful.' It was that before you delete anything, you
> enumerate what it *owns*, not what it's named — and a resource living in the
> wrong account is a latent outage that only shows up at deletion time. It's
> exactly why I'd want the decommission phase of this project to be a verified
> checklist behind an acceptance gate, not a cleanup task at the end of a sprint."

**Use this one.** It is honest, the lesson is directly relevant to Phase 7 of
*their* project, and it turns the worst thing on the list into the reason they
should trust you with the riskiest phase.

### "How do you handle a stakeholder who wants it faster?"

> "Separate what's reversible from what isn't. Deploying to the new account in
> parallel is reversible and can move as fast as they want. Switching the Slack
> URL is reversible in two minutes as long as the old stack is still there.
> Deleting the dev stack is the moment the rollback stops existing, and that one
> I'd hold behind sign-off. At FNF I ran twenty-five-plus change requests through
> formal CAB, so I'm comfortable having that conversation with a date attached
> rather than just saying no."

---

## 7. Ask them these

Pick four. They are diagnostic, not decorative.

1. **"Is this a contract engagement scoped to the migration, or a permanent
   CloudOps role where this is the first project?"** — ask this first.
2. **"Who owns the Slack app and the Jira webhook configuration? Are those
   changes I can make, or do they go through another team?"** — the cutover is
   those URLs. If a different team owns them with a two-week change window, the
   project plan is wrong and you will be the one who finds out.
3. **"Is the ReleaseBot source in a repo you control, with the existing
   workflows, or is some of it configured by hand in the console?"** — asks
   whether the baseline will match the templates without accusing anyone.
4. **"What does 'production acceptance' mean concretely — one release cycle, a
   soak period, a named sign-off?"** — the decommission gate is the only truly
   irreversible step and you want it defined before you start.
5. **"Is there a non-production GitHub org or a repo you're comfortable having a
   test bot cut real branches in?"** — you cannot validate a release bot without
   cutting a release. Proposing the shadow repo shows you have thought past the
   smoke test.
6. **"What happened to the previous migrations in this programme — anything that
   bit you that I should assume will bite here?"** — free risk register.
7. **"Who's on call for ReleaseBot after handoff, and what do they have today?"**
   — "operational ownership" is a deliverable in their own acceptance criteria.

---

## 8. Checklist for the day

- [ ] Re-read the SOW. Know the seven phase names in order.
- [ ] Be able to say phases 4 and 6 are the risky ones, and why, in two
      sentences each.
- [ ] Have the alias answer ready — including the env-var trap. It is your best.
- [ ] Have the 5.2.3 story ready. Ninety seconds, not five minutes.
- [ ] Have the `www` story ready, ending on the Phase 7 connection.
- [ ] Know your three honest gaps (Go, Slack apps, Datadog extension) and the
      adjacent-true-thing answer for each. **Volunteer the Go one early** —
      pre-empting it is strength; being caught by it is not.
- [ ] Have the repo open in a tab. Do not screen-share unprompted; offer it.
- [ ] Ask about contract vs permanent in the first five minutes.
- [ ] Say "I'd run it in your phase order" out loud at least once.

---

## 9. What not to do

- **Don't propose the four-functions-to-two change as a plan.** Propose it as a
  proposal, after acceptance, with the env-var trap named. Proposing an
  architecture change during a lift-and-shift reads as not having read the
  constraint.
- **Don't say "we should use Terraform."** They said *"infrastructure-as-code or
  repeatable automation where supported by the existing repository and account
  standards."* That sentence means: match what is there. Ask what is there.
- **Don't pitch CoachArc as a business.** It is a product you built to stay
  hands-on. No revenue, no users, no growth.
- **Don't mention Claude or ChatGPT in the FNF context.** Copilot was the
  sanctioned tool there.
- **Don't claim Go.** One follow-up question ends it, and it costs you the rest
  of the interview.
- **Don't lead with Jenkins.** Your positioning is GitHub Actions and AWS, which
  is what they use.
