# What got built, and why — in plain language

For talking about in the interview. No jargon you'd have to defend; each section
ends with a sentence you can actually say out loud.

---

## The thing in one breath

Somebody types `/cut news-app 5.4.0` into Slack. A few seconds later there's a
new release branch on GitHub and a message in the channel saying so. Everything
below exists to make that happen reliably, prove it happened, and make it safe to
leave on the public internet.

**Say it like this:** *"It's a Slack command that cuts a release branch and later
opens the merge-back pull request — the two moments in a release where a human
doing it by hand gets it wrong."*

---

## The journey of one request

### 1. The front door — **API Gateway**

A public web address that accepts the message from Slack. It's the only part of
the system anyone outside can reach.

Two things worth knowing about it:

- **It has two doors into the same building** — one marked `prod` and one marked
  `test`. Same code behind both, different settings. That's how you try something
  on the test door before anyone's real release depends on it.
- **The address has the AWS account baked into it.** This is the whole reason the
  migration is a project rather than an afternoon: move to a new account, you get
  a new address, and everything pointing at the old one has to be updated the
  same day.

**Say it like this:** *"The front door's web address contains the account number,
so moving accounts changes the address. That's what makes the cutover a list of
URLs in other people's systems rather than an infrastructure job."*

### 2. The workers — four **Lambda** functions

Small programs that don't run at all until something calls them, then run for a
second and stop. You pay for the second, not for a server sitting there.

There are four because the system has two jobs (cut a release, merge it back) and
each has a production copy and a test copy.

**Say it like this:** *"Four small programs that only exist while they're running.
Two jobs, each with a live and a test copy."*

### 3. The bookmark on each worker — a **Lambda alias**

This is the piece worth being able to explain, because it's where the safety
comes from.

Every time you deploy, the code is frozen into a numbered version that can never
change afterwards — version 1, version 2, and so on. The alias is a bookmark
called `live` that points at one of them. The front door is wired to the
bookmark, not to any particular version.

So deploying is: upload new code, freeze it as version 3, move the bookmark.
And rolling back is: **move the bookmark back to version 2.** One command, a
couple of seconds, no rebuild, no redeploy, nothing to go wrong.

**Say it like this:** *"Each function has a bookmark called `live` pointing at a
frozen version, and the front door follows the bookmark. A rollback is just moving
the bookmark back — seconds, and nothing gets rebuilt."*

⚠️ **This is also where I found a real bug.** See "What broke" below — it's the
best story in here.

### 4. Who's allowed to do what — **IAM roles**

Each worker runs as a named identity that can do a very short list of things —
read its own secrets, write its own logs, nothing else. If one were ever
compromised, the damage is bounded by that list.

**Say it like this:** *"Each function runs as its own identity that can only
touch its own secrets and its own logs. Nothing has a key to the whole account."*

### 5. How the deploy pipeline proves who it is — **OIDC**

The normal way to let GitHub deploy into AWS is to create a permanent password,
paste it into GitHub's settings, and hope nobody leaks it — and then rotate it
forever.

The better way: GitHub and AWS establish trust directly, so GitHub asks for a
temporary pass each time it deploys and the pass expires in an hour. **There's no
permanent password anywhere, so there's nothing to leak and nothing to rotate.**

I've done this before at scale — it's the twenty-plus repositories I moved off
static keys at my last job.

**Say it like this:** *"I'd rather the pipeline prove who it is each time than
store a permanent AWS key in GitHub. No stored key means nothing to leak and
nothing to rotate — and their own plan has a step for rotating deploy
credentials that this just deletes."*

### 6. Where the packaged program sits — **S3**

A private bucket holding the zipped-up program. The deploy picks it up from
there. Locked down so nothing public can read it.

### 7. The record books — **CloudWatch Logs**

Every run writes a line about what it did. Two deliberate choices:

- **Nothing sensitive is ever written down.** The messages coming in carry Slack
  tokens, and the system holds a GitHub key that can write to the main branch.
  Anything that looks like a token or a password is replaced with the word
  "redacted" before it's logged — not masked, removed, because a masked value
  still tells you how long it was.
- **We create the log books ourselves rather than letting AWS create them.** If
  AWS creates them, they're set to keep everything forever and they don't belong
  to anything, so when you delete the system at the end of a migration they
  survive and quietly keep costing money.

**Say it like this:** *"We declare the log groups rather than letting Lambda
create them, because the ones Lambda creates never expire and aren't owned by the
stack — so they outlive the decommission and keep billing."*

### 8. The alarms

Three, and the third is the one people forget:

1. **Something broke** — the functions are erroring.
2. **Something is being refused a lot** — the front door is public, so a flat
   low number of refusals is normal (that's the system correctly saying no). A
   sudden jump means either someone's probing, or a password rotation only half
   landed.
3. **Nothing has been checked in three hours.** This is the important one. **A
   checking system that stops running looks exactly like a system that's fine** —
   every dashboard stays green because no bad news arrives. So "we haven't heard
   anything" is itself wired up as an alert.

**Say it like this:** *"The alarm I care most about is the one that fires when
the checks stop reporting. A verification pipeline that dies looks identical to a
healthy system — everything stays green because nothing's complaining."*

### 9. The one screen for management — a **CloudWatch dashboard**

Six panels, opening with a plain-English note saying what green means. Left half
is "is it behaving correctly", right half is "is it up and is the infrastructure
healthy".

**Say it like this:** *"One screen that answers 'is it up' and 'is it still doing
the right thing' without anyone opening a log file."*

### 10. The checks — a behaviour test suite

Thirteen checks. Some obvious (does it cut the branch), some less so:

- Send a message with a **forged signature** — must be refused. The address is
  public; this is the only thing between the internet and the main branch.
- **Replay a real message ten minutes later** — must be refused. A genuine
  message stays genuine forever unless you also check *when* it was sent.
- **Ask for the same release twice** — must succeed quietly, not break anything.
  Slack retries. Jira retries. If a repeat is dangerous, a hiccup corrupts a real
  release.
- **Send a Jira event that isn't relevant** — must answer "fine, ignored" rather
  than "error". Jira switches off a webhook that keeps erroring, so "not for me"
  has to look like success.

The same checks run three ways from one file: free on a laptop with no AWS
involved, as the gate after every deploy, and on a schedule.

**Why that matters for their migration specifically:** the thing they have to
prove is that the new account behaves like the old one. If you check the two
accounts with two different test suites, you've proven nothing. Running the one
suite against both and comparing gives you *"they're green in the same way"*,
which is a much stronger statement than *"they're both green."*

**Say it like this:** *"Same checks, both accounts, and compare the results. The
evidence you want isn't 'production passed' — it's 'production and dev gave
identical answers.'"*

### 11. The heartbeat — a **Synthetics canary** (deliberately switched off)

A tiny robot that visits the health address every few minutes and complains if it
can't. Useful — but it's a *monitor*, not a test suite, and it bills per visit
forever.

So: one canary doing one thing, and it's off by default so nobody switches on a
recurring bill by accident. The behaviour checking lives in the pipeline where
it's free and version-controlled.

**Say it like this:** *"A canary answers 'is it alive', a test suite answers
'is it correct'. I'd use both, but I wouldn't put the test suite in the canary —
you'd be maintaining tests in two places and paying for them every five minutes."*

---

## What broke, and what it taught

This is the most valuable part, because none of it could have been found by
thinking harder — only by actually deploying.

### The bookmark was being mistaken for the environment ★

Every function has a bookmark called `live`. The code also had a rule saying
"if the bookmark tells you which environment you're in, believe it."

So the **test** function, reached through its `live` bookmark, concluded it was
**production** — and the test door started answering "I am production." If that
had reached a real system, you'd have a test environment that believes it's live,
which is about the worst thing a release tool can believe.

The fix is a naming idea, not a code trick: **a bookmark that says where to find
something is not the same as a label that says which environment you're in.**
`live`, `blue`, `green` are pointers. `prod`, `test` are environments. They get
written the same way, so it's easy to conflate — and only the second should ever
decide how a system configures itself.

My own test suite caught it on first contact with a real deployment. Nothing
running locally could have — nothing local goes through a bookmark.

**Say it like this:** *"A pointer that tells you where to find something isn't
the same as a label that tells you what environment you're in. I'd conflated them,
and the test stage started reporting itself as production. The check that caught
it was one I'd written for exactly that reason — which is the argument for
testing against the real thing."*

### Moving the bookmark back also moves the settings back ★★

I ran a rollback drill on the live sandbox — moved the bookmark from version 5
back to version 4 — and the service immediately broke.

The reason is worth knowing. **When you freeze a version, you freeze the settings
with it, not just the code.** Version 4 had been frozen before the passwords were
loaded, so going back to it meant going back to a copy that had no passwords.

Normally that's exactly what you want: what you shipped is what you tested,
settings included. But it has a consequence nobody writes down:

> **The moment you change a password, every older version stops being a safe
> place to roll back to**, because each one still carries the old password.

Now look at their cutover plan: three of its steps rotate passwords and tokens.
So on the day they cut over, the rollback plan quietly stops working — and nobody
finds out until they need it.

The fix is the one I'd already have suggested for security reasons: keep the
*address* of the password in the settings rather than the password itself. Then
rolling the code back doesn't roll the password back, and the rollback plan
survives the rotation the cutover requires.

**Say it like this:** *"Rolling back moves the settings back too, not just the
code. So rotating a password quietly invalidates every older version as a
rollback target — which matters most on cutover day, because that's when their
plan rotates credentials. I'd keep a pointer to the secret in the settings rather
than the secret itself, and then rollback still works afterwards."*

### Three others, shorter

- **The front door wouldn't accept a setting that held a whole address.** It only
  lets you swap out one piece of the address, not the lot. Nothing warned me —
  the template validated fine and the service rejected it on creation.
- **Turning on logging needed a permission set once per account**, which no part
  of the system creates for itself. On a brand-new account the error says
  "logging role not set in account settings", which reads like a problem with the
  thing you just built and isn't.
- **Creating the permissions and the programs back to back fails**, because the
  permissions take a few seconds to become visible and the program creation
  checks for them immediately. Same files, no change, works on retry. The build
  script now retries rather than sleeping for a guessed amount of time.

**Say it like this:** *"Three of the four things that bit me were account-level
or timing problems rather than anything wrong with the code — which is pretty
much what an account migration is made of."*

---

## The part I'd actually lead with

There are two scripts: one tears the whole thing down, one builds it back.

The teardown **doesn't trust itself**. After deleting, it lists every category of
thing back and fails if anything remains. First time I ran it, it caught a log
book that nothing had created on purpose and nothing would have deleted — benign,
but it would have sat there billing forever, and no console screen would have
shown it to you.

That's the point. **"Deleted" is something you verify, not something you assume**
— a console saying a thing is gone is not the same as it being gone. And their
plan's final phase is exactly this: delete the old system and confirm nothing is
left behind.

**Say it like this:** *"I can destroy the whole thing and rebuild it with one
command each, and the teardown proves itself rather than trusting the console.
The first run caught a leftover that nothing owned and nothing would have cleaned
up. Decommissioning is a verification job, not a delete job — which is why I'd
want their Phase 7 behind an acceptance gate with evidence, not run as cleanup at
the end of a sprint."*
