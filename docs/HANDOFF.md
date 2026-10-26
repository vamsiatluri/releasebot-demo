# Start here

Everything needed to pick this project up on a machine that has never seen it.

---

## 1 · What this is

A working reconstruction of **ReleaseBot ("Robbie")** — the NBC News release
automation tool — built to rehearse the migration described in their SOW: move it
from the `msnbc-dev` AWS account to the `msnbc` production account, run both in
parallel, cut over, decommission the old one.

It is **not** NBC's source. It is a faithful model of the system the SOW
describes, built so every decision in the migration plan can be *demonstrated*
rather than asserted.

**The contract started 2026-10-26.** Three months. The migration is the job; the
model is how it was prepared for.

### The shape of the real system

```
/cut news-app 5.4.0          Slack slash command
        ↓
API Gateway  ONE api, TWO stages (prod · test)
        ↓   a stage variable selects the function
cutRelease | cutReleaseTest | releaseAutomationMergeback | ...Test
        ↓   four functions, four IAM roles, nothing shared
GitHub REST API — create a branch, open a pull request
```

**Four functions, two commands.** Their SOW says both numbers in different
places, and conflating them is the easiest mistake to make out loud: *"four
permission identities and four small programs"* (Phase 2) and *"test the two
commands"* (Phase 5). Two jobs — cut and merge — each existing live and test.

⛔ It never runs `git`. It is an API client: read trunk's head, create a ref,
open a PR. No clone, no working copy, no credential on a disk.

---

## 2 · Set the machine up

Three paths, in order of how likely they are to work on a locked-down laptop.
Full detail and troubleshooting: **[NBC-MAC-QUICKSTART.md](NBC-MAC-QUICKSTART.md)**

```bash
# Path A — needs only curl and the AWS CLI. No Go, no Homebrew, no admin rights.
mkdir -p ~/releasebot && cd ~/releasebot
curl -fsSL -o baseline \
  https://github.com/vamsiatluri/releasebot-demo/releases/download/v1.0.0/baseline-darwin-arm64
curl -fsSL -o SHA256SUMS \
  https://github.com/vamsiatluri/releasebot-demo/releases/download/v1.0.0/SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing     # verify before you run it
chmod +x baseline && ./baseline
```

> *"developer cannot be verified"* → `xattr -d com.apple.quarantine baseline`
> Intel Mac → use `baseline-darwin-amd64`. Check with `uname -m`.

The repo itself, for the docs and scripts:
```bash
git clone https://github.com/vamsiatluri/releasebot-demo.git
```

### ⛔ AWS credentials on a machine you do not control

A dedicated IAM user exists for exactly this: **`releasebot-demo-laptop`**. Its
only permissions are `sts:AssumeRole` on the two demo accounts and
`sts:GetCallerIdentity`. Proven in both directions — it reaches
`releasebot-sandbox`, and it is **explicitly denied** on `coacharc-prod`.

⚠️ **Do NOT copy the `default` profile.** That is `user/vamsi.atluri` in the org
master, which can assume into **every** account in the organisation — including
`coacharc-prod` (899088266180), which is also VSA BioScience production, with
real customers and real orders. Those keys do not belong on an NBC laptop.

The scoped credentials were written to `~/releasebot-demo-credentials.txt`
(mode 0600) on the machine that created them. Move it, paste the three blocks
into `~/.aws/credentials` and `~/.aws/config`, **then delete the file.**

---

## 3 · The two live environments

Both running, both healthy. They cost money — tear them down when the demo value
has been extracted.

| | account | switch role | dashboard |
|---|---|---|---|
| **prod-demo** | `826653639065` | [switchrole](https://signin.aws.amazon.com/switchrole?account=826653639065&roleName=OrganizationAccountAccessRole&displayName=prod-demo) | https://d1ktxi0tgqjqcx.cloudfront.net/ |
| **prod-assess** | `450315222669` | [switchrole](https://signin.aws.amazon.com/switchrole?account=450315222669&roleName=OrganizationAccountAccessRole&displayName=prod-assess) | https://d2k77s00h0549z.cloudfront.net/ |

⚠️ The console switch-role links only work once signed in to the org master,
`732817553439`. And the org NAMES differ from the labels — `releasebot-sandbox`
is prod-demo, `releasebot-clean` is prod-assess. The labels are what the
dashboards and Slack cards say, so use those out loud.

**prod-demo** is the one the pipeline deploys to by default. **prod-assess** was
built end to end *by the assessment tool itself* — capture an account, generate a
template, deploy it, compare — which is the proof that the Phase 1 tooling
actually produces something deployable.

Check either at any time:
```bash
curl -s https://0aohv0kx5k.execute-api.us-east-1.amazonaws.com/prod/beta/health | python3 -m json.tool
curl -s https://u4f22c74mi.execute-api.us-east-1.amazonaws.com/prod/beta/health | python3 -m json.tool
```

---

## 4 · The tool, and the order to use it

```bash
./baseline preflight -profile P -region R    # ⭐ ALWAYS FIRST
./baseline capture   -profile P -region R -label L -out FILE
./baseline review    -in FILE
./baseline generate  -in FILE -out template.yaml      # a DRAFT. Read, do not apply.
./baseline compare   -a A.json -b B.json              # the parallel-run evidence
./baseline capture   -estate ...                      # + 11 services a teardown needs
./baseline teardown  -in FILE -out teardown.md        # ordered decommission plan
```

`scripts/assess.sh <profile> <region> [--against P R] [--teardown]` runs the
whole sequence.

**preflight exit codes:** `0` all reads permitted · `1` a required read denied,
and it NAMES the missing IAM actions · `2` only the decommission reads missing.

### Two refusals that are the point, not an inconvenience

**`teardown` refuses a `-prefix` capture.** A teardown built from a partial
inventory deletes what matched and leaves the rest billing, while reporting
success.

**`teardown` refuses a capture taken without `-estate`.** A migration baseline
cannot see buckets, tables, parameters or VPCs — planning a decommission from one
produces a plan that looks complete and omits everything that keeps costing money.

⛔ **Never use `-prefix` on a first run.** `-prefix releasebot` once excluded
**five of six functions** — they are named `cutRelease`,
`releaseAutomationMergeback` and so on — and the report read like a confident,
complete assessment of one sixth of the system.

---

## 5 · The thing this project is actually about

**[WHAT-BROKE.md](WHAT-BROKE.md) — 16 defects, every one of them green.**

> Every single one reported success. The tool was telling the truth about what it
> did; it just was not doing the thing I assumed.

The four best, if you only read four:

| | |
|---|---|
| **The deploy went green and the old code kept serving.** `AWS::Lambda::Version` republishes only when the version RESOURCE changes — a fixed `Description` meant new code landed on `$LATEST` and the alias never moved. Stack said `UPDATE_COMPLETE`. | #9 |
| **A credential was stored, capability-proven, and never delivered.** An empty template default meant `GITHUB_TOKEN=""`. And the logs could not tell: the resolver logs only on failure and the no-token path logs nothing, so wired and unwired were byte-identical. | #16 |
| **A teardown reported "the account is empty"** while leaving a CloudFront distribution, a Cognito pool, a VPC with a NAT gateway, a table and a queue running. It verified a hand-written list, so it proved the list rather than the account. | #10 |
| **The docs claimed test and prod had separate IAM.** They shared one role. A claim in a document is not a control. | #7 |

### The four questions that fall out of it

1. **Did the state change, or did the operation just succeed?** Read the alias back, not the stack status.
2. **Would the failure I am ruling out have written anything?** If not, "no errors" is evidence of nothing.
3. **Can this check fail?** A check never seen to fail has not been tested.
4. **Who is asserting this — me, or the other side?** Ask GitHub which credential it saw. Ask IAM to simulate. Ask the health endpoint what commit it is running.

---

## 6 · Day one at NBC

**[DAY-ONE.md](DAY-ONE.md)** has the full version. The short form:

- Run **preflight** first. A denial is a result — it names the IAM actions, and
  that list is a short conversation with the platform team rather than a stack trace.
- Capture **both** accounts, not just dev. The production account already exists
  and already holds other workloads, so *"here is what is in `msnbc` and here is
  where ReleaseBot collides with it"* is a week-one artifact nobody asked for.
  Stack names, IAM role names and log group names are account-global.
- What to hand them at the end of week one: what is actually there · **what no
  CloudFormation stack owns** (the finding) · where the secrets live and in what
  form · **everything pointing AT the API that nobody has written down** — the
  Slack commands, the interactivity URL, the Jira webhook. ⚠️ The invoke URL
  contains the API id, so a new account means a new URL. **That list IS the cutover.**

### The two scope questions to settle in week one, not week twelve

**The freeze calendar.** Election day was 11/03; year-end runs mid-Dec to early
Jan. On a 12-week contract that is 4–5 weeks of change windows gone.

**The release cadence.** Their Phase 7 gate is "production acceptance" — properly
a soak of about two release cycles. If they release monthly, **Phase 7 falls
outside the contract.** Compress and cut over in week 6–7, hand Phase 7 over as a
pre-approved runbook with a named executor, or extend.

---

## 7 · Where everything is

| | |
|---|---|
| [NBC-MAC-QUICKSTART.md](NBC-MAC-QUICKSTART.md) | run it on a locked-down laptop |
| [DAY-ONE.md](DAY-ONE.md) | the first morning |
| [WHAT-BROKE.md](WHAT-BROKE.md) | 16 defects and their mechanisms |
| [CHEAT-SHEET.md](CHEAT-SHEET.md) | one page, for talking about it |
| [CUTOVER.md](CUTOVER.md) | the irreversible step, in order |
| [RUNBOOK.md](RUNBOOK.md) · [TESTING.md](TESTING.md) | operating it |
| [MIGRATION-PLAN.md](MIGRATION-PLAN.md) · [THEIR-PLAN-EXPLAINED.md](THEIR-PLAN-EXPLAINED.md) | their seven phases, translated |
| [CONNECTING-SLACK-AND-JIRA.md](CONNECTING-SLACK-AND-JIRA.md) | the integration surfaces |
| [BASELINE-TOOL.md](BASELINE-TOOL.md) | how the assessment tool works inside |
| `catalog-info.yaml` | ReleaseBot as Backstage entities |

**Code:** `cmd/baseline` the assessment tool · `cmd/{cutrelease,mergeback}` the
Lambdas · `internal/baseline` capture, review, generate, compare, teardown ·
`infra/*.yaml` the stacks, numbered in deploy order · `scripts/` the operations.

---

## 8 · Open items

- **Both demo accounts are billing.** Tear down when done:
  `./baseline capture -estate ...` then `./baseline teardown ...`, then follow the
  plan. ⚠️ An AWS account cannot be deleted, only closed, with a 90-day tail.
- **Both release targets share one GitHub environment (`production`)**, so a
  prod-demo release can invalidate a pending prod-assess approval. The concurrency
  group was fixed to separate the runs; the environments were not. ~10 minutes:
  separate `production-demo` / `production-assess` environments.
- **A config-only change does not republish a Lambda version.** The version's
  `Description` carries the artifact key, so new *code* republishes and new
  *configuration* does not — which left prod-assess serving an old version with an
  empty token after a green `UPDATE_COMPLETE`. Worked around by hand; the template
  fix is to put the config generation in the description too.
- **Backstage** — a local portal exists at `~/Documents/GitHub/backstage-portal`
  with ReleaseBot catalogued. Not yet deployed to AWS. NBC said they are planning
  to adopt it, which is the opening for extending this contract.
