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
import json, subprocess, sys

stack, template = sys.argv[1], sys.argv[2]
overrides = dict(a.split("=", 1) for a in sys.argv[3:])

cur = json.loads(subprocess.run(
    ["aws", "cloudformation", "describe-stacks", "--stack-name", stack,
     "--query", "Stacks[0].Parameters", "--output", "json"],
    check=True, capture_output=True, text=True).stdout or "[]")

params = []
for p in cur:
    k = p["ParameterKey"]
    if k in overrides:
        params.append({"ParameterKey": k, "ParameterValue": overrides.pop(k)})
    else:
        params.append({"ParameterKey": k, "UsePreviousValue": True})
for k, v in overrides.items():          # genuinely new parameters
    params.append({"ParameterKey": k, "ParameterValue": v})

print(f"{stack}: {sum(1 for p in params if p.get('UsePreviousValue'))} kept, "
      f"{sum(1 for p in params if 'ParameterValue' in p)} set")

r = subprocess.run(
    ["aws", "cloudformation", "update-stack", "--stack-name", stack,
     "--template-body", "file://" + template,
     "--capabilities", "CAPABILITY_NAMED_IAM", "CAPABILITY_IAM",
     "--parameters", json.dumps(params)],
    capture_output=True, text=True)
if r.returncode != 0:
    if "No updates are to be performed" in r.stderr:
        print("  no changes"); sys.exit(0)
    print(r.stderr.strip(), file=sys.stderr); sys.exit(1)

subprocess.run(["aws", "cloudformation", "wait", "stack-update-complete",
                "--stack-name", stack], check=True)
print("  updated")
