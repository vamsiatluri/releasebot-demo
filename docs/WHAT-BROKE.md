# What broke, and how we found it

Every row here is a real defect from building this, in a real AWS account. None are
hypothetical and none came from reading documentation.

**The theme, if there is one:** almost none of these were caught by a test. They were
caught by *checking the running system against what the tool claimed*. The single most
repeated shape is **a green result that meant something other than what it said**.

---

## 1. Green that wasn't — the most dangerous class

| # | What happened | Why it was invisible | The fix |
|---|---|---|---|
| 1 | **CloudFormation updated the stack and not the Lambda code.** A new field was simply absent from the API response. | CFN compares `Code` as *strings*. Overwriting the same S3 key leaves `S3Key` identical, so `UpdateFunctionCode` is never called. Env vars really did change, so the stack was genuinely updated. | S3 key carries the artifact's **sha256**, so a code change becomes a template change. |
| 2 | **A verification step that failed only when it succeeded.** `curl \| grep -q` under `set -o pipefail`. | `grep -q` exits on the match → curl dies on the broken pipe → pipefail takes the status from curl. Passed when the marker was missing. | Fetch to a file, grep the file. |
| 3 | **The deploy check went stale on a rename.** It looked for an element id that no longer existed and failed a deploy that had worked. | A check whose correctness depends on being edited alongside the thing it checks. | Compare a **hash** of the uploaded file to a hash of the served file. Cannot go stale. |
| 4 | **A whole feature was live in AWS and never committed.** The Slack approval Lambda, its template, its tests. | Nothing fails when infrastructure is live but unrecorded. Every behaviour check passed. `git status` had shown it for hours. | `git status --porcelain` must be empty before claiming done. The CI step that used it had **never once run**. |
| 5 | **The pipeline had never sent a gate notification.** It died on `unknown status: awaiting`. | Every Slack message in the channel had been posted by hand while the button was being built. | Verify delivery at the destination, not at the transport. |
| 6 | **A deploy printed "page uploaded" while CloudFront served the old file.** | Uploading an object and serving it are two different facts. | Verify the **served URL**, with retries, not the upload. |
| 7 | **The per-environment IAM boundary did not exist.** `cutRelease` and `cutReleaseTest` shared one role, while the docs claimed otherwise. | A claim in a document is not a control. | Four roles, verified with `simulate-principal-policy` in **both** directions — that test can read test, and *cannot* read prod. |

---

## 2. Lambda behaviour that surprised us

| # | What happened | Why | The fix |
|---|---|---|---|
| 8a | **A "GitHub timeout" that was not a timeout, and not GitHub.** A prod cut failed with `context deadline exceeded (Client.Timeout exceeded while awaiting headers)` — in **3 milliseconds**. | Same root cause as #8, wearing a disguise. A frozen goroutine resumes in a LATER invocation with its 25-second timer long expired, so the first context-aware call fails instantly and Go words it as a timeout. It is also logged against whichever invocation happened to resume it. | Already fixed by #8. Finding that out cost four wrong theories — NAT, security group, DNS, the HTTP client — each disproved by a throwaway probe. **The duration was the tell: a 15-second timeout that returns in 3ms is not a timeout.** |
| 8 | **Work started in a goroutine after the response was silently dropped — non-deterministically.** Four `cut` commands recorded; the `mergeback` logged `releasebot starting` and nothing else. | Lambda **freezes the execution environment** the instant the handler returns. The goroutine survives only if the container is reused. Warm worked, cold didn't. | Acknowledge, then **invoke the function again** with `InvocationType: Event`. The job gets its own lifetime. Falls back to inline if the dispatch fails — a slow release beats a lost one. |
| 9 | **The async invoke returned 403 `InvalidSignatureException`, and IAM said `allowed`.** | The version was in the **path** (`/functions/name:41/invocations`). The colon's percent-encoding doesn't survive the canonical-request step. | Put the version in the **query string** (`?Qualifier=`). |
| 10 | **A 403 couldn't be diagnosed** because the error body was deliberately not logged. | Correct — an AWS error body names the role ARN. But "403" alone can't distinguish *forbidden* from *badly signed*, and those have different fixes. | Log `X-Amzn-Errortype` — a bare string, safe, and the single most useful fact. |
| 11 | **A config change landed on `$LATEST` and the aliases kept serving the old behaviour.** | A published version freezes code **and** configuration. Aliases point at versions. | Publish a new version and move the alias — which is the deploy mechanism working as designed. |
| 12 | **Lambda env vars pin to a VERSION, not an alias.** | So rotating a secret silently invalidates every older version as a rollback target. | Per-environment config in **Parameter Store**; the version freezes a *pointer*, not the values. |
| 13 | **A dead-letter queue was impossible with version pinning.** | A Lambda destination attaches to a **qualifier** — alias or `$LATEST` — and a template cannot name a version that doesn't exist until deploy time. | Target the alias; carry the accepting version in the payload so running on different code is **logged**, not invisible. |
| 14 | **The DLQ would have silently done nothing.** | Lambda writes an on-failure destination **as the function**, using its execution role — not as a service principal. | `sqs:SendMessage` on the execution role. |
| 15 | **The DLQ alarm would have sat in ALARM forever.** | An empty SQS queue reports **no datapoints**, not zero, and the default treats missing as breaching. | `TreatMissingData: notBreaching`. |

---

## 3. The gate, and how a release could go backwards

| # | What happened | Why it mattered | The fix |
|---|---|---|---|
| 16 | **Two runs sat pending approval at once**, one of them on a commit production had already moved past. | Approving the stale one would have **replaced newer production code with older code** — and every check would pass. A correct deploy of the wrong thing. | Three layers: a **concurrency group** (newer supersedes pending), a **Reject button** in Slack, and a **downgrade guard** that asks production what it's running and refuses an ancestor. |
| 17 | **The gate had only one button.** | Without a NO, the only ways to clear a release you've decided against are to approve it anyway or leave it pending — and a growing queue is how a gate stops being read. | Reject, same GitHub endpoint, state `rejected`. Decision read from `action_id`, **never the label** — a copy change would otherwise turn Reject into Approve. |
| 18 | **A fine-grained PAT can never approve a deployment.** The 403 reads as a missing permission; the token already had `Actions: write`. | Undocumented in the error. | Classic PAT with `repo` scope only. |
| 19 | **`live` alias ≠ an environment.** `cutReleaseTest`, invoked as `cutReleaseTest:live`, overrode its own `RARC_ENV=test` and answered `{"env":"prod"}`. | No unit test and no local run could see it — neither invokes through an alias. | A **deployment pointer** (`live`, `blue`, `green`) and an **environment** (`prod`, `test`) are different things Lambda spells the same way. Only the second may decide configuration. |
| 20 | **Rollback by version number is wrong.** `cutRelease` is on v21 while `releaseAutomationMergeback` is on v19 — the same release, different integers. | "Roll back to v19" means something different to each function → half an environment on each release. Passes health checks. | Roll back to a **commit**; `publish-version` writes it into the description and each function resolves its own version. |
| 21 | **Some rollback targets cannot start.** `v23` on both test functions carries a secret rotated since. | It fails during INIT: HTTP 500 and an **empty log**, because it died before it could log. | A config pre-flight that hashes the secret-bearing keys and **refuses** the rollback. |

---

## 4. Secrets, permissions, networking

| # | What happened | The fix |
|---|---|---|
| 22 | **Secret env vars were hand-applied**; a redeploy wiped them and the function died at INIT with `GITHUB_TOKEN is required`. | Template-owned as `NoEcho` parameters. |
| 23 | **The dashboard borrowed a `repo`-scoped classic PAT** to read pipeline state. | Deploy script announces the over-grant on **every run** and accepts a read-only fine-grained PAT instead. Still open. |
| 24 | **GitHub's anonymous API limit is 60/hour per egress IP**, and a Lambda's IP is shared. | Server-side cache so N browser tabs are one GitHub read; serves the last good value marked `cached` rather than blanking. |
| 25 | **The API endpoint was public with no rate limiting**; an unauthenticated flood costs invocations *before* the signature check. | WAF rate rule + stage throttle on **prod only** — test stays open because CI must be able to exercise it. |
| 26 | **A resource policy change doesn't apply until the stage is redeployed**, and CFN won't create a new Deployment just because the policy changed. | `DeploymentNonce` parameter whose only job is to change the Deployment resource. |
| 27 | **And it takes 20–30 seconds to propagate.** A probe 6 seconds later reported the old behaviour and looked like a broken condition. | Poll until it flips, with the unconditional-deny probe first to separate Resource matching from condition evaluation. |
| 28 | **`!Ref AWS::NoValue` does not REMOVE a property** — it means "don't manage it". The old policy persisted after reverting. | Clear it explicitly, then verify by reading it back. |

---

## 5. Tooling traps that cost real time

| # | Trap | Tell |
|---|---|---|
| 29 | `aws cloudformation deploy` substitutes template **defaults** for every parameter you omit. **It deleted this account's GitHub OIDC provider** and no pipeline run could have assumed its deploy role. | A stack update that "succeeded" while something unrelated vanished. Fixed with `_update-stack.py`, which re-sends everything as `UsePreviousValue`. |
| 30 | `read` returns 1 at EOF without a delimiter, **even though it populated the variable**. Under `set -e` the script exits silently. | `pbpaste \| script` printed nothing at all. |
| 31 | CFN resources appended after `Outputs:` parse as outputs. | Caught before deploying by counting them either side of the marker. |
| 32 | Python 3.9 can't nest the same quote inside an f-string; and single quotes inside `python3 -c '...'` terminate the shell string. | `SyntaxError: f-string: unmatched '('` from a line that looks fine. |
| 33 | A multi-line Python snippet inside a YAML block scalar silently ends the block. | "Workflow file issue" with no line number. |
| 34 | Lambda rejects a non-JSON payload at the API boundary, so it can't be used to force a failure. | `InvalidRequestContentException` — the invocation never happens. |
| 35 | A null `started_at` unmarshals to `""`; treating it as the zero time reports **56 years** of elapsed time. | A dashboard renders it without blinking. |

---

## 6. Process failures — mine

Worth keeping because they are the same class of error, applied to how the work was done
rather than to the code.

| # | What I got wrong | What changed |
|---|---|---|
| 36 | Claimed the pipeline posted gate notifications to Slack. **It never had.** | Never describe a CI step as working until a CI **run** has done it. A step exercised by hand is a demonstration, not a step. |
| 37 | Described Parameter Store as done when one value was wired. The owner pushed back and was right. | Finish the thing, or say which part isn't done. |
| 38 | Documented a per-environment IAM boundary that did not exist. | A claim in a doc is not a control; verify in both directions. |
| 39 | Deployed the IAM stack with explicit parameters and deleted the OIDC provider. | `UsePreviousValue` for anything not deliberately being changed. |

---

## The three lines worth memorising

> **A check whose correctness depends on being edited alongside the thing it checks will eventually lie.**

> **A deployment pointer and an environment are different things that Lambda spells the same way. Only the second may decide configuration.**

> **Verify at the destination, not at the transport.** The webhook returned 200 the whole time.

> **Read the duration before the error text.** A 15-second timeout that returns in 3 milliseconds is not a timeout — and chasing the word rather than the number cost four wrong theories about the network.

---

## 7. What a genuinely EMPTY account found

`scripts/bootstrap.sh` was written from a working account and had never been run against
nothing. Its first execution in a brand-new AWS account (450315222669, created for this)
found **ten real defects in a row**, none of which could surface anywhere else.

| # | What happened | Why only an empty account shows it |
|---|---|---|
| 1 | `_update-stack.py` crashed on `describe-stacks` | It assumed the stack already existed. The very first deploy is the one case it most needed to handle. |
| 2 | `LedgerTable` failed with a **KMS NotFoundException** naming a key id that is nowhere in the template | `SSEEnabled: true` does not mean "encrypt" — the table is already encrypted with an AWS-owned key. It means "use the **managed** key `aws/dynamodb`", which **does not exist in a new account** until something creates it. |
| 3 | The compute stack could not deploy at all | A new account's Lambda concurrency limit is **10**, and AWS refuses any reservation leaving fewer than 100 unreserved. The reservation that protects a mature account makes the stack undeployable in a fresh one — and the error talks about unreserved concurrency, not about the account being new. |
| 4 | The retry loop hammered a dead stack six times | A failed create leaves `ROLLBACK_COMPLETE`, which can be neither updated **nor** created. It must be deleted. The loop also printed "IAM not propagated" for *every* failure — a retry that cannot tell a transient failure from a permanent one is a way of not reading the error. |
| 5 | `S3 Error Code: NoSuchKey` | Bootstrap uploaded **content-addressed** keys and never passed them to the template, so the functions pointed at the default `releasebot/cutrelease.zip`. The digest is the whole point and is useless if nothing is told about it. |

### #6 — the orphan a correct `DeletionPolicy` leaves behind

The compute stack failed `AWS::EarlyValidation::ResourceExistenceCheck` with
**"Validation failed with 1 error(s)"** and **no resource-level event** — CloudFormation
named nothing. Bisected with a five-rung resource ladder, one change-set per rung:

```
A_funcs_logs           CREATE_COMPLETE
B_versions_aliases     CREATE_COMPLETE
C_alarms               CREATE_COMPLETE
D_ledger               FAILED      ← flips here
E_full                 FAILED
```

**Root cause: `releasebot-ledger` already existed.** `LedgerTable` carries
`DeletionPolicy: Retain` — correct for an audit record of what shipped to production — so
the *earlier* rolled-back create left the table behind with no stack owning it, and every
retry after that failed on the table already existing.

Proven in both directions rather than assumed:

```
WITH the orphaned table present:
  table-only                      FAILED
  queue-and-alarm-only            CREATE_COMPLETE   ← so it is the table, not the stack
removing the orphan…
WITHOUT it:
  table-only-after-cleanup        CREATE_COMPLETE
  full-template-after-cleanup     CREATE_COMPLETE
```

The trap is self-reinforcing: **every failed attempt re-creates the condition that makes
the next attempt fail**, and the retry loop looks like a flaky account.

`bootstrap.sh` now resolves it rather than documenting it. Before the compute stack it
checks whether the table exists while no healthy stack owns it, and:

- **zero rows** → it can only be wreckage of a rolled-back create. Delete it and continue.
- **any rows** → that is real release history. **Stop**, and say so. Deleting an audit
  table to get a green deploy is the wrong trade, and a script should not be the thing
  that makes it.

### #7 — a disable switch two functions never saw

With the ledger orphan gone, the compute stack failed again:

```
Specified ReservedConcurrentExecutions for function decreases account's
UnreservedConcurrentExecution below its minimum value of [10].
```

Bootstrap had read the real quota, printed `concurrency limit 10 — per-function
reservation DISABLED`, and passed `ReservedConcurrency=0`. The switch was working. Two of
the four functions simply **hardcoded `ReservedConcurrentExecutions: 2`** and never
consulted it, and on an account whose total limit is 10 *any* reservation is refused.

★ A parameter that most of the resources honour reads as working. The two that ignore it
are invisible until you land in the one account where the difference matters.

The lint added for it then found a **third** — `infra/50-slack-approve.yaml`, a different
stack, which would have failed four steps later. `preflight.sh` now greps every template
for a bare numeric reservation and treats one as a blocker.

One more thing the same run fixed: preflight was raising the concurrency limit as a
**blocker** while bootstrap was already compensating for it, so bootstrap refused to run on
a condition it had itself handled. A check that fails on something the caller has already
dealt with teaches people to pass `--skip-preflight`, which costs you every other check.
It now downgrades to a warning when the caller declares the reservation disabled.

### #8 — a function pointing at a key nothing ever wrote

Step 9 created the approval function with the template's default
`releasebot/slackapprove.zip` and failed with `NoSuchKey`. The upload loop at step 4
iterated over `cutrelease mergeback` and had never included `slackapprove` — while
`make package` had been building the zip the whole time. The failure arrives four steps
away from the loop that was supposed to write it, naming a key that looks entirely
plausible.

Two smaller ones in the same step. The approval stack's three credential parameters have
no defaults, so the first run aborted with
`[SlackSigningSecret, GitHubApprovalToken, GitHubEnvironmentId] must have values` — which
reads like a broken script rather than like work a human still owes. And the stack has to
*exist* before `set-approval-token.sh` can fill the token in (it updates one parameter and
carries the rest forward), so aborting was also the wrong shape. It now deploys with
values that **fail closed** and says so in as many words; the environment id, which is not
a secret, is read from the GitHub API instead of being asked for.

And `_update-stack.py`, having correctly deleted a `ROLLBACK_COMPLETE` stack, then fell
into its own "the probe failed for some other reason" guard — because after a deliberate
delete the probe had *succeeded* and `stderr` was empty. It exited 1 printing nothing at
all. **A reasonless failure is worse than the dead stack it replaced.**

### #9 — ⛔ the green deploy that kept serving the old code

The worst defect in the whole exercise, and the last one found.

`cutRelease`'s `/health` returned **502** while `cutReleaseTest` returned 200. The function
log showed `START / END / 1.25 ms` and **no error**. A direct `Invoke` gave the real
answer:

```
{"errorMessage":"initialisation failed: config: GITHUB_TOKEN is required"}
```

That part is correct behaviour — prod has no token yet, test runs with
`RELEASEBOT_DRY_RUN=1` which makes it optional. But fixing the *reporting* of it exposed
something much worse. After rebuilding and redeploying, the health response still said
`commit: 8e68d63` and `built: 03:10:31Z` — **the old binary**. The stack said
`UPDATE_COMPLETE`.

```
$ aws lambda get-alias --function-name cutRelease --name live --query FunctionVersion
1
$ aws lambda list-versions-by-function --function-name cutRelease
$LATEST   1
```

**`AWS::Lambda::Version` publishes a new version only when the Version RESOURCE changes.**
Its only property was `Description: !Sub 'deployed by ${AWS::StackName}'` — a string that
never changes. So CloudFormation updated the function's code onto `$LATEST`, left the
`live` alias pointing at version 1, created no new version, and reported success.

This is the same family as the content-addressed-key bug and strictly more dangerous.
There, the deploy didn't change anything and the stack went green. Here the deploy *did*
change something, the stack went green, **and the change was not serving.** Nothing about
the stack's status, the function's `LastModified`, or the S3 object can tell you.

Two fixes, and the second matters more than the first:

1. The description now carries the content-addressed key —
   `!Sub 'deployed by ${AWS::StackName} from ${CutReleaseKey}'`. A changed binary changes
   the key, which changes the Version resource, which publishes a version, which moves
   the alias.
2. **Bootstrap reads the alias back.** It compares the `CodeSha256` the `live` qualifier
   actually serves against the artifact it just uploaded, for all four functions, and
   dies if they differ:

```
✓ cutRelease live -> v2 serves the artifact just uploaded
✓ cutReleaseTest live -> v2 serves the artifact just uploaded
✓ releaseAutomationMergeback live -> v2 serves the artifact just uploaded
✓ releaseAutomationMergebackTest live -> v2 serves the artifact just uploaded
```

★ **A deploy is not finished when the deploy tool is finished.** Every check in this file
that found something real asked the destination; every check that got fooled asked the
transport.

The INIT failure got three fixes of its own, because its failure mode was misleading in
three separate ways at once: a bare 502 is indistinguishable from a crash, the reason
existed only in a direct-Invoke response body nobody reads until much later, and
`/health` — the one endpoint whose entire job is to report readiness — was the endpoint
that could not answer. It now returns **503 with the missing key named**, the failure is
logged at INIT where an operator will see it, and every other route still refuses:

```
HTTP 503
{"ok": false, "ready": false, "reason": "config: GITHUB_TOKEN is required",
 "hint": "...the function is refusing to serve rather than running without credentials."}
```

### #10 — the teardown that would have reported an empty account

`sandbox-down.sh` is built around the right idea — it lists every resource class back and
fails if anything remains. Its stack list had simply gone stale: written before the
dashboard, approval and network stacks existed, it would have deleted seven stacks, left a
**CloudFront distribution, a Cognito user pool, a VPC with a NAT Gateway, two interface
endpoints, a DynamoDB table and an SQS queue** running, and then printed:

```
TEARDOWN VERIFIED: the account is empty.
```

Because it verified only the classes it already knew about. ★ **A verification that
enumerates a hand-written list proves the list, not the account** — and it fails in the
most expensive possible direction, since the thing it misses is the thing still billing.
It now covers DynamoDB, SQS, Cognito, CloudFront, WAF, NAT gateways, VPC endpoints,
non-default VPCs and SSM parameters, and deletes the retained ledger table explicitly,
since no stack delete ever will.

★ The lesson: **a build script written against an account that already has everything has
never been tested.** All ten were invisible in the account it was written in, and each of
the last four was invisible even in the empty one until the one before it was fixed —
defects queue up behind one another, so a single run that goes green start to finish proves
more than any amount of re-reading. #9 in particular could not be found by review at all:
the template was valid, the deploy succeeded, and the wrong code was serving.
