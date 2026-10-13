#!/usr/bin/env python3
"""Remove every object version and delete marker from a bucket.

⚠️ WHY THIS IS NOT `aws s3 rm --recursive`. That removes CURRENT objects only.
A versioned bucket still holds the old versions and a delete marker for each
object removed, and a bucket holding either is NOT empty -- so CloudFormation's
stack delete fails with a 409 that says "the bucket you tried to delete is not
empty" about a bucket `s3 ls` reports as empty.

Found when the dashboard stack hit DELETE_FAILED on its own site bucket while
every other stack deleted cleanly.
"""
import json
import subprocess
import sys

bucket = sys.argv[1]
removed = 0

while True:
    raw = subprocess.run(
        ["aws", "s3api", "list-object-versions", "--bucket", bucket,
         "--max-items", "500", "--output", "json"],
        capture_output=True, text=True)
    if raw.returncode != 0:
        break
    page = json.loads(raw.stdout or "{}")
    items = [{"Key": o["Key"], "VersionId": o["VersionId"]}
             for o in (page.get("Versions") or []) + (page.get("DeleteMarkers") or [])]
    if not items:
        break
    # delete-objects takes at most 1000 keys per call.
    for i in range(0, len(items), 900):
        subprocess.run(
            ["aws", "s3api", "delete-objects", "--bucket", bucket,
             "--delete", json.dumps({"Objects": items[i:i + 900], "Quiet": True})],
            capture_output=True)
    removed += len(items)

if removed:
    print(f"    removed {removed} object version(s)/delete marker(s)")
