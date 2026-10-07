# 90-day delivery plan

A three-month contract to deliver a seven-phase SOW. This maps the phases onto
weeks, names what each one is actually blocked on, and flags the two calendar
problems that will decide whether it lands on time.

**The engineering is not the long pole.** Four Lambdas, two routes and a deploy
workflow is maybe three to four weeks of build. The other eight weeks are access
latency at the front, soak time in the middle, and other teams' change windows at
the end. Plan against those and the contract finishes early; plan against the
build and it finishes late.

---

## The two calendar collisions — raise these in the interview

### 1. The midterm elections are **Tuesday 3 November 2026**

This is **NBC News**. A news organisation's release automation during national
election coverage is the single worst thing to be cutting over, and there will
be a change freeze around it — probably opening a week or two before and closing
some days after, depending on how long the results run.

A contract starting mid-to-late October therefore spends its first two to four
weeks inside a window where **no production change is going to be approved**.
That is not lost time if it is planned for: phases 1 through 4 are
discovery, build in a *new* account, and pipeline work, none of which touch the
production system of operation. It is only fatal if you had Phase 6 scheduled
there.

### 2. The year-end freeze

Mid-December to early January is a near-universal freeze. A contract starting
20 October runs to roughly 19 January — so the back half of the engagement has a
three-to-four week hole in it exactly where cutover and decommission live.

**Together those two can remove four or five of your twelve weeks of available
change windows.** Nobody else interviewing will have noticed. Say it out loud:

> "Looking at the calendar — you've got midterms on 3 November and then the
> year-end freeze. If the contract runs roughly late October to late January,
> the change windows I'd actually be able to cut over in are mid-November to
> mid-December, and then January. I'd want to front-load discovery and the
> parallel build into the election freeze, where none of it touches production,
> and target cutover for the back half of November. Is that how your freeze
> calendar actually works?"

That question does more for you than any technical answer in the interview.

---

## Week by week

Weeks are relative to start. The phase names are theirs.

| Wk | Phase | Work | Blocked on — the real risk |
|---|---|---|---|
| 1 | 1 | Access to both AWS accounts, GitHub org, Slack admin, Jira admin, Datadog. Baseline the **live** dev account to files. | **Access provisioning.** At a company this size this is the single most likely thing to eat two weeks. Request everything on day one, in one ticket, including the things you won't need until week 8. |
| 2 | 1 → 2 | Finish the baseline diff. Inventory every integration holding the invoke URL. Build IAM + execution roles in prod. | Finding the owner of the Slack app and the Jira webhook. These are often not CloudOps. |
| 3 | 2 | Four Lambdas in prod, packaging proven, log groups declared, alarms. Diff prod config against the week-1 baseline. | Nothing external. This is the week that actually feels like engineering. |
| 4 | 3 | API Gateway, routes, both stages, secrets. **Publish the new invoke URL and start socialising it.** | Secrets handling sign-off if you're moving them out of env vars. |
| 5 | 4 | Dual-account deploy. Build once, deploy many, verify `CodeSha256` in each account. Split-brain boundary written into the pipeline. | OIDC trust policy, if you've proposed it. Stand it up in prod while dev is still the system of operation. |
| 6–8 | 5 | Validation. Full release cycles against a shadow repo through the prod test stage. Evidence package built as you go, not at the end. | **A repo you're allowed to cut real branches in.** Ask for this in week 1, not week 6. |
| 9 | 6 | Cutover. Test surfaces first, then prod Slack slash URLs, then interactivity, then Jira, then secret and token rotation, then one full real cycle. | **A change window, plus Slack and Jira admins available in it.** Book it in week 5. |
| 10–11 | — | Acceptance soak. Two release cycles or two weeks, whichever is longer. Dev stack stays deployed and untouched — this is the rollback. | Their release cadence. If they release monthly, "two cycles" is two months and the gate does not fit the contract. **Find this out in week 1.** |
| 12 | 7 | Decommission, each deletion verified. Rotate every credential that ever had production reach. Runbooks, handoff, operational ownership. | Sign-off. |
| 13 | — | Buffer. Assume you need it. | |

---

## The one structural problem with the contract

**Their Phase 7 gate may not fit inside your contract.**

The decommission gate is "production acceptance", which properly means a soak of
two release cycles or two weeks. If their release cadence is monthly, two cycles
is two months — and with a cutover in week 9, Phase 7 lands outside the
engagement.

That is not a reason to shorten the gate. It is a reason to name it on day one
and agree what happens:

- **Option A — cut over earlier.** Compress validation, cut over in week 6 or 7,
  leave a real soak inside the contract. Only works if the change windows allow
  it, which the freezes above may not.
- **Option B — hand off Phase 7.** Finish through acceptance, leave the
  decommission as a fully documented, pre-approved runbook with the verification
  steps written and the credentials inventory complete, and name the person who
  executes it. The dev stack stays up until they do.
- **Option C — extend.** Fine, but it should be a decision made in week 2, not a
  discovery in week 12.

**Bring this up in the interview.** Not as a problem — as the first thing you
noticed:

> "One thing I'd want to agree up front: your Phase 7 gate is production
> acceptance, and a real soak is two release cycles. Depending on your release
> cadence, that could land the decommission outside a three-month engagement. I'd
> rather we decide in week one whether I'm compressing validation, or whether
> decommission is a documented handoff with someone named to execute it — than
> have it be a surprise in week twelve."

A hiring manager for a fixed-term contract has been burned by exactly this. You
naming it unprompted is the strongest signal available that you have delivered
fixed-scope work before.

---

## What changes about the design proposals

On a permanent role, `docs/DESIGN-PROPOSALS.md` is a roadmap. On a three-month
contract **you will not be there for the follow-up quarter**, so pitching P1 work
as "what I'd do next" is pitching work you won't do.

Re-frame it:

- **P0 stays P0.** Secrets out of env vars, OIDC, idempotency, the Slack ACK —
  these are inside the migration and inside the contract.
- **Everything else becomes a written handoff recommendation**, not a roadmap.
  The alias collapse, the custom domain, Graviton, `al2023`: each one written up
  with the argument for, the argument against, and the trap — so the team can
  decide after you've gone. `DESIGN-PROPOSALS.md` is already in that shape.
- **Do not create dependencies on yourself.** The deliverable of a contract is a
  system someone else operates. Every "I'd handle that" is a liability; every
  runbook entry is an asset.

Say this out loud too. It is the difference between a contractor who gets
extended and one who doesn't.

---

## How to be the one they extend

Three-month contracts at large media companies extend or convert regularly, and
the behaviour that causes it is predictable:

1. **Finish a phase early and say so with evidence.** Their acceptance criteria
   are already written; meet them visibly.
2. **Write the handoff from week one, not week twelve.** `RUNBOOK.md` should be
   accumulating from the first incident, not assembled at the end.
3. **Surface risk early and in writing.** The freeze calendar, the soak gate, the
   access latency — all of these are better raised in week one as a plan than in
   week ten as an excuse.
4. **Leave no dependency on yourself.** Paradoxically this is what gets you asked
   back: a team that can operate what you built trusts you with the next thing.
5. **Find the next thing while you're there.** "Final application identified for
   migration" means this programme is ending — so the next work is whatever the
   newly-consolidated production account needs. Observability consolidation,
   pipeline standardisation, the custom domain. Notice it, write it down, hand it
   over. That is how a three-month contract becomes a six-month one.

---

## Rate and terms — think about this before the call

You will be asked your number early; for contracts it is usually the vendor's
screening filter rather than a negotiation.

Things to have straight before you are asked:

- **Work out your own floor first**, from your actual runway, and do not go into
  the call without it. This is a rate conversation, not a salary conversation,
  and the two are not the same number.
- **A contract rate is not a salary divided by 2,080.** There are no benefits, no
  PTO, no employer payroll contribution, and a hard stop at twelve weeks with no
  severance. Contract rates price above the salary-equivalent hourly for exactly
  those reasons, and that is a normal thing to say out loud.
- **Know which structure you are being offered** — W2 through a staffing vendor,
  C2C, or 1099 — because the same take-home needs a different headline number in
  each. Ask directly.
- **The vendor's bill rate to NBC is higher than your rate.** That margin is
  normal and not yours to fix, but it does mean "that's the budget" usually means
  "that's the budget *after* our margin".
- **Ask about extension and conversion explicitly.** "Is there a path to
  extension or conversion, and what would it depend on?" It costs nothing and the
  answer tells you how to spend twelve weeks.
- **If pressed before you are ready:** *"I'd want to understand the scope and the
  structure — W2 or C2C — before I give you a number. What range is the role
  budgeted at?"* Then stop talking.

Given the twelve weeks are fixed, the questions worth negotiating alongside rate
are **start date** (a week later can skip an access-provisioning hole) and
**whether Phase 7 is in scope**, which is a real scope question with a real cost.
