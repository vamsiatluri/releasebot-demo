# The same thing, in plain English

No jargon. Read this one before the interview; read the other one if someone asks
for the specifics.

---

## What the project actually is

They have a small robot that cuts software releases. It lives in one AWS account.
They want it moved to a different AWS account, with nothing about it changed.

**That last part is the whole job.** Their own document says: keep the names the
same, keep the behaviour the same. So this is not a building project. It is a
**moving** project — and moving something while it is still being used is a
coordination problem, not a coding problem.

I built a working model of their system in a throwaway account, moved it, broke it
on purpose, and wrote down what broke.

> **If you say one thing about this project, say this:** the code is frozen, so the
> risk is not in the code. It is in everything that *points at* the system and has
> to be re-pointed on the same day.

---

## The one-sentence version of each piece

### Why a brand-new AWS account

Because the migration starts from an empty account, and you cannot rehearse "the
target is empty" inside an account that already has things in it. It also let me
delete everything and *check* that it was gone.

### Five identities, each allowed to do one thing

Think of these as five keys cut narrowly.

- **One key for the robot that does the deploying.** The interesting part: *there is
  no key*. GitHub proves who it is at the moment it deploys, and gets a pass that
  expires within the hour. Nothing is stored anywhere to be stolen.

  And it only works from the one branch and the two named environments. Not "anyone
  with access to this project" — that is how a pull request from a contractor ends
  up rewriting production.

- **Two keys for the robot itself**, one per job it does. Each can read only its own
  settings and write only its own logs. If one is stolen, that short list is the
  entire damage.

- **Two housekeeping keys** so that request logging works. These exist because of a
  genuinely annoying discovery: see "the thing that looks like your fault" below.

### The robot — four copies, exactly as their plan says

Four small programs. Two do the real work, two are the test twins. Their document
names all four, and I kept the names.

**Not simplifying them was a decision, not laziness.** If something misbehaves after
the move, you want the only thing that changed to have been the account. Change the
names at the same time and you have two suspects instead of one.

### The front door — one door, two doorbells

One web address. Two doorbells on it: one marked **production**, one marked **test**.
Same door, same routes, and which doorbell you ring decides which copy of the robot
answers.

The alternative — two separate doors — means two sets of routes that slowly drift
apart until one day they behave differently and nobody knows why.

> **Here is the part that makes this a real migration problem.**
>
> The web address has the account's fingerprint baked into it. Move accounts, the
> address *has* to change. Which means every single thing that knows the old
> address has to be updated the same day: the chat integration, the ticket system's
> notification hook, and every instruction document nobody wrote down.
>
> I hit this three times just building the model.
>
> **The fix I'd propose:** put a permanent address of our own in front of it. Then
> the switchover is "point the permanent address at the new place", the undo is
> "point it back", and it takes seconds instead of a scramble. But that is a
> suggestion for *after* the move lands — not something to bolt onto it.

### Bookmarks — the thing I'd most want to explain

Every copy of the robot has a **bookmark** on it called `live`. The front door never
calls the robot directly; it follows the bookmark.

So a deploy is: upload the new version, then **move the bookmark**.

And an undo is: **move the bookmark back.** One command. Seconds. No rebuild, no
redeploy, no scramble at 2am. The old version is still sitting right there.

**The bigger idea, which I am deliberately only proposing:**

The test twin runs *identical* code to the real one. So you could have one copy with
two bookmarks — "test" and "production" — and promoting would just be moving a
bookmark to the version test already approved. Half as many things to configure.

I proved that works. And then I found the reason not to do it:

> A program gets **one set of permissions**, no matter which bookmark you came
> through. So merging the pairs means the single remaining copy must be allowed to
> read **both** test and production secrets. The wall between test and production
> stops being a wall.
>
> **Four copies with four narrow keys give you that wall for free. Merging spends it.**

That is why both approaches are supported and only one is shipped. The ability to
merge is already built in and sitting dormant. If their security people want the
simplification, it is a configuration change, not a rewrite. If they don't, nothing
deviated from their plan.

**This is the kind of thing I'd want to be hired for.** The clever version and the
right version were not the same version, and the difference only showed up because
I built both.

### Settings and passwords

Four layers, and each thing lives in exactly one of them:

| | Where it lives | Why there |
|---|---|---|
| Which environment am I? | worked out from the bookmark it was called through | the only thing that can differ between two bookmarks |
| Settings the same everywhere | frozen with the code | what shipped is what was tested |
| Settings that differ per environment | a central settings store | change it without redeploying |
| **Passwords and tokens** | **a pointer, never the actual value** | see below |

### The single most valuable thing I found

Rolling back a version rolls the **settings** back too. They are frozen together.

I proved it: I rolled one back one version and it immediately started failing,
because that older version was from before the passwords were set.

Follow that through:

> **Rotating a password quietly destroys your ability to roll back**, because every
> older version still has the old password in it.

Now look at their switchover plan. Steps 6, 7 and 8 **rotate the passwords.**

So under the obvious approach, **the undo button stops working on the exact day you
most need it** — and nobody finds out until they reach for it.

Storing a *pointer* instead of the actual password fixes it. Roll the code back, the
password stays where it is, and the undo still works.

**That is a finding, not a preference.** I'd open the engagement by asking whether
they'd checked it.

---

## How a release actually runs

```
build  →  test  →  ⏸ someone approves  →  production
```

The important detail: **production does not get rebuilt.** It ships the exact same
package that just passed on test. And both steps check the package's fingerprint
and refuse to continue if it differs.

Here is the actual proof from the running system:

```
real copy  →  rO9l+tiQLTDp1kjYNd5viQ1zGAh8sI/BilcX0JJJDJw=
test copy  →  rO9l+tiQLTDp1kjYNd5viQ1zGAh8sI/BilcX0JJJDJw=
                identical, character for character
```

"We shipped what we tested" is something the pipeline **checks**, not something
anyone has to hope.

---

## Approvals — two buttons, one gate

### The main one: approve in GitHub

The release **stops** and waits. GitHub emails the named reviewer, and nothing moves
until a person clicks approve. It cannot be skipped, because GitHub enforces it —
not a rule inside the pipeline that anyone who can edit the pipeline could delete.

> Worth knowing: this feature needs a paid team plan on a private project. On a free
> or personal plan it is refused outright. This project is public, which is what
> makes it available. On a private project without the plan, the nearest equivalent
> is making the release require a deliberate manual start — still a human decision,
> just enforced by the pipeline instead of by GitHub.

### The optional one: a button in chat

The pipeline posts a message in Slack saying a release is waiting, with an
**Approve and deploy** button on it. Clicking it approves the release.

**And I'd tell them straight: this is a weaker control than the first one.**

GitHub's version says *only these named people, signed in.* A chat button says
*anyone who can click in this channel*, and GitHub's record ends up showing the
robot's name rather than the person's. You have handed approval authority to
"whoever is in the channel".

So it has three protections, none of them optional:

1. **Every click is checked to be genuinely from Slack**, using a shared secret, and
   old clicks are refused so a captured one cannot be replayed. The address is
   public — without this, anyone who learns it can deploy to production.
2. **The person clicking is checked against a named list.** Being in the channel is
   not good enough. And **an empty list approves nobody** — if that read as "allow
   everyone", a configuration slip would silently become an open door.
3. **Every attempt is written down** — who clicked, whether they were allowed, what
   happened. Never the secret itself.

And it is deployed as a **separate, standalone piece**, on purpose. If a reviewer
doesn't like the chat button, the answer is "delete that one piece" and GitHub's
gate is untouched. Easy to reason about, easy to reverse.

### What happened when I actually clicked it — and this is the best story in the project

The button works. The click reached our code, the "is this really from Slack" check
passed, the "is this person allowed" check passed, it picked the right release, and
it called GitHub.

**And GitHub said no.**

Not because of anything we built. Because of the *kind* of password we were using.

GitHub has two kinds of access tokens — an older kind and a newer, more precise kind.
Everyone sensibly reaches for the newer one. It turns out the newer kind **is not
allowed to approve releases at all**. Not "needs another permission ticked" — not
allowed, full stop.

> And here is why that costs people an afternoon: the error says
> **"Resource not accessible by personal access token"**, which sounds exactly like a
> permission you forgot to switch on. So you go and look — and the permission is
> *already on*. It was on. The token wasn't missing anything. It was the wrong kind
> of token, and no amount of adding permissions to it would ever have worked.

**Honest status, then:** every piece I built is proven working. The one thing left is
swapping in the older kind of token, which takes a minute. I deliberately did not
reuse the general-purpose token already on this machine — that one can also delete
repositories and administer the organisation, and putting it inside a small service
to save sixty seconds is a bad trade. A narrow, short-lived one is the right answer.

**This is a good thing to say out loud in an interview**, because it is exactly the
shape of the problems this job is made of: nothing was broken, every component worked,
the documentation technically said so, and the error message pointed at the wrong
thing.

### And then a second one, found the same way

The next release failed at the step that posts the message. The reason:
**the chat feature had never been saved into the project.** It was running in the
cloud, it worked, everyone could see it — and it existed nowhere but one laptop.

Which meant something worse than a missing file. **The release system had never
actually sent that message.** Every one anyone had seen was sent by hand while the
button was being built. It was being *demonstrated*, not *used* — and a step that has
never run on its own is not a step, it is an assumption.

> **Deployed is not committed, and a running system is not a record of itself.**

Nothing warned about this, because nothing was broken. The thing that caught it was a
release failing for an unrelated reason, and someone actually reading why.

**That is the whole job, in one sentence:** the dangerous problems in a migration are
not the ones that break. They are the ones that work, in a way nobody wrote down.

### A detour worth telling, because it is a good instinct story

Slack hid the setting I needed and said, helpfully, *"you won't need this."* It was
in a mode that delivers messages over a permanently open connection to a
permanently running program — which is exactly the thing a pay-per-use function
cannot be. Right choice behind a corporate firewall. Wrong choice the moment you
want something that only runs when called. I turned it off and the field came back.

---

## Watching it

**Logs** — written as structured records. Passwords are removed by *name*, not
blanked out, because a blanked-out value still tells you how long it was. And the
front door's detailed logging is deliberately off, because that mode writes the
contents of requests into the logs — and these requests contain chat secrets.

**Three alarms. The third is the one people forget:**

| | |
|---|---|
| something important is failing | the obvious one |
| **nothing has reported in three hours** | **the one that matters** |
| someone is probing the public address | an attack, or a half-finished password change |

> A system whose checks have *stopped running* looks exactly like a healthy system.
> Every dashboard stays green and the silence is invisible. So the second alarm
> treats *silence itself* as the failure.

**A dashboard** — one screen, behaviour on the left, availability on the right.

**A heartbeat check** — switched off by default, because it bills forever and it is
a *monitor*, not a test.

---

## How I check it still works

**Thirteen behaviour checks**, written once and runnable three ways: free on a laptop
with nothing connected, as the gate after every deploy, and on a timer.

Every check says *why it exists* in plain words, so the report is readable by
someone who is not an engineer. Four of them are not obvious:

- a forged request must be refused
- a repeated old request must be refused
- cutting the same release twice must succeed quietly, not blow up
- an irrelevant notification must still answer "fine" — because the ticket system
  **switches off** a notification that keeps erroring

**A reading tool** — it reads a *running* account and writes down what is actually
there, which is not always what the paperwork says. Then it compares two accounts.

That comparison is the real point. During the overlap period, the evidence you want
is not "production worked". It is **"production and the new one gave the same
answer."**

**And I ran the whole loop:** read the old account, generate the new one, compare,
fix, repeat. The differences closed from **15 down to 8**, the blocking ones from
**7 down to 0**, and every one of the remaining 8 is explainable on purpose. Three
of five attempts failed before creating anything — which is the safety working, not
the tool failing.

---

## The dashboard for management

One web page, behind a login, built for people who are not going to open a console.

It shows the flow, which stage is where, whether approval is waiting, which version
each environment is on, and the fingerprint match proving test and production are
running the same thing.

**Why this is not decoration:** on a 3-month contract, the thing that kills the
project is not technical. It is a manager three levels up who cannot tell whether it
is on track, and starts asking for status reports that eat the delivery time. One
link that answers the question is cheaper than a weekly meeting.

---

## Their seven phases, and what I have for each

| Their phase | What I'd bring day one |
|---|---|
| 1 · Get access, inventory it | a tool that reads the live account rather than trusting the paperwork |
| 2 · Build the infrastructure | the roles and functions, already built once |
| 3 · Routes and settings | both doorbells, settings store, passwords as pointers |
| 4 · Run both accounts side by side | build-once-ship-twice with the fingerprint check |
| 5 · Validate | the thirteen checks, the alarms, the account comparison |
| 6 · Switch over | a checklist — plus the rollback trap found before they hit it |
| 7 · Shut the old one down | a teardown that deletes and then **proves** it; it caught a leftover |

---

## The three sentences I'd actually open with

> "I read your scope of work, built a working model of it in a throwaway account,
> and moved it the way you're planning to."

> "Three things broke that I'd want to tell you about before you hit them — the
> biggest is that your rollback plan and your password-rotation step collide, and
> you'd only find out when you needed the rollback."

> "I also found a way to simplify your four functions into two, proved it works,
> and then found the reason not to do it. I'd rather bring you both and let you
> decide."

---

## Two things to be ready for

**"You're not a programmer."**

> "Right — I don't write the code. On this project the code is frozen by your own
> scope of work. What I do is read a running system, find where it is going to
> break, and get it moved without an outage. The three problems I found here I found
> by building it and breaking it, not by reading it."

**"What did you not finish?"**

> "The chat-button approval is live and correct right up to the last step, where
> GitHub refuses it — because the modern kind of access token isn't permitted to
> approve releases, which the error message does not tell you. It needs a different
> token and I didn't want to reuse a broad one just to close it out. The GitHub
> approval path itself is fully proven. And none of this has been tested at your
> scale, on your account, with your integrations — the first week would be finding
> out where my model is wrong."

That second answer is the one that lands. Being specific about what is unproven is
what makes the proven list believable.
