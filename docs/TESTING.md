# Testing and monitoring

## Canaries are not tests, and tests are not canaries

The question "should this be AWS Synthetics canaries?" has a real answer, and
getting it right is most of the value here.

| | **Contract suite** | **Synthetics canary** |
|---|---|---|
| Answers | *does it behave correctly?* | *is it alive right now?* |
| Runs | on demand — PR, post-deploy, schedule | continuously, from outside |
| Can it block a deploy? | yes, it exits non-zero | not really |
| Cost | free in CI | billed per run, forever |
| Where it lives | version-controlled with the code | a script in S3 |
| How many | one suite, many checks | one journey |

**Use both, for the thing each is good at.** A canary that drove the full
contract suite would mean maintaining tests in two places, paying for them every
five minutes, and putting a Slack signing secret into a script sitting in an S3
bucket. A contract suite with no canary means you learn the service is down when
somebody next opens a pull request.

So:

- **Contract suite** — every behaviour, including the auth rejections. Gates the
  deploy. Also runs on a 30-minute schedule, which covers most of what people
  reach for a canary for.
- **One Synthetics canary** — the health endpoint, nothing else, every five
  minutes. That is the 1-minute-resolution heartbeat a cron cannot give you.
  It is `EnableCanary: false` by default in `infra/40-observability.yaml`, so
  nobody switches on a recurring bill by accident.

## One definition, three targets

```
cmd/contracttest  -base http://localhost:8080                        # PR: free, no AWS
                  -base https://<api>.execute-api.../test            # post-deploy gate
                  -base https://<api>.execute-api.../test  (cron)    # scheduled canary
```

Same binary, same cases, same assertions — only `-base` changes.

That matters more than it sounds **during an account migration**. The deliverable
of the parallel-run phase is evidence that the production account behaves like
the dev account. If the two accounts are checked by two different suites, the
evidence is worth nothing. The `compare-accounts` job in
`.github/workflows/contract-tests.yml` runs the one suite against both and diffs
the results, so the conclusion is not "both are green" but **"both are green in
the same way."**

## Reporting — three audiences, three formats

Every run emits all three from one execution:

1. **Terminal** — for whoever is running it.
2. **`$GITHUB_STEP_SUMMARY` markdown** — a table rendered on the run page and in
   the PR. **Every row carries a `Why`**, which is the bit that makes it
   readable by someone who is not an engineer. "23 passed" tells a manager
   nothing. *"Forged Slack signature rejected — the endpoint is public; without
   this, anyone who finds the URL can cut a release branch"* tells them whether
   they can sleep.
3. **JUnit XML** — GitHub's native per-test reporting, so a failure is annotated
   on the run rather than buried in a log.

Plus **CloudWatch metrics**, pushed by the workflow (not by the binary — that
keeps the runner credential-free and safe to run from a fork):

| Metric | Why it exists |
|---|---|
| `ContractSuccessPercent` | the gauge on the dashboard |
| `ContractCriticalFailed` | pages. Any value above 0 means do not cut over |
| `ContractChecksFailed` | visible, does not page |
| `ContractDuration` | a suite that gets slower is usually a service that got slower |

## The dashboard

`infra/40-observability.yaml` creates a CloudWatch dashboard called
**ReleaseBot** — one screen, six widgets, opening with a text panel that says in
plain English what green means. Behaviour on the left, availability and
infrastructure on the right.

The widget worth explaining to a manager is **"unauthorised requests rejected."**
It is not a failure graph. A flat non-zero line is the public endpoint being
probed and the bot correctly saying no. A sudden jump means either someone is
scanning, or a signing-secret rotation half-landed. Both are worth knowing.

## The alarm nobody writes and everybody needs

```
releasebot-contract-not-reporting  —  TreatMissingData: breaching
```

A verification pipeline that **stops running** looks exactly like a system that
is fine. Every other dashboard stays green. The contract metrics simply stop
arriving, and absence is invisible.

So one alarm treats missing data as breaching: if no contract run has reported
in three hours, that is itself the alert. The critical-failure alarm next to it
deliberately does *not* do this — it treats missing data as not breaching — so
the two concerns stay separate and neither masks the other.

## What is NOT covered, and why

The mutating checks — cutting a branch, opening the mergeback PR — need a real
GitHub repository. They are **off by default** (`-mutating`), and the runner
**refuses to run them against a repository whose name does not contain `shadow`,
`sandbox`, `test` or `scratch`.**

That guard is not paranoia. During the parallel-run window there are two live
bots with branch-write credentials on the same organisation; a suite pointed at
the wrong repository is not a failed test, it is a corrupted release.

Which is why **"is there a repo you're comfortable with a test bot cutting real
branches in?"** is a week-one question, not a week-six one. You cannot validate a
release bot without cutting a release.

## Proven, not asserted

Deployed to a throwaway AWS account on 2026-10-07 and run against both live
stages. The suite found a real defect on its first contact with a real
deployment — see `internal/config/config.go`, `WithAlias`: an alias named `live`
was being treated as the environment `prod`, so every function behind the `live`
alias reported itself as production, including the test stage. No unit test and
no local run could see it, because neither invokes through an alias.

That is the argument for this whole layer in one sentence: **the checks that
matter are the ones that only fail against the real thing.**
