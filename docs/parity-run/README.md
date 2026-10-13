# The parallel run, proven end to end

**2026-10-13.** Two AWS accounts, captured and compared by `scripts/assess.sh`. This is the
Phase 4/5 shape from the SOW in miniature: read the source, build the target from templates,
capture both, and drive the differences to zero — keeping only the ones you intend.

| | account | how it was built |
|---|---|---|
| **A — source** | 826653639065 | accumulated over a week of hand deploys, like a real dev account |
| **B — target** | 450315222669 | `scripts/bootstrap.sh`, one command, from empty |

```
scripts/assess.sh releasebot-sandbox us-east-1 \
  --against releasebot-clean us-east-1 --out build/assess/parity
```

The target came up with **all seven stacks CREATE_COMPLETE on the first run**, the alias
read-back check passed for all four functions, and `/test` answered 200.

## ⛔ A separate ACCOUNT, not a second VPC

Two reasons, and the first was learned the expensive way:

- **IAM is global.** An earlier attempt ran the target in a second REGION of the same account
  and the execution role names collided — `releasebot-exec-cutrelease` can exist once per
  account, not once per region. A parallel run inside one account cannot use the real names.
- It is what the engagement actually is: `msnbc-dev` → `msnbc` prod are two accounts.

## The result: 27 differences, 10 blockers

### What the run FOUND, that nothing else would have

**★ Production and test are running different binaries in the source account.**

```
SOURCE                                          TARGET
cutRelease        emXCd2ym1cwVPMDp              cutRelease      6+jHsG60wNvR+pdo
cutReleaseTest    Qnm8g5UFMOQAQHOE              cutReleaseTest  6+jHsG60wNvR+pdo  ← same
```

The target shares one artifact across both environments — build once, deploy many, which is
the invariant the architecture exists to hold. The source does not, because its functions were
deployed at different moments. **That is exactly the drift a hand-maintained account
accumulates, and the comparison is the only thing that shows it.**

**The source runs the previous-generation runtime** — `provided.al2` against the template's
`provided.al2023`. Worth a separate, approved change rather than one bundled into a migration.

**Four orphaned log groups from throwaway probes** — `releasebot-netprobe`, `-goprobe`,
`-ghprobe`, `-egress-probe-45214`. The probe functions delete themselves; their log groups do
not. Same family as everything else in `WHAT-BROKE.md` §7.

### ⛔ And a defect in the comparator itself — 12 of the first 31 differences were noise

```
iam/releasebot-dashboard-ApiRole-X1Lyy4HP4DBG   A: absent    B: present
iam/releasebot-dashboard-ApiRole-ttjoWT4DNnuB   A: present   B: absent
```

The same role, from the same template, in two accounts. CloudFormation generates
`<stack>-<LogicalId>-<RANDOM>` whenever a template does not name a resource, and **that suffix
can never match across accounts** — so each such resource produced two permanent blockers.

★ That matters more than the count: the parallel run exists to drive differences to **zero** and
then trust the number. A comparator with a permanent floor of noise teaches people to ignore
its output. Fixed by comparing stack-owned resources on their logical name; **14 blockers → 10**,
and every survivor is real.

⚠️ The normalisation applies only to STACK-OWNED resources. A hand-made role that happens to end
in something suffix-shaped keeps its whole name — a false MATCH between two different roles is
the expensive direction, a false difference is merely noisy.

## How to re-run it

```
scripts/assess.sh <source-profile> <region> --against <target-profile> <region>
```

Exit 0 clean · 1 at least one blocking difference · 2 warnings only · 3 the tool failed. It is
read-only against both accounts and safe to point at production.
