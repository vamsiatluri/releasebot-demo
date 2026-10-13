# Account assessment — reference-working-env

| | |
| --- | --- |
| Source | `releasebot-sandbox` · us-east-1 · account 826653639065 |
| Name filter | _(everything)_ |
| Taken | 2026-10-13 01:49 UTC |

```

Baseline: reference-working-env
  account 826653639065  region us-east-1  captured 2026-10-13 01:48 UTC
  6 functions, 6 roles, 1 APIs, 13 log groups, 3 alarms
  6 blockers, 9 warnings, 12 informational
```

## Files

- `source.json` — what is actually running (no secret values)
- `source-review.txt` — what to look at before copying any of it
- `generated.yaml` — a starting-point template. **Read it, do not apply it.**

## Exit code: 1

**At least one blocker.** Migrating as-is would cause an incident.
