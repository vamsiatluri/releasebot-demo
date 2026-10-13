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
# ⛔ ANY command piped into `grep -q`, not just curl.
#
# The original rule matched `curl ... | grep -q` because that is where this bug
# was first found. It then MISSED `unzip -p ... | strings | grep -q`, which
# failed in exactly the same way and reported that a binary lacked a symbol it
# contained. The mechanism has nothing to do with curl: `grep -q` exits on the
# first match, everything upstream gets SIGPIPE, and under `set -o pipefail`
# the pipeline's status is that failure. So the rule is about `grep -q` at the
# END of a pipe, whatever is upstream.
#
# ⚠️ Deliberately NOT flagged: `grep -q` with no pipe into it (reading a file is
# fine), and `grep -q` that is not last in the pipeline.
pipefail_grepq = re.compile(r'\|\s*grep\s+(-[a-zA-Z]*q[a-zA-Z]*)\s')

def in_single_quotes(line, idx):
    """Is position idx inside a SINGLE-quoted string?

    ⚠️ The distinction matters and I got it wrong once in this file. Inside
    DOUBLE quotes a variable still expands, so `echo "$REPO..."` is the real bug
    -- that is exactly the form that broke set-write-token.sh. Only single
    quotes make it inert. A first attempt skipped every quoted string and
    silently disabled the check that had found the original defect: the guard
    went green by no longer looking.
    """
    return (line[:idx].count("'") - line[:idx].count("\\'")) % 2 == 1


def in_any_quotes(line, idx):
    """Is position idx inside any quoted string?

    ⚠️ Used ONLY for the pipefail rule, because this checker matched its own
    documentation: a message reading "no curl piped into grep -q" was reported
    as the defect it describes. A text-matching guard eventually matches the
    text that explains it, and a linter that cries wolf about a comment is one
    people switch off.
    """
    b = line[:idx]
    return (b.count("'") - b.count("\\'")) % 2 == 1 or \
           (b.count('"') - b.count('\\"')) % 2 == 1


bad = 0
for f in sorted(glob.glob("scripts/*.sh")):
    src = open(f, encoding="utf-8").read()
    pipefail = "pipefail" in src
    for n, line in enumerate(src.splitlines(), 1):
        m = unbraced.search(line)
        if m and not in_single_quotes(line, m.start()):
            print(f"{f}:{n}: ${m.group(1)} is followed by {m.group(2)!r} — "
                  f"bash reads that as part of the name. Use ${{{m.group(1)}}}.")
            bad += 1
        pg = pipefail_grepq.search(line)
        if pipefail and pg and not in_any_quotes(line, pg.start()):
            print(f"{f}:{n}: a pipe into `grep -q` under pipefail fails WHEN IT MATCHES — "
                  f"grep exits on the first hit and upstream gets SIGPIPE. "
                  f"Write to a file first, then grep the file.")
            bad += 1

print(f"  {len(glob.glob('scripts/*.sh'))} scripts checked, {bad} problem(s)")
sys.exit(1 if bad else 0)
