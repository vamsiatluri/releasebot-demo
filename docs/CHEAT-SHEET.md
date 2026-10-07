# One page — have this open during the call

## The reframe: this is not a development job, and their own document says so

Read their SOW again:

> *"Preserve existing function names and application behavior unless a change is
> required and approved."*

**The code is explicitly frozen.** Nobody is asking you to write Go. Look at
what the deliverables actually are: infrastructure build, CI/CD migration,
testing and production readiness, cutover and decommission, documentation and
handoff. And one of the six rows in their own skills table is **Delivery
leadership** — technical planning, dependency management, change coordination,
test evidence, stakeholder communication, operational handoff.

Even the runtime row says **"troubleshooting," "packaging," "environment
configuration"** — operator words, not developer words. They did not write
"develop in Go."

**So lead with this, in your own words, early:**

> "The way I read your plan, this is a migration, not a development project —
> you've said preserve the function names and the behaviour. So the code isn't
> the risk. The risk is everything the code is attached to: the Slack URL, the
> Jira webhook, the token, and whatever else is pointing at that API that nobody
> has written down. Coming into a system I didn't build, working out what it
> really is and what it's connected to, and moving it without breaking anything
> around it — that's what I've done for thirty years."

---

## Sell the thing you're actually good at

You are not the person who writes the application. You are the person who comes
in, works out how it really hangs together, and makes it move without breaking.
That is a different job and it is the harder one on a migration.

Four lines, all true, all in plain language:

1. **"I'm not a developer and I've never claimed to be. I own everything around
   the code — how it's built, packaged, deployed, configured, monitored, and
   what happens when it breaks at two in the morning."**

2. **"What I'm good at is walking into something undocumented and figuring out
   what it actually does, as opposed to what people think it does."**

3. **"The first thing I'd do is not touch anything. I'd read the live account —
   not the templates, the running account — because six years of console edits
   won't be in the templates. The gap between those two is the project."**

4. **"I care that the proof is real. You can't verify a system using the thing
   you're verifying. If I tell you the new account behaves like the old one, I
   want that to be evidence, not an assertion."**

Number 4 is yours — it is the idea you've been writing about publicly, and on
this project it *is* the job: their whole Phase 5 is producing evidence that the
new account behaves like the old one. **Lead with it if you only get to say one
thing.**

---

## When you don't know — the three-part move

This is the most important skill in this interview, and from thirty years in it
reads as confidence, not a gap. Practise the shape, not the content:

1. **Say you don't know. One sentence, no apology, no waffle.**
2. **Say the adjacent thing you do know.**
3. **Say how you'd find out.**

> *"I don't know that offhand. What I do know is that the packaging on those
> custom runtimes has to be exactly right or it fails at startup with an error
> that tells you almost nothing — so the first thing I'd do is get a trivial
> version deployed end to end before touching anything real, and work from the
> error."*

**Never recite a line you can't go one level deeper on.** One follow-up question
is all it takes, and the cost isn't that answer — it's that everything you said
before it stops being believed. If you're not sure you can defend it, don't say it.

---

## Five things to land — in your words, not jargon

1. **"I'd run it in your phase order."** Say it out loud. They wrote a seven-phase
   plan; agreeing with it is worth more than improving it.

2. **"The cutover is URLs, not infrastructure."** The address of the API has the
   account baked into it, so it changes when you move accounts. Everything
   pointing at it has to move the same day — the Slack command, the Slack button
   handler (separate thing, easy to miss), the Jira webhook, and every runbook
   nobody has inventoried.

3. **"Running both at once isn't a safety net, it's a risk."** Two copies of the
   bot, both holding credentials that can write to the same repos. Only one can
   be the live one at any moment, and that has to be written down, not remembered.

4. **"I'd put an alias on each function so a rollback is one command."** You don't
   need more than that sentence. If they go deeper and you're comfortable: the
   bigger idea is collapsing the prod/test pairs onto one function with two
   aliases, *but* the config is attached to the version rather than the alias, so
   it isn't a free change — and their SOW says don't change behaviour without
   approval, which you agree with. **If you're not sure you can hold that
   conversation, stop after the first sentence.** It is still a good answer.

5. **"Deleting isn't the same as decommissioned."** A console that says deleted
   isn't proof. Log groups outlive their stack and keep billing. And the GitHub
   token isn't an AWS resource at all, so nothing you delete in AWS touches it.

Plus the two contract points: **the freeze calendar** (midterms 3 Nov, then
year-end — that could take out four or five of your twelve weeks of change
windows) and **the Phase 7 soak may not fit the contract**.

---

## If they ask about coding

Do not volunteer this. If asked, be straight and unbothered:

> "I'm not an application developer — I never have been. I read code and debug
> it, I've owned the build and release path for Java, .NET, Node and now Go, and
> for scripting and automation I use Python. These days when I need code written
> I generate it and then verify it, which is how most of this work is going. What
> I bring is knowing what the system is supposed to do and whether what's in
> front of me actually does it."

On Go specifically — **this one you should pre-empt**, because it's in their
required skills:

> "I should say up front, I'm not a Go developer. Your plan says preserve the
> behaviour, so I'm reading the Go surface here as packaging, configuration and
> troubleshooting rather than feature work — and that part I'm comfortable with.
> If it does turn into writing Go, I'd rather tell you now."

**Then ask them the question that settles it:** *"How much of this do you expect
to be actual code changes versus infrastructure and coordination?"* If the answer
is "a lot of code," you've found out in minute five instead of week two.

---

## The repo — think before you mention it

You have a working model of their system. **You did not write that Go by hand
and you cannot walk through it line by line.** If you offer it and they ask you
to, you're in exactly the position you're trying to avoid.

Safest: **don't bring it up.** It did its job — it's why you know the packaging
trap, the URL problem and the alias catch. That knowledge is yours regardless of
who typed the code.

If they ask what you've done to prepare, and only if you feel steady:

> "I put together a working model of what your scope of work describes, so I
> could pressure-test the migration plan against something real instead of just
> having opinions. I'll be upfront — I generated the code; what I was after was
> the failure modes and the sequencing."

Never screen-share it unprompted.

---

## Two stories — 90 seconds, plain language

**"A deploy went wrong."** A release shipped to both app stores with zero tests
having run, on a green pipeline. A check sat above the unit tests, failed
wrongly, and killed the run before the tests — and because it exited clean, the
pipeline was green. The fix wasn't that one bug, it was the ordering: a check
about the test harness could silently hide a problem in the product. **"Green
pipeline is a statement about the pipeline, not about the software."**

**"Something you broke."** Took `www` down. Deleted a stack in a development
account that turned out to own production DNS — created there years earlier,
nobody had written down what it owned. The lesson was to enumerate what a thing
*owns* before deleting it, not what it's named. **Land it on their Phase 7.**

Both stories are about judgement and verification. Neither needs you to be a
programmer. That's why they're the right two.

---

## Ask them

1. **"How much of this is code changes versus infrastructure and coordination?"**
2. "How does the change freeze run around election coverage and year-end?"
3. "What's your release cadence, and what does production acceptance actually
   mean — one cycle, two, a soak?"
4. "Who owns the Slack app and the Jira webhook config — can I change those, or
   is that another team?"
5. "Is there a repo you're comfortable having a test bot cut real branches in?"

---

## Do not

- **Don't recite anything you can't defend one level down.** Fewer, safer lines
  beats more, riskier ones. Every time.
- Don't volunteer that you use AI to write code — but don't dodge it if asked.
- Don't claim Go. Pre-empt it instead.
- Don't say "my hands-on development is Python and TypeScript." Retired.
- Don't lead with the repo, and don't screen-share it.
- Don't pitch the follow-up proposals as "what I'd do next." You won't be there.
- Don't say "we should use Terraform." Ask what they already have.
- Don't pitch CoachArc as a business. "A product I built to stay hands-on."
- Don't mention Claude or ChatGPT in the FNF context. Copilot was sanctioned there.
- Don't lead with Jenkins.
- Don't give a rate before you know W2-via-vendor vs C2C vs 1099.
