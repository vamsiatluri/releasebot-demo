#!/usr/bin/env python3
"""Build a CloudFormation --parameters file that changes ONLY the approval token.

Every other parameter is carried forward with UsePreviousValue, so this cannot
silently reset the signing secret or the approver list -- which an
`update-stack` with a hand-written parameter list very easily does.

The token arrives in the environment and is written to a file the caller
created with mode 600. It is never a command-line argument: arguments are
visible in `ps` to every other process on the machine.
"""
import json
import os
import subprocess
import sys

stack = os.environ["STACK"]
region = os.environ["REGION"]
dest = os.environ["PARAMS"]
token = os.environ["TOKEN"]

raw = subprocess.run(
    ["aws", "cloudformation", "describe-stacks",
     "--stack-name", stack, "--region", region,
     "--query", "Stacks[0].Parameters", "--output", "json"],
    capture_output=True, text=True, check=True,
).stdout

keys = [p["ParameterKey"] for p in json.loads(raw)]
if "GitHubApprovalToken" not in keys:
    sys.exit("stack %s has no GitHubApprovalToken parameter" % stack)

params = [{"ParameterKey": k, "UsePreviousValue": True}
          for k in keys if k != "GitHubApprovalToken"]
params.append({"ParameterKey": "GitHubApprovalToken", "ParameterValue": token})

with open(dest, "w") as fh:
    json.dump(params, fh)
