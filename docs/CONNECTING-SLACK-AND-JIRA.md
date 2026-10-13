# Connecting Slack, Jira and GitHub

`scripts/bootstrap.sh` builds the AWS side. This is everything it deliberately
cannot do, because it lives in someone else's console or involves a credential.

Work top to bottom. Each step says how to **prove** it rather than assume it.

---

## 0. What you need from them before you start

Ask for these in the kickoff, not in week three:

| | why it blocks |
|---|---|
| Slack workspace admin, or someone who can install an app | the slash command cannot be created any other way |
| The channel release notifications go to | `slack-channel` in Parameter Store, per environment |
| The Slack **user IDs** of the people allowed to approve production | not emails, not @handles — `U…` ids, and they differ per workspace |
| Jira admin, or someone who can add a webhook | same |
| Which Jira **projects** map to which repositories | the webhook derives the repo from the project key |
| The GitHub org, and whether it is github.com or **Enterprise Server** | GHES changes the API base and may remove the need for internet egress entirely |
| Who owns the VPC, and whether they will give you subnets | decides `--vpc` vs `--no-vpc` |

---

## 1. The two GitHub tokens

⛔ **They are not interchangeable, and this cost a day to learn.**

| | token | why |
|---|---|---|
| `slackApprove` | **classic PAT**, `repo` scope | `POST /actions/runs/{id}/pending_deployments` accepts **only** OAuth apps and classic PATs. A fine-grained token cannot call it with ANY permission set, and its 403 reads as a missing permission — the one we tried already had `Actions: Read and write`. |
| the four release functions | **fine-grained PAT** | They need `Contents: Read and write` and `Pull requests: Read and write` on the release repositories. That is the entire list, taken from the call sites. A classic `repo` token here is push access to every repository the owner can reach. |

```bash
bash scripts/set-write-token.sh      # refuses a classic token
bash scripts/set-approval-token.sh   # refuses a fine-grained token
```

Each verifies against the repository **before** deploying, and `set-write-token.sh` proves
write by creating a throwaway ref and deleting it.

⚠️ **A fine-grained token reads a PUBLIC repo without the `Contents` permission.** So "it
can read the repo" proves nothing about whether it can write. That is why write is proved
by writing.

★ Prefer a **GitHub App** over the fine-grained PAT if they will allow it: installation
tokens are short-lived, scoped per installation, revocable centrally, and attributable in
the audit log. A PAT is tied to a person and leaves when they do.

---

## 2. Repository secrets

`Settings → Secrets and variables → Actions`:

| secret | what it is |
|---|---|
| `AWS_DEPLOY_ROLE_ARN` | `arn:aws:iam::<acct>:role/releasebot-github-deploy-<acct>` — **not a credential**, useless without the trust policy |
| `SLACK_SIGNING_SECRET` | from the Slack app's Basic Information |
| `SLACK_WEBHOOK_URL` | the incoming webhook for the release channel |
| `JIRA_WEBHOOK_SECRET` | any long random string; you also give it to Jira |

Prove the OIDC trust works before anything else:

```bash
gh workflow run release.yml --ref main
gh run watch
```

The job **"Which AWS account did OIDC actually put us in?"** prints the account id. If the
assume-role fails, the error says *"Not authorized to perform sts:AssumeRoleWithWebIdentity"*
and names nothing useful — check both subject shapes and the per-environment one
(`scripts/preflight.sh` prints all three).

---

## 3. The GitHub Environments

`Settings → Environments`:

- **`test`** — no protection rules.
- **`production`** — add a **required reviewer**. This is the gate; nothing in the workflow
  enforces it, which is the point.

⛔ **Required reviewers are not available on a private repository below the Team tier.**
Creating the rule is refused with a billing error (HTTP 422). If the repo is private and
the org is on Pro, the gate becomes a deliberate `workflow_dispatch` input instead — still a
human decision, still recorded, but enforced by the workflow rather than by GitHub. Decide
this in week one; it changes the acceptance criteria.

⚠️ Note the **environment id** for the Slack approver: `gh api repos/{owner}/{repo}/environments`.

---

## 4. Slack

### 4a. Create the app
`api.slack.com/apps → Create New App → From scratch`, in **their** workspace.

### 4b. Slash commands
`Features → Slash Commands → Create New Command`

| field | value |
|---|---|
| Command | `/cutrelease` |
| Request URL | `https://<api-id>.execute-api.<region>.amazonaws.com/prod/beta/cut` |
| Usage hint | `news-app 5.4.0` |

Repeat for `/mergeback` → `.../prod/beta/merge`.

⚠️ **Point them at a CUSTOM DOMAIN if there is one.** The `execute-api` URL contains the
API id, and the API id **changes when the API is recreated** — which is exactly what a
migration does. `infra/30-apigw.yaml` carries a custom-domain option for this reason. A
URL their integrations depend on should be yours, not AWS's.

### 4c. Interactivity — the Approve button
`Features → Interactivity & Shortcuts → ON`

Request URL: `https://<api-id>.execute-api.<region>.amazonaws.com/prod/beta/slack-approve`

⚠️ **Block Kit replies go to the `response_url` in the payload, NOT to the HTTP response
body.** Returning the message in the body looks correct, returns 200, and is silently
discarded — the button appears to do nothing at all. That is already handled in the code;
do not "simplify" it.

### 4d. Incoming webhook
`Features → Incoming Webhooks → ON → Add New Webhook to Workspace`, pick the release
channel, copy the URL into the `SLACK_WEBHOOK_URL` repository secret.

### 4e. Signing secret
`Settings → Basic Information → App Credentials → Signing Secret`. It goes in **two**
places: the `SLACK_SIGNING_SECRET` repository secret, and the Lambda stacks.

### 4f. Approvers
Get each person's Slack **user id** (profile → ⋮ → Copy member ID — a `U…` string, not
their email), and pass them to the slack-approve stack as `SlackApprovers=U123,U456`.

### 4g. Prove it
```bash
BASE=https://<api-id>.execute-api.<region>.amazonaws.com/test/beta \
  bash scripts/slash.sh cut news-app 5.4.0       # correctly signed -> works
BAD_SIG=1 bash scripts/slash.sh cut news-app 5.4.0   # -> 401
SKEW=600 bash scripts/slash.sh cut news-app 5.4.0    # 10 min old -> 401
```

The last two matter as much as the first. A signature proves the request is **from Slack**;
the timestamp proves it is **from now**. Without the replay window a captured request is a
permanent key to cut a release.

---

## 5. Jira

`Settings → System → WebHooks → Create a WebHook`

| field | value |
|---|---|
| URL | `https://<api-id>.execute-api.<region>.amazonaws.com/prod/beta/cut` |
| Events | Issue → **updated** |
| JQL filter | narrow it — e.g. `project in (NEWS, TODAY) AND fixVersion is not EMPTY` |

Add the shared secret as a header: `X-Releasebot-Secret: <JIRA_WEBHOOK_SECRET>`.

⚠️ **A webhook that gets a non-2xx is retried, and eventually disabled by Jira.** So a
payload carrying no `fixVersion` returns **200** with `{"ignored":"no fixVersion"}` — "not
for me" is not an error. Do not change that to a 400.

The repository is derived as `<default-owner>/<lowercased project key>`, so `NEWS` →
`msnbc/news`. If their project keys do not map to repository names, that mapping needs a
Parameter Store entry like `properties` does.

---

## 6. Datadog, if they use it

The functions already emit structured JSON logs and `MONITORING|` metric lines, and the
templates take a `DatadogExtensionArn` which is attached conditionally. To turn it on:

1. Get the Datadog extension layer ARN for the region and architecture.
2. Pass it as `DatadogExtensionArn` to `20-lambda.yaml`.
3. Put the Datadog API key in Secrets Manager and reference it — **not** an environment
   variable, for the rotation/rollback reason in §1.

`DD_SERVICE`, `DD_ENV` and `DD_SITE` are already set per environment.

---

## 7. Prove the whole chain before saying it works

In order. Each step fails for a different reason, which is why they are separate:

```bash
bash scripts/preflight.sh            # before anything exists
bash scripts/probe-egress.sh         # the subnets actually reach GitHub and Slack
bash scripts/demo.sh                 # 13 behaviour checks, no AWS, no credentials
curl -s $BASE/prod/beta/health       # the deployed thing answers, and says which commit
bash scripts/slash.sh cut ...        # a signed command does real work
# then a commit, and watch it stop at the gate
```

⛔ **None of these is "the deploy went green".** Every one of them reads the running system.
`docs/WHAT-BROKE.md` is the list of times a green deploy meant nothing.
