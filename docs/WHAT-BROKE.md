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
