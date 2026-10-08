# Their document, line by line, against what was built

Walk this top to bottom. For each thing they asked for: what it means, what was
actually built and proven against it, and **the question it raises for them** —
because the questions are what make you sound like someone who has done this,
rather than someone who has read about it.

---

## 1. Project objectives

> **Build the AWS production infrastructure required to support ReleaseBot.**

Built and proven. Four stacks — identity, compute, edge, monitoring — deployed
into a genuinely empty AWS account and verified working. Then torn down, the
teardown *verified* (not assumed), and rebuilt from one command.

**What I learned doing it:** three of the things that broke had nothing to do with
the application. Permissions take seconds to become visible and the next step
fails if it doesn't wait. Request logging needs a permission granted once per
account *and per region*. The platform reserves some of its own labels and
refuses to let you set them.

**Ask them:** *"How long does AWS access provisioning usually take here? I'd want
to request everything on day one, including things I won't need until week eight."*

---

> **Migrate production and test ReleaseBot functions, API endpoints, deployment
> workflows, configuration, and integrations.**

Note the order of that list — functions first, integrations last. **I'd argue the
risk runs the other way.** The functions move in an afternoon. The integrations
are the project.

**Ask them:** *"Who owns the Slack app and the Jira webhook configuration? Are
those changes I can make, or do they go through another team with their own change
window?"* If a different team owns them with a two-week lead time, the project
plan is wrong and you want to know in week one.

---

> **Validate deployment and application behavior through smoke, integration, and
> end-to-end release testing.**

Built as a behaviour suite of thirteen checks that runs unchanged in three places:
free on a laptop with no AWS involved, as the gate after every deploy, and on a
schedule. Proven against four live endpoints across two regions.

**The thing worth saying:** a smoke test proves the function is alive, not that
it's correct. The only honest test of a release bot is cutting a release — and
nobody wants to cut a real one from an unproven account.

**Ask them:** *"Is there a throwaway repository in the same GitHub organisation,
with the same branch protection, that a test bot can cut real branches in?"*
Week-one ask, not week-six.

---

> **Cut over ReleaseBot to the production account and decommission the legacy AWS
> development resources.**

The teardown script doesn't trust itself: after deleting, it lists every category
back and fails if anything remains. Its first run caught a log book that nothing
had created on purpose and nothing would have deleted.

**Ask them:** *"What does 'production acceptance' mean concretely — one release
cycle, two, a soak period, a named sign-off?"* This gate is the only truly
irreversible step, and on a twelve-week contract it may not fit inside the
engagement.

---

## 2. Scope of work

| They wrote | Built / proven | The question it raises |
|---|---|---|
| **AWS access and IAM execution-role configuration** | Least-privilege roles, one per function family, scoped to their own secrets path. `iam:PassRole` restricted by `PassedToService`, which is the quiet privilege-escalation path people leave open. | *"Is there an existing account standard for execution roles I should match, or am I writing these?"* |
| **Four Lambda functions** | All four, named exactly as specified, on the Go custom runtime, with a packaging check that fails the build rather than failing at startup in AWS. | *"Is the ReleaseBot source in a repo you control with its existing workflows, or is some of it configured by hand in the console?"* |
| **API Gateway routes, production and test stages** | Both routes, both stages, each stage wired to its own function through a stage variable. | *"Is there a custom domain in front of the current API, or is Slack pointed at the raw AWS address?"* The answer decides how painful the cutover is. |
| **Runtime, handler, environment variables, secrets, logging, Datadog** | All configured. Secrets deliberately held as *references* rather than values — see §5. | *"Where do secrets live today — function settings, or a secret store?"* |
| **GitHub Actions secrets and parallel deployment jobs** | A dual-account workflow that builds **once** and deploys the same artifact to both, then verifies the deployed code fingerprint matches what it built. | *"Would you be open to the pipeline proving its identity each deploy instead of storing a permanent AWS key in GitHub?"* |
| **Slack endpoints, Jira webhook validation, GitHub access, release-cycle testing** | Signature verification with constant-time comparison and a replay window; Jira shared-secret check; idempotent GitHub writes. All four failure paths tested. | *"Does the current bot verify the Slack signature? And is there a timestamp window on it?"* Asked neutrally — it's a real question, not an accusation. |
| **Controlled cutover, removing parallel logic, credential rotation, documentation, decommissioning** | Cutover checklist with the rollback that stays valid until the decommission gate; verified teardown; runbook. | *"Who's on call for ReleaseBot after handoff, and what do they have today?"* Operational ownership is in their own acceptance criteria. |

---

## 3. The four things only a real deployment found

This section is the strongest thing you have, because none of it could have come
from reading. Lead with it if they ask what you'd watch out for.

**A pointer is not an environment.** Every function has a bookmark called `live` —
that's the rollback mechanism. The code treated the bookmark as a statement about
which environment it was in, so the *test* function, reached through its `live`
bookmark, reported itself as *production*. A pointer that says where to find
something and a label that says which environment you're in get written the same
way, and only the second may decide configuration.

**Rolling back also rolls the settings back.** Freezing a version freezes its
settings with it. So rolling back to a version published before a password changed
means rolling back to a copy with the old password — it fails to start. **Which
means rotating a credential quietly invalidates every older version as a rollback
target.** Three steps of their cutover rotate credentials. Under the current
design, the rollback plan stops working on the day it's most needed.

**"Account-level" usually means account-level *per region*.** The logging
permission had been set in the first region for weeks, so when the second region
failed it read like a problem with the thing just built. It wasn't.

**An alarm that names no categories watches nothing.** The staleness alarm — the
one that fires when the checks stop reporting — was written without dimensions
against a measurement only ever published with them. It could never fire and never
clear. It sat red through four passing runs. *An alarm nobody has seen fire is not
monitoring.*

---

## 4. Deliverables and acceptance criteria

> **"IAM, four Lambdas, API Gateway stages/routes, configuration, secrets,
> logging, and monitoring are deployed in msnbc."**

Done, and more importantly **re-doable**: destroy and rebuild are one command
each, and the rebuild refuses to report success unless every behaviour check
passes. That's the difference between "it's deployed" and "the templates are the
source of truth."

> **"All four GitHub Actions workflows can deploy to the production account;
> parallel deployment is available during validation."**

Built — with the split-brain risk written into the pipeline rather than into
somebody's memory. Two live bots both holding credentials that can write to the
same repositories is a risk, not a safety net. Exactly one is the system of
operation at any moment, and the pipeline says which.

> **"API, Slack, Jira, GitHub configuration access, observability, and end-to-end
> release behavior pass documented tests."**

The suite, the dashboard, the alarms. **And the version that's worth more than
"production passed":** run the *same* checks against both accounts and compare the
results. The evidence you want isn't that production is green — it's that
production and development answered identically.

> **"Production endpoints and credentials are active; parallel jobs are removed;
> legacy functions, API Gateway, and dev credentials are retired."**

Cutover checklist, ordered so the first hard-to-reverse step is as late as
possible. Verified teardown.

> **"Runbook, endpoint records, deployment instructions, rollback steps, test
> evidence, and operational ownership are current."**

Written. On a twelve-week contract this row decides whether they'd have you back:
the deliverable is a system somebody else operates.

---

## 5. Their skills table — honest scoring

| They ask for | Where you actually stand |
|---|---|
| **AWS Lambda, API Gateway, IAM, account migrations, CLI, CloudWatch, least privilege** | Strong, and now demonstrated end to end in a real account. Five AWS accounts at FNF, a second-region DR environment built from scratch, three isolated accounts on your own product. |
| **GitHub Actions, repo secrets, pipelines, release management, rollback, automation** | Strongest area. Travis to GitHub Actions across 30+ applications; 20+ repos moved off static keys; build decoupled from deploy. |
| **Slack apps, Jira webhooks, GitHub API, REST, HMAC verification, token auth** | Mixed, and say so. HMAC verification and token auth are direct — you did constant-time webhook verification and JWT authorisation on your own product. A *Slack app with slash commands and a signing secret* was new, so you built one. Inbound Jira webhooks were new too. |
| **Go custom runtime, Linux packaging, bootstrap handlers, HTTP payload troubleshooting** | **The gap. Pre-empt it.** Note their own words: *troubleshooting*, *packaging*, *environment configuration*. Operator words, not developer words — and their plan says preserve the behaviour, so the code shouldn't be changing. |
| **Datadog and CloudWatch logging, dashboards, monitors, runbooks, post-cutover support** | CloudWatch is strong. Datadog you selected and stood up at William Hill; the Lambda extension specifically you haven't run. The approach to state: keep structured logs going to CloudWatch regardless, so observability never depends on the vendor layer resolving. |
| **Technical planning, dependency management, change coordination, test evidence, stakeholder communication, operational handoff** | **Your strongest row and the one people undersell.** 25+ change requests through formal change control, the monthly release train, offshore coordination, weekly reporting to leadership. Previously Director of DevOps. |

---

## 6. Their responsibilities list — what to say about each

> *"Own the migration plan and technical execution from discovery through decommissioning."*

*"I'd run it in your phase order."* Then name phases 4 and 6 as the ones you'd
spend planning time on.

> *"Use infrastructure-as-code or repeatable automation where supported by the
> existing repository and account standards."*

**This sentence means: match what's already there.** Do not propose replacing
their templates. Ask what they have.

> *"Preserve existing function names and application behavior unless a change is
> required and approved."*

Agree with it, visibly. This is also why the alias proposal is a *proposal*: if
something misbehaves after the move, you want the only variable to have been the
account.

> *"Maintain a rollback path until production acceptance is complete."*

The old account stays deployed and untouched throughout — that's what makes it a
rollback rather than a re-migration. And raise the rotation trap: credentials
rotated at cutover silently invalidate older versions as rollback targets.

> *"Protect credentials and tokens; do not expose secret values in source, logs,
> tickets, or documentation."*

Directly built for: logs redact by key name rather than masking (a mask still
tells you the length); full request logging stays off because those request bodies
carry Slack tokens; the capture tool never reads a secret value at all.

> *"Produce implementation notes, test evidence, risk decisions, change records,
> and support documentation."*

Accumulate the runbook from week one, not week twelve.

> *"Coordinate with CloudOps/SRE, application owners, AWS account administrators,
> Slack administrators, Jira/GitHub owners, and observability stakeholders."*

Six groups. **That list is the real schedule risk** — each one is a dependency
with its own calendar.

**Ask them:** *"Of those six groups, which has the longest lead time for a change?
I'd want to book that one first."*

---

## 7. The five questions, if you only ask five

1. *"How does your change-freeze calendar run around election coverage and
   year-end?"*
2. *"What's your release cadence, and what does production acceptance mean
   concretely?"*
3. *"How much of this do you expect to be actual code changes versus
   infrastructure and coordination?"*
4. *"Who owns the Slack app and the Jira webhook config — can I change those, or
   is that another team?"*
5. *"Is there a repo you're comfortable having a test bot cut real branches in?"*

Each one is diagnostic. Each answer changes how you'd run the project. That is
what separates someone scoping work from someone hoping to be given it.
