# Day one

Start date **2026-11-02**. This is the page to open that morning.

⚠️ **Election day is 2026-11-03.** You arrive the day before a US midterm at a
news organisation. Assume a change freeze is already in force and do not be
surprised by it — everything below is read-only and touches nothing, which is
exactly why it is the right first week's work.

---

## Before you start — ask for these in the offer/onboarding thread, not on day one

Access takes days to land. Asking on the morning of the 2nd costs you the week.

| ask | why that form |
|---|---|
| **Read-only on `msnbc-dev`**, via SSO/Identity Center or an assumable role | enough for all of Phase 1; ⛔ do not ask for long-lived access keys |
| AWS's **`ReadOnlyAccess`** managed policy | smaller ask than a bespoke list, and covers every call this tool makes |
| **Repo read** on ReleaseBot and the repos it cuts branches in | |
| A named **Slack workspace admin** you can book | you need a collaborator seat on the app, not admin yourself |
| Whether the **`msnbc` account exists yet**, and who creates it | the longest lead time in the project, and nobody has costed it |

Build rights on the production account can wait until week two.

---

## The first hour — prove you can read, before trying to read everything

```bash
go build -o /tmp/baseline ./cmd/baseline
/tmp/baseline preflight -profile msnbc-dev -region us-east-1
```

```
0  everything needed is readable            → go
1  a REQUIRED read is denied                → hand them the named list, that is your morning
2  only the Phase 7 reads are missing       → carry on; fix before the decommission
```

★ **A denial on day one is a result, not a setback.** "Here is exactly what I can
and cannot see in your account" is a better first contact with a platform team
than either "please give me read-only" or a tool that fell over.

## The first day — the assessment

```bash
scripts/assess.sh msnbc-dev us-east-1 --label msnbc-dev
```

Runs preflight, captures the account, grades it, and drafts a template.
**It creates nothing, changes nothing, deletes nothing** — which is what makes it
safe to run in a freeze, on your first day, without asking anyone's permission.

Produces, under `build/assess/<date>-msnbc-dev/`:

| file | what it is |
|---|---|
| `source.json` | the raw inventory |
| `source-review.txt` | graded BLOCKER / WARN / INFO |
| `generated.yaml` | a draft template — ⛔ **read it, do not apply it** |
| `README.md` | the page a person reads first |

⛔ **Do not use `--prefix` on the first run.** `--prefix releasebot` once excluded
five of six functions, because they are named `cutRelease`,
`releaseAutomationMergeback` and so on — and the report then read like a
confident, complete assessment of one sixth of the system.

## What you hand them at the end of week one

Not the JSON. One page, in their language:

1. **What is actually there** — functions, roles, API, stages, and what each is wired to.
2. **What no CloudFormation stack owns.** This is the finding. A hand-made
   resource is the thing that will not come across, and it is invisible to
   anyone reading the templates.
3. **Where the secrets live, and in what form** — a literal in an environment
   variable is a blocker, not a note.
4. **Everything pointing AT the API that nobody has written down** — Slack slash
   commands and the interactivity URL, the Jira webhook, runbooks, bookmarks.
   ⚠️ The invoke URL contains the API id, so a new account means a new URL. This
   list IS the cutover.

## The two questions to settle in week one, not week twelve

**The freeze calendar.** Midterms 11/03, year-end mid-Dec to early Jan. On a
12-week contract that is 4–5 weeks of change windows gone. Front-load Phases 1–4
into the election freeze — discovery and a build in a NEW account touch nothing
in production — and target cutover for the back half of November.

**The release cadence.** Their Phase 7 gate is "production acceptance", properly
a soak of about two release cycles. If they release monthly, **Phase 7 falls
outside the contract.** Three options, agreed in writing early: compress
validation and cut over in week 6–7 · hand Phase 7 over as a pre-approved
runbook with a named executor · extend.

★ Raising the second one unprompted, in week one, is the strongest available
signal that you have delivered fixed-scope work before.

---

## Later: Phase 7

```bash
scripts/assess.sh msnbc-dev us-east-1 --teardown
```

Needs the estate reads (`preflight` exit 2 names them). Produces `teardown.md`:
an ordered decommission plan derived from the capture rather than a hand-written
list — including the resources no stack owns, which is the list that keeps
billing after the project is "done".
