# Their plan, explained

The document they sent is a **statement of work for one project**, with the phases,
deliverables and acceptance criteria already written. This is that plan in plain
language, so you can talk about any part of it without rehearsing jargon.

---

## What the project actually is

They have an application called **ReleaseBot** (nicknamed "Robbie") that automates
part of their software release process. It currently lives in a **development**
AWS account called `msnbc-dev`. They want it moved into their **production**
account, `msnbc`.

Two sentences from their document carry most of the meaning:

> *"It is the final application identified for migration to the AWS msnbc
> production account."*

This is the **last item in a migration programme that is nearly finished**.
Everything else has already moved. Somebody wants this closed out.

> *"Preserve existing function names and application behavior unless a change is
> required and approved."*

**The application itself is not changing.** This is a move, not a rewrite. That
single line tells you what kind of engineer they're hiring: not someone to build
features, someone to relocate a working system without breaking it.

**Say it like this:** *"It's the last application in a migration programme, and
the brief explicitly says preserve the behaviour. So it's a relocation job — the
risk isn't in the code, it's in everything the code is connected to."*

---

## What ReleaseBot does

Their document never says outright, but the scope of work pins it down: two
commands named "cut" and "merge", a GitHub key, a Slack key, Jira credentials,
and Slack slash commands.

That's a bot that **cuts a release branch when someone asks for one in Slack, and
later opens the pull request that merges it back** — the two moments in a release
where a person doing it by hand gets it wrong.

⚠️ **State that as a reading, not a fact.** *"The scope of work pins it down to…"*
Being confidently wrong about their system is worse than saying you inferred it.

---

## The shape of the whole thing

Seven phases, but really **four moves**. This is the mental model to hold:

| Move | Phases | In one line |
|---|---|---|
| **Find out what's actually there** | 1 | Not what the documents say — what the running account really contains |
| **Build a copy** | 2, 3 | Stand up an equivalent in the new account |
| **Run both and prove they match** | 4, 5 | Deploy to both, and gather evidence they behave identically |
| **Switch, then clean up** | 6, 7 | Point everything at the new one; remove the old one — and verify it's really gone |

**Say it like this:** *"It's four moves: inventory what's really there, build a
copy, run both until you can prove they match, then switch and clean up."*

---

## The seven phases

### Phase 1 — Access and baseline

**What they wrote:** confirm credentials; inventory the current roles, variables,
workflows, integrations and monitoring.

**What it means:** get into both accounts, then write down exactly what exists
today. Not just in AWS — also the Slack app, the Jira webhook, the GitHub key,
and the monitoring.

**What goes wrong:** people take the inventory from the **blueprints** instead of
the **building**. On an application that's been running for years, somebody has
changed something by hand during an incident and never put it back in the
blueprint. That difference is invisible until the copy behaves differently.

**The other half people skip:** most of what has to change at cutover isn't in
AWS at all. It's the web address stored in Slack's settings, the one stored in
Jira's settings, and the ones in runbooks nobody has read in two years.

**Say it like this:** *"I'd take the baseline from the live account, not the
templates — years of hand edits won't be in the templates, and that gap is the
project. And I'd inventory the things that aren't in AWS at all, because that's
what the cutover actually consists of."*

> **Real risk on a 12-week contract:** getting access at a company this size can
> eat two weeks. Request everything on day one, including things you won't need
> until week eight.

---

### Phase 2 — AWS infrastructure

**What they wrote:** create four permission identities and four small programs in
the new account.

**What it means:** build the compute side of the copy. Four programs because the
system has two jobs, each with a live and a test version.

**What goes wrong:** the packaging. The programs are written in Go and packaged a
particular way, and if the package is even slightly wrong the program fails to
start with an error message that tells you almost nothing. It's an afternoon of
confusion that a packaging check catches in a second.

**The one people miss:** if you don't create the log books yourself, AWS creates
them for you — set to keep everything forever, and belonging to nothing. Which
means they survive Phase 7 and keep costing money after the project is "done."

**Say it like this:** *"Two things here: the packaging has to be exactly right or
it fails at startup with an unhelpful error, so I'd prove a trivial deploy end to
end before touching anything real. And I'd declare the log groups rather than
letting AWS create them, because the ones AWS creates never expire and aren't
owned by the stack — so they outlive the decommission."*

---

### Phase 3 — API and configuration

**What they wrote:** create the web routes and the two environments; set the
GitHub key, Slack key, Jira credentials, the environment setting, and the
monitoring settings.

**What it means:** build the front door, and load the settings and passwords.

**What goes wrong — and this is the big one:**

> **The new front door has a different web address.** The address contains the
> account's ID, so a new account necessarily means a new address.

That is the entire cutover, and it's known on day one of Phase 3. Write the new
address down then and start telling people — don't discover it during the switch.

**The configuration trap:** copying production passwords by hand into the new
account's settings. It's the cheapest moment to put them somewhere proper
instead, because nothing is live yet.

**Say it like this:** *"The new front door gets a new address because the address
has the account in it. I'd publish that address in week four and start
socialising it, because every system holding the old one has to be updated on
cutover day."*

---

### Phase 4 — CI/CD parallel run

**What they wrote:** add the production account's credentials and a second deploy
step to all four pipelines, while still deploying to development.

**What it means:** for a while, both copies are live and getting every update.
That's the safety net — the old one still works, so you can go back.

**What actually goes wrong:** people hear "parallel run" and think safety. It's
also a **risk**:

> Two live copies of the bot, both holding a key that can write to the same
> code repositories. If both are connected to Slack or Jira, one person's request
> can produce two actions on real code.

So the plan has to say, at every moment, **which one is the real one**. The other
is tested by poking it directly, never by being connected to Slack.

Three things that make it safe: build the package **once** and send the same one
to both accounts; **check afterwards that both accounts are running identical
code** rather than assuming; and make every action **safe to repeat**, because
Slack and Jira both retry.

**Say it like this:** *"Running both isn't automatically a safety net — two bots
with write access to the same repos is a split-brain risk. I'd build the package
once, deploy the same one to both, verify afterwards that both are running
identical code, and be explicit about which account is the live one at any
moment."*

---

### Phase 5 — Validation

**What they wrote:** test the two commands; verify Slack messages, Jira events,
GitHub access, logs, metrics and alerts.

**What it means:** gather the evidence that the copy works. Their deliverable for
this phase is literally the words *"production-readiness evidence."*

**The honest problem:** the only real test of a release bot is cutting a release
— and nobody wants to cut a real one from an unproven account.

**The answer:** ask for an **empty throwaway repository** in the same GitHub
organisation, set up the same way, and run full release cycles through it. Real
GitHub, real key, real Slack channel, zero production impact.

**Say it like this:** *"You can't validate a release bot without cutting a
release. I'd ask for a throwaway repo in the same org with the same branch
protection and run full cycles through it — real everything, no production
impact. That's a week-one ask, not a week-six one."*

> **The strongest version of this phase:** run the *same* checks against both
> accounts and compare. The evidence you want isn't "production passed," it's
> **"production and development gave identical answers."**

---

### Phase 6 — Cutover

**What they wrote:** update the Slack addresses for production and test; replace
the official passwords; remove the parallel deploy steps; run a full release
cycle.

**What it means:** flip the switch. And notice what it actually consists of —
**editing a handful of web addresses in other people's systems.** Which is good
news: each one is a single field, so each one is also a single-field undo.

**Order matters:** test surfaces first, then the live Slack command, then the
Slack *button* handler (a separate setting — easy to forget, and the symptom is
that commands work and buttons silently do nothing), then Jira, then the password
rotations, then one full real release.

**Keep the old one running.** It stays deployed and untouched throughout, which is
what makes this a rollback rather than a re-migration.

**Say it like this:** *"The cutover is a handful of addresses in other people's
systems, which means every step is also a one-field rollback — as long as the old
account is still there. I'd capture every current address beforehand, because the
moment you need them is the moment the console you'd look them up in is the
broken one."*

⚠️ **The thing to raise that nobody else will:** rotating a password at this step
quietly breaks the ability to roll back to any earlier version, because each
version carries the password it was frozen with. Three of their steps rotate
credentials.

---

### Phase 7 — Decommission

**What they wrote:** delete the old programs and front door; revoke or rotate the
development credentials; update the runbooks; hand over.

**What it means:** remove the old system — and this is the only genuinely
irreversible step, because it's the moment the rollback stops existing.

**What goes wrong:** "deleted" gets assumed instead of verified.

- A console that shows a deletion time is not proof the thing is gone.
- Log books nobody created on purpose survive and keep billing.
- A permission can be removed while an old key still works.
- **The GitHub key isn't an AWS thing at all**, so nothing you delete in AWS
  touches it. If the new account got a copy of the old key, that key is now a
  credential with production reach sitting in an account nobody watches.

**Say it like this:** *"Decommissioning is a verification job, not a delete job.
A console saying it's gone isn't evidence. And the credential that matters most —
the GitHub key — isn't an AWS resource, so no amount of deleting stacks touches
it."*

> ⚠️ **The structural problem to raise in week one:** their gate for this phase is
> "production acceptance," which properly means a soak of a couple of release
> cycles. If they release monthly, that lands outside a twelve-week contract.
> Agree up front whether you compress validation, hand Phase 7 over as a written
> runbook with a named owner, or extend.

---

## What they're buying, in their own words

Their acceptance criteria, translated:

| They wrote | Means |
|---|---|
| Production infrastructure build | The copy exists and is configured |
| CI/CD migration | All four pipelines can deploy to the new account |
| Testing and production readiness | You can *show* it works — evidence, not assurances |
| Cutover and decommission | Everything points at the new one; the old one is gone |
| Documentation and handoff | Someone else can run it after you leave |

That last row is a deliverable, not an afterthought — and on a three-month
contract it's the one that decides whether they'd have you back.

---

## If they say "walk me through how you'd approach this"

Ninety seconds, in this order:

1. **"I'd run it in your phase order."** Say it early.
2. **The four moves** — inventory, build a copy, run both until they provably
   match, switch and clean up.
3. **"The two phases I'd spend planning time on are 4 and 6."** Parallel run,
   because two bots with write access is a risk not a net. Cutover, because the
   address changes, so it's really a list of URLs in other people's systems.
4. **The calendar.** Midterms on 3 November, then the year-end freeze — front-load
   phases 1 to 4 into the freeze, because none of that touches the live system.
5. **Stop.** Let them ask.
