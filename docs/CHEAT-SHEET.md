# One page — have this open during the call

## Know who you're talking to

**Vendor / recruiter screen** → availability, rate structure, "yes, I've done AWS
account migrations, Lambda, API Gateway, GitHub Actions." 60-second pitch. **No
deep technical content** — they can't score it and it burns the call.

**Hiring manager / CloudOps** → everything below.

---

## The frame: they are buying delivery of a written SOW in 12 weeks

Not a career, not a culture fit. Every answer should sound like someone who has
landed fixed-scope work before. **Agree with their plan.** Say *"I'd run it in
your phase order"* out loud at least once.

---

## Land these seven

**Opening (first 10 minutes)**

1. **"I'd run it in your phase order. The phases I'd spend planning time on are
   4 and 6."** Parallel run and cutover.
2. **The freeze calendar.** Midterms 3 Nov, then year-end. Across 12 weeks that
   could remove 4–5 of your usable change windows. Front-load phases 1–4 into
   the election freeze — none of it touches the production system of operation.
   Target cutover for the back half of November.
3. **The Phase 7 collision.** Their decommission gate is "production
   acceptance" = a soak of ~2 release cycles. If they release monthly that falls
   outside 12 weeks. Agree in week 1 which it is: compress validation, hand off a
   documented runbook with a named executor, or extend.

**Technical (when asked)**

4. **The cutover is URLs, not infrastructure.** The invoke URL carries the
   account-specific `apiId`, so it necessarily changes. Slack slash command URL,
   Slack **interactivity** URL (separate — easy to miss), Jira webhook, and every
   runbook holding the old one.
5. **Parallel run is a split-brain risk, not a safety net.** Two bots, both with
   branch-write GitHub tokens. Exactly one is the system of operation at any
   moment. Back it with: build once / deploy many, verify the deployed
   `CodeSha256` against the digest you built in each account, and make every
   GitHub write idempotent.
6. **Aliases.** Put a `live` alias on all four during the migration — additive,
   breaks nothing, rollback becomes one `update-alias`. *Propose* collapsing the
   prod/test pairs after acceptance. **The trap: Lambda env vars are pinned to a
   published VERSION, not an alias** — two aliases on one version see identical
   env vars, so `RARC_ENV=prod` vs `test` can't be expressed that way. Read the
   alias off the invoked function ARN instead. Log groups are per function too.
7. **Decommission is verification, not deletion.** A `DeletionTime` is not a
   deleted stack. Implicit log groups are never-expire and not stack-owned.
   And the GitHub token isn't an AWS resource, so no stack deletion touches it.

---

## Pre-empt these three — volunteer, don't get caught

| Gap | Say |
|---|---|
| **Go** | "I'm not a Go developer — my hands-on is Python and TypeScript. I've owned the build, packaging and release path for languages I don't author for twenty years. Here the Go surface is the runtime contract and packaging, not feature development. If it turns into Go feature work I'd want to be straight with you about ramp." |
| **Slack apps** | "I've built Slack into deployment approvals twice — ECS blue/green at Cascade, approval gates at FNF. A Slack *app* with slash commands and a signing secret was new, so I built one." |
| **Datadog extension** | "I selected and stood up Datadog at William Hill, Sumo Logic as SIEM at Cascade. Haven't run the Lambda extension. I'd keep structured JSON going to CloudWatch regardless, extension on top." |

---

## Two stories — 90 seconds each, not five minutes

**"A deploy went wrong."** Release 5.2.3 shipped to both app stores with zero
tests having run, on a green pipeline. A harness check sat above the unit tests
in a sequential job, returned a false positive, and aborted the run before them —
clean exit status, green pipeline. Fix wasn't the false positive; it was gate
*ordering*, plus moving full regression from a push trigger to a promotion gate.
**"'The pipeline was green' is a statement about the pipeline, not the software."**

**"Something you broke."** Took `www` down — deleted a stack in a dev account
that owned production DNS records, created there years earlier, nobody had
written down what it owned. Lesson: enumerate what a resource *owns*, not what
it's named. **Land it on their Phase 7**: "which is exactly why I'd want
decommission to be a verified checklist behind an acceptance gate."

---

## Ask them

1. How does the change-freeze calendar run around election coverage and year-end?
2. What's your release cadence, and what does "production acceptance" mean
   concretely — one cycle, two, a soak period?
3. Who owns the Slack app and the Jira webhook config — can I make those changes,
   or does another team?
4. Is there a repo you're comfortable having a test bot cut real branches in?
5. Is there a path to extension, and what would it depend on?

---

## Do not

- **Don't claim Go.** One follow-up ends it.
- **Don't assert what ReleaseBot does.** Say *"the scope of work pins it down to —"*
  You're inferring from their document. Stating inference as inference is a strength;
  being wrong about their system is not.
- **Don't lead with the repo.** Let it come out when a technical question lands:
  *"I built a model to make sure I understood the bootstrap contract before
  talking about it."* Offer; don't screen-share unprompted.
- **Don't pitch the P1 proposals as "what I'd do next."** You won't be there.
  They're handoff recommendations.
- **Don't say "we should use Terraform."** They said match existing repository and
  account standards. Ask what's there.
- **Don't pitch CoachArc as a business.** No revenue, users, growth. "A product I
  built to stay hands-on with a modern stack."
- **Don't mention Claude or ChatGPT in the FNF context.** Copilot was sanctioned there.
- **Don't lead with Jenkins.**
- **Don't give a rate before you know W2-via-vendor vs C2C vs 1099.**
