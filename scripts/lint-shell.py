#!/usr/bin/env python3
"""Catch the shell traps that `bash -n` cannot see.

⚠️ `bash -n` parses a script and reports NOTHING for either of these, because
both are syntactically valid. They fail at runtime, on the one line a human
reads straight past.

1. A $VAR immediately followed by a NON-ASCII character. Bash reads the
   character as part of the NAME, so `$REPO…` is the variable `REPO…` -- and
   under `set -u` the script dies with "REPO?: unbound variable", naming a
   variable that does not appear anywhere in the file. The ellipsis looks like
   punctuation and reads as punctuation; it is an identifier character to bash.

2. `curl ... | grep -q` under `set -o pipefail`. grep exits on the first match,
   curl dies on the broken pipe, and pipefail takes the pipeline's status from
   curl -- so the check FAILS exactly when it SUCCEEDS.
"""
import glob, re, sys

unbraced = re.compile(r'(?<!\$)\$([A-Za-z_][A-Za-z0-9_]*)([^\x00-\x7F])')
pipefail_grepq = re.compile(r'curl[^|\n]*\|\s*grep\s+-q')

bad = 0
for f in sorted(glob.glob("scripts/*.sh")):
    src = open(f, encoding="utf-8").read()
    pipefail = "pipefail" in src
    for n, line in enumerate(src.splitlines(), 1):
        m = unbraced.search(line)
        if m:
            print(f"{f}:{n}: ${m.group(1)} is followed by {m.group(2)!r} — "
                  f"bash reads that as part of the name. Use ${{{m.group(1)}}}.")
            bad += 1
        if pipefail and pipefail_grepq.search(line):
            print(f"{f}:{n}: `curl | grep -q` under pipefail fails WHEN IT MATCHES. "
                  f"Fetch to a file, then grep the file.")
            bad += 1

print(f"  {len(glob.glob('scripts/*.sh'))} scripts checked, {bad} problem(s)")
sys.exit(1 if bad else 0)
