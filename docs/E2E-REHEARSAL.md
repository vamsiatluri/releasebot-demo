# End-to-end rehearsal: ReleaseBot standing in for the production release bot

ReleaseBot can run **unchanged** under the migration Terraform (the `robbie-env`
and `slack-approve` modules) and behave like the production release bot closely
enough to rehearse a full cutover in a personal account: Slack commands, the Jira
mergeback webhook, the async SQS path on test, and Slack approval of a prod deploy.

This is this repository's own implementation. It reads the same **config data
format** (the rarc YAML: `github` / `slack` / `jira` / `projects`); no code from
the production bot is used, and every value in fixtures and examples is invented.

## How the mode is chosen

`cmd/cutrelease` and `cmd/mergeback` switch on one variable:

| `ROBBIE_CONFIG_URL` | Behaviour |
|---|---|
| set (the Terraform always sets it) | config-driven release bot: `internal/robbie` |
| unset | the original demo (`/cut <repo> <version>`, ledger, …): unchanged, as are its harness and tests |

## Build

```bash
make package-e2e      # build/cutrelease.zip, build/mergeback.zip, build/slackapprove.zip
```

All three: `bootstrap` at the zip root, linux/amd64, CGO off. The cut and mergeback
zips fit `provided.al2` (the handler name is ignored). The slackapprove zip fits
`provided.al2023`. Point the Terraform's `slack_approve_zip` at
`build/slackapprove.zip`. (`make package` alone still builds only cut and mergeback,
and removes any other zip, as before.)

## Routes

| API / stage | Route | Function | What it accepts |
|---|---|---|---|
| prod `beta` | `POST /cut` | cutRelease | Slack Events API (`url_verification`, `app_mention`, DM `message`) and slash commands |
| prod `beta` | `ANY /mergeback` | releaseAutomationMergeback | Jira automation JSON. The `x-api-key` is checked by API Gateway, not by the code |
| prod `beta` | `POST /interact` | slackApprove | Slack `block_actions` from the deploy card |
| test `test` | `POST /cut`, `ANY /cutReleaseTest` | cutReleaseTest | same as prod `/cut`; acks and enqueues when `ASYNC_ENQUEUE_MODE=sqs` |
| test `test` | `ANY /mergeback` | releaseAutomationMergebackTest | same as prod |
| (SQS) | event source mapping → `cutReleaseTest:live` | cutReleaseTest | SQS events carrying queued Slack commands |

Every function also answers `…/health` (or `?health=1`) with mode, env, account
label, config source and project count, and where each secret came from
(`env` / `ssm-param` / `ssm-ref`; never the value). It returns 503 if the
environment is incomplete or the config can't be loaded.

## Environment variables read

| Variable | Used for | Notes |
|---|---|---|
| `ROBBIE_CONFIG_URL` | rarc YAML: `https://` (GitHub raw: sent with `GITHUB_TOKEN`), `s3://bucket/key` (SigV4, the role's credentials), or a local path | Loaded **once per container**. A failed load is retried on the next request; only a good document is cached |
| `ROBBIE_CONFIG_URL_BACKUP` | tried if the primary fails | The production bot doesn't read it. Here it is a fallback |
| `ROBBIE_ENV` | `prod` / `test` | fallback: the demo's `RARC_ENV` |
| `ROBBIE_SSM_ROOT` | reported on `/health` | |
| `GITHUB_TOKEN`, `SLACK_TOKEN`, `JIRA_TOKEN`, `DD_API_KEY` | secrets | each is read as `NAME` (a literal, or `ssm:/path`), else `NAME_SSM_PARAM` (a SecureString name, decrypted). That covers both `secret_mode = "env"` and `"ssm"` |
| `SLACK_SIGNING_SECRET`, `SLACK_SIGNING_SECRET_VERSION_NUMBER` | Slack signature (`v0` if unset) | required on cut, not on mergeback (the Terraform doesn't give mergeback one) |
| `JIRA_EMAIL`, `JIRA_ACCOUNT_ID` | Jira Basic auth; release ticket reporter | fallback for email: `JIRA_USER` |
| `DD_ENV`, `DD_TRACE_ENABLED` | read and reported | no Datadog client: logs are structured JSON on stdout |
| `ASYNC_ENQUEUE_MODE=sqs`, `ASYNC_QUEUE_URL` | ack Slack, `SendMessage` to the queue, then process the SQS event | queue calls use SigV4 and the AWS JSON protocol |
| `RELEASEBOT_ACCOUNT_LABEL` | shown on `/health` and in the context line under "… complete" messages | optional |
| `RELEASEBOT_DRY_RUN=1` | no GitHub, Jira or Slack writes | optional |
| `RELEASEBOT_GITHUB_API_URL`, `SLACK_API_URL`, `JIRA_API_URL` | point the clients at local mocks | **local only**. The Jira base URL comes from `jira.baseURL` in the config, as in the production bot. `JIRA_API_URL` only overrides it for API calls |

slackApprove reads `SLACK_SIGNING_SECRET_SSM_PARAM`,
`GITHUB_APPROVAL_TOKEN_SSM_PARAM` (or the demo's `SLACK_SIGNING_SECRET` /
`GITHUB_APPROVAL_TOKEN`, as a literal or `ssm:`), `GITHUB_REPO` as `owner/name` (or the
demo's `GITHUB_OWNER` plus `GITHUB_REPO=name`), `GITHUB_ENVIRONMENT` (a name,
default `production`; the id is looked up from the run's pending deployments, or
taken from `GITHUB_ENVIRONMENT_ID` if set) and `SLACK_APPROVERS` (empty = nobody).

## Config

Same structure and field names as the config repo's schema. Keys are matched
case-insensitively (real files mix `doneStateID` and `doneStateId`). Project lookup
is case-insensitive. Two additions, both allowed by the schema's
`additionalProperties`:

```yaml
jira:
  baseURL: https://<your-site>.atlassian.net
  qaStateId: 31        # transition id for "moved to QA" on cut
  doneStateID: 41      # transition id for "done" on mergeback
  fields:              # custom fields on the release ticket. NO defaults
    platforms: customfield_10044     # [{"id": project.jira.platform}]
    risk: customfield_10045          # {"id": project.jira.releaseIssueDefaultRisk}
    notes: customfield_10046         # the release notes (ADF, since this uses API v3)
    envVarChange: customfield_10047  # {"value": "Yes"|"No"}
```

A project's `jira.envVarChangeFieldKey` overrides `jira.fields.envVarChange`. A field
with no key configured is **left off** the ticket and a warning is logged. It is never
sent to a guessed id. The ticket's `description` always carries the notes too.
See `internal/rarc/testdata/rarc-example.yml` for a full invented example.

## Behaviour compared with the production bot

| Command / path | Status | What it does here |
|---|---|---|
| `ping`, `version`, `help`, `projects`, `release-please` | ✅ | informational. Also allowed in a DM |
| `precut <app>` | ✅ | PRs to the default branch with `github.mergelabel` (`none` = no label needed), without `excludelabels`, with ≥ `approvalsneeded` approvals (latest review per reviewer). Then the commits on default but not release (`compare release...default`) |
| `cut <app>` / `recut <app>` | ✅ | squash-merges the ready PRs (mergelabel / recutlabel). Failures go to the team channel as a list. Then `POST /merges` default → release branch. Changelog since tag `v<version from filesWithVersion[0] on release>` goes to the team channel with QA mentions, plus a "View tickets in JIRA" link. Keys from `issueIDFormat` (project, else global), filtered by `filterIssuesQuery` through `POST /rest/api/3/search/jql`, then moved with `qaStateId` (per board / issue type) |
| `release <app> patch\|minor\|major [hasEnvChanges]` | ✅ | "Starting *patch* release for X, old version: *1.4.0* new version: *1.4.1*" plus `1.4.0 → 1.4.1 (patch)`. Bumps every `filesWithVersion` file on the release branch in one commit (package.json, lockfile v2/v3 twice, YAML `version:`). Annotated tag `vX`, GitHub release whose notes **start with `vNEW - https://github.com/ORG/REPO/compare/vOLD...vNEW`** (the deploy card parses this), the changelog, and a Jira `id in (…)` link. Then the Jira release ticket (`releaseIssueTitleFormat`, issue type, project, risk / platforms / notes / env-var change as configured), "We're ready for a *X* release <ticket>…" in `slack.releasechannel`, channel topic `{version}` update, and "Release *1.4.1* complete" with compare / release / ticket links. If a step fails, the reply lists which steps passed (✅ tag / ✅ release / ❌ ticket) |
| `autoDetectEnvVarChange` | ✅ | a release is flagged Yes if any of its issues has the env-var field = Yes (search/jql) |
| `jiraticket <app> [hasEnvChanges]` | ✅ | ticket for the latest GitHub release |
| `mergeback <app>` (Slack) and `POST /mergeback` (Jira) | ✅ | Jira: the project is found by matching `issue.fields.summary` against each `releaseIssueTitleFormat` (the format without `%s` must be in the summary; the longest match wins). It needs `automergeback: true`. No match → **400 with a JSON error** naming the summary, not a crash. Keys come from the release ticket's notes field (or description), else from the commits between the last two `v` release tags (pre-release tags skipped). Moved with `doneStateID`; fixVersion `v<ver> <app>` created in `jira.projectID` and added. PRs release → `masterBranch` (if set, with `masterMergebackLabels`) and release → default (`developMergebackLabels`), merged with a merge commit when mergeable or `blocked`. Otherwise "Please review today's mergebacks" with `devteammention` |
| Slack plumbing | ✅ | signature check (version from env, plus a 5-minute replay window the production bot doesn't have). `url_verification` is answered (the production bot doesn't). `X-Slack-Retry-Num` is acknowledged and dropped. Edits, bot messages, the bot's own user and non-DM `message` events are ignored. Release commands in a DM are refused. The asker is tagged as `<@U…>` except in DMs |
| async (test) | ✅ | with `ASYNC_ENQUEUE_MODE=sqs`: verify, parse, enqueue, answer 200 straight away (slash: "Working on it…"). If the enqueue fails, it runs inline. The SQS handler runs each job and returns no `batchItemFailures` (see gaps) |
| slackApprove | ✅ | `robbie_deploy_approve` / `robbie_deploy_reject`, value = workflow run id (digits only). Other `action_id`s are ignored before the approver check. Approver allowlist (empty = nobody). Environment id looked up via `GET …/actions/runs/{id}/pending_deployments` (needs `current_user_can_approve`), then `POST` approved / rejected with "… from Slack by <user> (<id>)". The card is replaced through `response_url` |
| `cherry-pick` | ❌ | parsed. Answers "not supported" (the production bot parses it but doesn't dispatch it either) |

## Known gaps and differences

- **Stacked PRs** (stack detection, "N of M layers ready", stack preflight checks) and
  **required-status-check diagnostics** for failed merges are not implemented. Failures
  report GitHub's error or mergeable state.
- **Reverts** aren't grouped separately in the changelog.
- **Datadog**: no tracer or Datadog log links. The context line under "… complete"
  shows account / env / request id instead.
- **Slack retries** get 200 plus `X-Slack-No-Retry: 1`; the production bot answers 401.
  The prod functions run synchronously (no async in the Terraform), so a long cut still
  exceeds Slack's 3 seconds on a slash command, as it does today.
- **SQS failures aren't redelivered** (a half-done release must not rerun). They're
  logged and reported in Slack. The queue's DLQ only sees messages Lambda itself fails
  to process.
- **Mergeback from Jira** posts to the project's team channel (there's no Slack origin).
- **Jira API v3**: notes and description are sent as ADF. A Jira whose notes field is a
  single-line text field would need a plain string (not supported).
- The **ledger** (DynamoDB) isn't written in this mode: the migration Terraform grants
  no DynamoDB access.
- `ROBBIE_SSM_ROOT` and `DD_*` are read and reported only. The Parameter Store
  `settings` block the Terraform creates is not read: the github / slack / jira blocks
  come from the config URL.

## Tests

`go test ./...` covers: config parsing of the invented fixture (case-insensitive keys,
string or int ids, per-board transitions, field keys, validation, backup URL, retry
after a failed load, token only to GitHub raw hosts), command parsing, version bump
and the version jump string, version-file rewriting, Jira key extraction, title-format
matching, Slack inbound parsing and signature checks (forged, stale, lower-case
headers, `url_verification`, retries), the async enqueue → SQS path, each flow against
in-memory GitHub / Jira / Slack fakes (including the release-notes first line and the
ticket fields), the Jira client against an HTTP mock (v3 paths, Basic auth, search/jql
pagination, and a local refusal of `/rest/api/{2,3}/search`), and slackApprove against
a mock `pending_deployments` API.
