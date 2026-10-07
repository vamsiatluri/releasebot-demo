# The baseline tool — reading their dev account into something you can diff

```
baseline capture  -profile msnbc-dev -label dev  -out dev.json
baseline review   -in dev.json
baseline compare  -a dev.json -b prod.json
baseline generate -in dev.json -out generated.yaml
```

## What problem it solves

Their Phase 1 deliverable is an *"approved implementation baseline"*. The whole
risk of that phase is that the baseline gets taken from the **templates** instead
of the **running account**. On an application live for years, somebody changed
something in the console during an incident and never put it back — and that
difference stays invisible until the copy behaves differently in production.

So this reads the account, not the templates.

## What it will not do

Be straight about this in the interview; it's what makes the rest credible.

1. **It never reads a secret value.** Environment variables are captured as key
   *names*; anything whose name looks like a credential is reported so a person
   decides how it moves. A tool that copies production secrets between accounts
   is a liability, not a feature. The classifier deliberately over-redacts —
   a wrongly-withheld value costs ten seconds, a captured credential in a JSON
   file attached to a ticket is an incident.
2. **It does not emit a template you apply blindly.** Generated IaC faithfully
   reproduces whatever accidents the source account accumulated. Carrying those
   forward is the opposite of what a migration is for. The output is written to
   be *read*: account-specific values become parameters, secrets become
   parameters with no default so the stack fails loudly if left unfilled, and
   every review finding is emitted as a comment above the resource it concerns.
3. **It does not move the code artifact.** In a real migration the artifact is
   rebuilt from source and promoted through the pipeline, so what ships is what
   was tested.

AWS does ship something adjacent — CloudFormation's IaC generator scans an
account and writes templates from existing resources. Worth using. It also won't
tell you what differs between two accounts, won't flag which resources are owned
by no stack, and will happily carry a secret value into a template.

## The four things `review` looks for

| Finding | Why it matters |
|---|---|
| Resource owned by **no stack** | Made by hand. A template-derived baseline misses it entirely. |
| **Secrets in environment variables** | Readable by anyone who can describe the function. Must move deliberately. |
| **Log groups with no expiry** | Created by Lambda, owned by no stack — they survive the decommission and keep billing. |
| **Full request logging enabled** | Writes request bodies to CloudWatch. These bodies carry Slack tokens. |

Plus: inline IAM policies (severity depends on whether a stack owns the role —
on a stack-owned role it's a design choice, on an unowned one it's undocumented
permission), aliases splitting traffic (someone left a canary running), missing
throttling on a public route, and alarms that notify nobody.

## `compare` is the point

Capturing one account is mildly useful. Capturing **both** and printing the
differences is the production-readiness evidence Phase 5 is asking for.

Things that legitimately differ between accounts — account ids inside ARNs, API
ids, regions — are normalised away, so the real differences don't drown in noise.
That normalisation is the difference between a tool people use and one they
ignore.

**The most valuable single comparison is the code hash.** Identical hashes across
both accounts is the proof that "build once, deploy many" actually happened,
rather than each account having built its own artifact from the same commit and
everyone hoping.

Exit codes gate a pipeline: `0` clean, `1` a blocking difference, `2` warnings.

## Proven against a real account

Run against the live sandbox on 2026-10-07. It found two defects in itself,
which is the honest part of the story:

- It reported "no resource policy" for four perfectly reachable functions,
  because the permission is granted on the **alias** (`cutRelease:live`) and
  querying the bare function name doesn't return it. A confidently-wrong finding
  is how a tool loses people's trust — fixed to read the policy per qualifier.
- It flagged inline IAM policy at the same severity whether or not a stack owned
  the role. On a stack-owned role that's a normal design choice already captured
  in a template; flagging both identically trains people to ignore the finding.

Then the drift test: capture, make a console edit of exactly the kind this is
built to catch (bump a memory size, add an undocumented `EMERGENCY_BYPASS`
variable), re-capture, compare.

```
[BLOCKER] lambda/cutRelease — environment variable names
          A: ...,RARC_ENV,RELEASEBOT_DRY_RUN,...
          B: ...,EMERGENCY_BYPASS,...,RARC_ENV,RELEASEBOT_DRY_RUN,...
[WARN   ] lambda/cutRelease — memory
          A: 512
          B: 1024
```

Leak check on the outputs: zero occurrences of any real secret value in either
the captured JSON or the generated template.

## How to talk about it

> *"Their first phase is producing a baseline, and the way that usually goes
> wrong is people take it from the templates instead of the running account —
> so years of console edits just don't appear. I wrote something that reads the
> account itself and diffs two of them. It never touches secret values, and it
> doesn't generate a template you'd apply blindly — it generates one you read.
> The part that actually matters is the comparison: during the parallel run,
> what you want isn't 'production passed', it's 'production and dev gave
> identical answers'."*
