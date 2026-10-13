# What survives the teardown, and how to bring it back

The sandbox account is being torn down after the interview. **Everything needed to
rebuild it, or to compare it against someone else's account, is in this repository.**
Nothing of value lives only in AWS.

---

## 1. The reference capture — the thing to diff NBC against

`docs/reference-capture/` is a snapshot of a **known-good, working ReleaseBot
environment**, taken from the live account on 2026-10-12 before teardown.

| file | what it is |
|---|---|
| `source.json` | every resource the baseline tool could read — functions, versions, aliases, roles, inline policies, API stages, alarms |
| `source-review.txt` | the same, reviewed: 6 blockers, 9 warnings, 12 informational |
| `generated.yaml` | a CloudFormation template **drafted from what was actually there** — read it, do not apply it |
| `SUMMARY.md` | the headline numbers |
| `parameters-prod.json` / `parameters-test.json` | the per-environment configuration from Parameter Store — the VALUES the templates do not carry |

⚠️ **Verified to contain no credential values** before committing: scanned for `ghp_`,
`github_pat_`, `xox*`, Slack webhook URLs and `AKIA*`. Clean. Parameter Store here holds
configuration, not secrets — the two `*-token-ref` entries are POINTERS, by design.

### How to use it against their account

```bash
# Day one: what is actually in msnbc-dev?
scripts/assess.sh msnbc-dev us-east-1 --prefix release

# Then diff THEIR dev against this known-good reference
scripts/assess.sh msnbc-dev us-east-1 \
  --against msnbc-prod us-east-1 --label msnbc-migration
```

★ The reference is what makes a difference *meaningful*. "Their account has 6 blockers" is
a number; "their account has 6 blockers and a working one has these 6" is a comparison.

---

## 2. Rebuilding the environment from the repository

```bash
# Everything, including the VPC (~$47/month while it runs)
AWS_PROFILE=<profile> ADMIN_EMAIL=you@example.com SLACK_APPROVERS=U123 \
  bash scripts/sandbox-up.sh

# Cheaper: no VPC, no NAT. The functions run on the AWS-managed network,
# which is what they did before the VPC existed.
NETWORK=0 AWS_PROFILE=<profile> bash scripts/sandbox-up.sh
```

⚠️ **This script was wrong until 2026-10-12 and it is worth knowing why.** It rebuilt four
of the eight templates. The VPC, the Slack approver and the dashboard had been deployed by
hand during development and never added to it — so "the code is preserved" would have been
true and the environment still would not have come back. **An environment that cannot be
rebuilt from the repository only exists in the account**, which is precisely the condition
this whole project is about migrating away from.

### Then the two credentials, which are deliberately NOT in git

```bash
bash scripts/set-approval-token.sh   # CLASSIC PAT      -> slackApprove only
bash scripts/set-write-token.sh      # FINE-GRAINED PAT -> the four functions
```

Each refuses the other's token. See the header of either script for why the approval
endpoint accepts only a classic PAT, and why that is not a permission you can add.

---

## 3. Taking it down

```bash
bash scripts/network-down.sh   # the VPC: detach, re-publish, wait for the ENIs, delete, VERIFY
bash scripts/sandbox-down.sh   # everything else, and prove each resource class is empty
```

⛔ Order matters. A VPC cannot be deleted while a Lambda ENI is attached, and Lambda does
not release those immediately — `network-down.sh` waits, then requires `describe-stacks`
to FAIL by name before it reports success. A teardown that trusts `delete-stack`'s exit
code is a teardown that reports success having deleted nothing.

⚠️ `releasebot-ledger` is `DeletionPolicy: Retain`. It is an audit record of what shipped,
and a stack delete that silently takes the release history with it is the behaviour you
discover during the audit. Remove it by hand if you actually want it gone.

---

## 4. What is NOT preserved, and why that is fine

| | |
|---|---|
| the DynamoDB ledger rows | demo data; the table is recreated empty and fills on first use |
| CloudWatch logs | 7-day retention anyway |
| the Cognito user | recreated by the dashboard deploy; a new temporary password is emailed |
| the API Gateway id | ⚠️ **changes on rebuild** — Slack and Jira point at a URL containing it. That is the migration lesson in miniature, and the reason `infra/30-apigw.yaml` carries a custom-domain option. |
| the two tokens | deliberately not in git |

---

## 5. If this goes to NBC production

Do not deploy these templates at them. They describe a *model* of ReleaseBot built from the
SOW, not their system. The order of operations is:

1. `scripts/assess.sh` against **their** `msnbc-dev` — read what is actually there
2. Diff against `docs/reference-capture/` — what does a working one look like
3. Reconcile the differences into their templates, not ours
4. Only then use the pipeline shape: build once, deploy to test, verify the fingerprint,
   hold for a human, promote the same artifact

★ The transferable parts are the **pipeline and the verification**, not the infrastructure.
`docs/WHAT-BROKE.md` is the list of things that will bite in their account too.
