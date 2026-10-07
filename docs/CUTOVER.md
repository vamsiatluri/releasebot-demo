# Cutover and rollback

The cutover is not an AWS change. By the time it happens both accounts have been
deployed and validated for weeks. **The cutover is a handful of URL fields in
other people's systems** — which is good news, because every one of them is a
single-field edit and therefore a single-field rollback.

Run it in a window when no release is in flight. Confirm that in the release
channel before starting, do not assume it.

## Pre-flight

- [ ] Production acceptance evidence complete (MIGRATION-PLAN Phase 5 table)
- [ ] No release in flight — confirmed in `#release`, with a name attached
- [ ] Current Slack slash-command Request URLs **captured verbatim**, both
      `/cut` and `/merge`, prod and test
- [ ] Current Slack interactivity Request URL captured
- [ ] Current Jira webhook URL and secret captured
- [ ] Dev-account function versions recorded, so rollback does not depend on the
      dev pipeline still working
- [ ] Rollback owner named, and awake
- [ ] CloudWatch + Datadog dashboards open for **both** accounts

## Order of operations

The order is chosen so that the first irreversible-ish step is as late as
possible, and so that the system is never in a state where *both* bots are live
on the same trigger.

1. **Freeze the dev pipeline.** Set `migration_phase: prod-only` — stop
   deploying to dev, but leave what is deployed there running. The rollback
   target must not move during the cutover.
2. **Switch the TEST surfaces first.** Point the Slack test slash command and
   the Jira test webhook at the production account's `test` stage. Exercise a
   full cycle on the shadow repository. If anything is wrong, this is where it
   shows up, and nothing production depends on it.
3. **Switch the production Slack slash command Request URLs** — `/cut`, then
   `/merge`. One at a time, each verified before the next.
4. **Switch the Slack interactivity Request URL.** Easy to forget: the slash
   command works, and buttons silently do nothing because the interactivity URL
   still points at an account that no longer has a listening function.
5. **Switch the Jira webhook URL and rotate its shared secret.**
6. **Rotate the Slack signing secret**, production account first. Verify an
   old-secret signature is now rejected — rotation that leaves the old secret
   valid is not rotation.
7. **Rotate the GitHub token** to a new one issued for the production account,
   and confirm the old one is rejected by GitHub. The audit trail should now
   attribute release-branch creation to the production identity.
8. **Run one full real release cycle**: cut, verify the branch, mergeback,
   verify the PR, confirm the Slack message, confirm the Jira transition,
   confirm Datadog received metrics and CloudWatch has the structured logs.
9. **Remove the parallel deploy jobs** — but only after the full cycle passes,
   and in a separate commit, so reverting it is a revert.

## Rollback

**Valid until Phase 7 begins.** The dev stack stays deployed and untouched for
the entire acceptance period; that is what makes this a rollback rather than a
re-migration.

| Symptom | Action | Time |
|---|---|---|
| Bad deploy in the production account | `aws lambda update-alias --function-name <fn> --name live --function-version <previous>` | seconds |
| The bot misbehaves after cutover | Repoint the Slack Request URLs to the dev account's invoke URL | ~2 minutes |
| Jira integration broken | Repoint the Jira webhook URL and restore the old secret | ~2 minutes |
| Production account unreachable | All of the above; dev is still fully deployed | ~5 minutes |

The reason the captured URLs are a pre-flight checklist item rather than
something to look up during an incident: at the moment you need them, the
console you would look them up in is the one that is broken.

### What rollback does *not* undo

- A release branch already created. GitHub state is real state; a rollback of
  the bot does not un-cut a release.
- A rotated GitHub token. Rolling back the bot means reissuing a token for the
  dev identity, not restoring the old one.

Both are arguments for doing the rotations (steps 6–8) **after** the URL
switches have been verified, not before.

## Decommission gate

Do not start Phase 7 until **all** of these hold:

- [ ] Production has served at least one full release cycle with no manual
      intervention
- [ ] An agreed soak period has passed with no incidents (two release cycles, or
      two weeks, whichever is longer)
- [ ] Application owners have signed off in writing
- [ ] Runbooks updated to the new URLs, and the old URLs removed from them
- [ ] Dashboards and alarms repointed; the dev alarms are confirmed silent
      *because nothing is happening*, not because they were deleted

Then, and only then, delete the dev resources — and verify each deletion per
MIGRATION-PLAN Phase 7 rather than trusting the console's optimism.
