#!/usr/bin/env python3
"""Update a stack's TEMPLATE while keeping every parameter it already has.

⚠️ WHY THIS EXISTS. `aws cloudformation deploy` substitutes the template's
DEFAULT for any parameter you do not pass. So a deploy intended to add one
resource silently resets everything you omitted. That is not hypothetical: it
deleted this account's GitHub OIDC provider, and for two minutes no pipeline
run could have assumed its deploy role.

`update-stack` accepts UsePreviousValue, which `deploy` does not. This reads
the stack's current parameters and re-sends every one of them unchanged,
except the ones named on the command line.

    scripts/_update-stack.py <stack> <template> [Key=Value ...]
"""
import json, os, subprocess, sys

stack, template = sys.argv[1], sys.argv[2]
# ⚠️ A value of the form Key=@ENVVAR is read from the ENVIRONMENT, not from the
# command line. Anything on argv is visible in `ps` to every user on the box and
# lands in shell history; a secret must never travel that way. Same rule as
# scripts/set-approval-token.sh.
overrides = {}
for a in sys.argv[3:]:
    k, v = a.split("=", 1)
    if v.startswith("@"):
        name = v[1:]
        if name not in os.environ:
            print(f"{k}=@{name} but {name} is not set", file=sys.stderr)
            sys.exit(2)
        v = os.environ[name]
    overrides[k] = v

# ⚠️ THE STACK MAY NOT EXIST YET. This script was written against an account
# that already had everything, so it assumed describe-stacks would succeed --
# and it crashed on the FIRST deploy into an empty account, which is the one
# case it most needs to handle. Found by running bootstrap.sh in a brand-new
# account; no amount of re-reading it in a populated one would have shown this.
probe = subprocess.run(
    ["aws", "cloudformation", "describe-stacks", "--stack-name", stack,
     "--query", "Stacks[0].Parameters", "--output", "json"],
    capture_output=True, text=True)
exists = probe.returncode == 0

# ⚠️ ROLLBACK_COMPLETE IS A DEAD STACK. A create that fails leaves the stack in
# that state, and it can be neither updated NOR created -- so a retry loop
# hammers it forever, reporting "IAM not propagated" for something that will
# never succeed. It has no resources left; deleting it is safe and is the only
# way forward.
if exists:
    st = subprocess.run(
        ["aws", "cloudformation", "describe-stacks", "--stack-name", stack,
         "--query", "Stacks[0].StackStatus", "--output", "text"],
        capture_output=True, text=True).stdout.strip()
    if st in ("ROLLBACK_COMPLETE", "REVIEW_IN_PROGRESS"):
        print(f"  {stack} is in {st} — a dead stack; deleting it so it can be recreated")
        subprocess.run(["aws", "cloudformation", "delete-stack", "--stack-name", stack], check=True)
        subprocess.run(["aws", "cloudformation", "wait", "stack-delete-complete",
                        "--stack-name", stack], check=False)
        exists = False
        cur_override = []
if not exists and "does not exist" not in probe.stderr:
    # A failure for any OTHER reason (no credentials, wrong region, throttling)
    # must not be silently treated as "the stack is new" -- that would turn a
    # permissions problem into a create attempt with a confusing error.
    print(probe.stderr.strip(), file=sys.stderr)
    sys.exit(1)
cur = json.loads(probe.stdout or "[]") if exists else []

params = []
for p in cur:
    k = p["ParameterKey"]
    if k in overrides:
        params.append({"ParameterKey": k, "ParameterValue": overrides.pop(k)})
    else:
        params.append({"ParameterKey": k, "UsePreviousValue": True})
for k, v in overrides.items():          # genuinely new parameters
    params.append({"ParameterKey": k, "ParameterValue": v})

print(f"{stack}: {'CREATE' if not exists else 'update'} — "
      f"{sum(1 for p in params if p.get('UsePreviousValue'))} kept, "
      f"{sum(1 for p in params if 'ParameterValue' in p)} set "
      f"({', '.join(sorted(p['ParameterKey'] for p in params if 'ParameterValue' in p))})")

verb = "update-stack" if exists else "create-stack"
# ⚠️ A brand-new stack cannot carry UsePreviousValue -- there is no previous
# value. On a create, drop those entries and let the template's defaults apply.
if not exists:
    params = [p for p in params if "ParameterValue" in p]

r = subprocess.run(
    ["aws", "cloudformation", verb, "--stack-name", stack,
     "--template-body", "file://" + template,
     "--capabilities", "CAPABILITY_NAMED_IAM", "CAPABILITY_IAM",
     "--parameters", json.dumps(params)],
    capture_output=True, text=True)
if r.returncode != 0:
    if "No updates are to be performed" in r.stderr:
        print("  no changes"); sys.exit(0)
    print(r.stderr.strip(), file=sys.stderr); sys.exit(1)

waiter = "stack-update-complete" if exists else "stack-create-complete"
w = subprocess.run(["aws", "cloudformation", "wait", waiter, "--stack-name", stack],
                   capture_output=True, text=True)
if w.returncode != 0:
    # ⚠️ The waiter failing is not the useful information. WHY it failed is,
    # and it is in the stack events -- otherwise you get "Waiter failed" and
    # have to go looking.
    print("  stack did not reach a complete state:", file=sys.stderr)
    ev = subprocess.run(
        ["aws", "cloudformation", "describe-stack-events", "--stack-name", stack,
         "--query", "StackEvents[?contains(ResourceStatus,'FAILED')]"
                    ".[LogicalResourceId,ResourceStatusReason]", "--output", "text"],
        capture_output=True, text=True)
    print("\n".join("    " + l for l in ev.stdout.strip().splitlines()[:6]), file=sys.stderr)
    sys.exit(1)
print("  created" if not exists else "  updated")
