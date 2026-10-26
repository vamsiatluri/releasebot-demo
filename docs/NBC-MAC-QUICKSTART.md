# Running the assessment on the NBC Mac

Everything here assumes **a locked-down corporate laptop with nothing installed**
except the AWS CLI. No Go, no Homebrew, no Claude, possibly not even git.

⚠️ **The whole assessment is read-only.** Every AWS call it makes is a `get-`,
`list-` or `describe-`. It creates nothing, changes nothing, deletes nothing.
That is what makes it safe to run on day one, during a change freeze, without
asking anyone's permission.

---

## Path A — the 3-minute one. Try this first.

Needs only `curl` and the AWS CLI.

```bash
mkdir -p ~/releasebot && cd ~/releasebot

curl -fsSL -o baseline \
  https://github.com/vamsiatluri/releasebot-demo/releases/download/v1.0.0/baseline-darwin-arm64
curl -fsSL -o SHA256SUMS \
  https://github.com/vamsiatluri/releasebot-demo/releases/download/v1.0.0/SHA256SUMS

# ⚠️ Verify before running. Takes two seconds and you are about to point this
# at someone else's production account.
shasum -a 256 -c SHA256SUMS --ignore-missing

chmod +x baseline
./baseline            # prints usage — if you see it, you are done setting up
```

> Intel Mac instead of Apple Silicon? Use `baseline-darwin-amd64`.
> Check with `uname -m` — `arm64` is Apple Silicon, `x86_64` is Intel.

### If macOS refuses to run it

> *"baseline cannot be opened because the developer cannot be verified"*

That is **Gatekeeper quarantine**, and it happens when a file arrives via a
browser. A `curl` download usually does not set it. Clear it with:

```bash
xattr -d com.apple.quarantine baseline
```

If MDM blocks unsigned binaries outright, `xattr` will not help — go to Path B.

---

## Day one, in order

```bash
cd ~/releasebot

# 1. ⭐ ALWAYS FIRST. Can this role actually read the account?
./baseline preflight -profile msnbc-dev -region us-east-1
```

```
exit 0   everything needed is readable            → carry on
exit 1   a REQUIRED read is denied                → it NAMES the missing IAM actions.
                                                    That list IS your morning: send it on.
exit 2   only the Phase 7 (decommission) reads    → carry on. Fix before the teardown.
         are missing
```

★ **A denial on day one is a result, not a setback.** "Here is exactly what I can
and cannot see in your account" is a far better first contact with a platform
team than "please give me read-only".

```bash
# 2. The inventory.
./baseline capture -profile msnbc-dev -region us-east-1 \
    -label msnbc-dev -out msnbc-dev.json

# 3. What a person should look at — graded BLOCKER / WARN / INFO.
./baseline review -in msnbc-dev.json | tee msnbc-dev-review.txt

# 4. A DRAFT template of what is actually there.
#    ⛔ Read it. Do not apply it. It faithfully reproduces whatever the account
#    accumulated by hand, which is the opposite of what a migration is for.
./baseline generate -in msnbc-dev.json -out msnbc-dev-generated.yaml
```

### Then do the same to the target account

The production account already exists and already holds other workloads, so this
is worth more than it sounds:

```bash
./baseline capture  -profile msnbc -region us-east-1 -label msnbc -out msnbc.json
./baseline compare  -a msnbc-dev.json -b msnbc.json | tee differences.txt
```

★ *"Here is what is already in the production account, and here is where
ReleaseBot would collide with it"* is a strong week-one artifact, and nobody will
have asked you for it. Stack names, IAM role names and log group names are all
account-global — a collision with a workload you did not build is found either by
you now, or by CloudFormation later.

### ⛔ Do not use `-prefix` on the first run

`-prefix releasebot` once excluded **five of six functions**, because they are
named `cutRelease`, `releaseAutomationMergeback` and so on. The report then read
like a confident, complete assessment of one sixth of the system.

### Later — the decommission (Phase 7)

```bash
./baseline capture  -estate -profile msnbc-dev -region us-east-1 \
                    -label msnbc-dev -out estate.json
./baseline teardown -in estate.json -out teardown.md
```

`-estate` adds the eleven services a decommission needs and a migration baseline
does not. `teardown` **refuses** a capture taken with `-prefix` — a teardown
built from a partial inventory deletes what matched and leaves the rest billing,
while reporting success.

---

## Path B — if the binary will not run

Build it yourself. Needs Go 1.22+ and git.

```bash
# Go, if it is not there. No admin rights needed — it installs into your home dir.
curl -fsSL -o /tmp/go.tar.gz https://go.dev/dl/go1.23.4.darwin-arm64.tar.gz
mkdir -p ~/.local/lib && tar -xzf /tmp/go.tar.gz -C ~/.local/lib
export PATH="$HOME/.local/lib/go/bin:$PATH"
go version

git clone https://github.com/vamsiatluri/releasebot-demo.git ~/releasebot-src
cd ~/releasebot-src
go build -o baseline ./cmd/baseline
./baseline preflight -profile msnbc-dev -region us-east-1
```

⚠️ If Homebrew is blocked, the tarball above still works — it is a plain
extract into your home directory and needs no privileges.

No git either? Download the repo as a zip from the GitHub web UI and unzip it.

---

## Path C — GitHub Actions

⚠️ **Read this before reaching for it.** Running in Actions needs an **OIDC role
in NBC's AWS account that trusts your GitHub repo** — which is *more* access to
arrange, and more explaining, than running read-only from your own laptop. It is
not the easy fallback it looks like.

It is genuinely useful for one thing: producing an assessment on a **schedule**,
or where the output needs to land somewhere the team can see without you sending
files around.

If it comes to that, the workflow is `.github/workflows/assess.yml` in the repo —
it takes a profile/account and region, runs preflight then capture then review,
and uploads the results as a build artifact.

---

## Troubleshooting

**`Unable to locate credentials` / `could not authenticate`**
`aws sts get-caller-identity --profile msnbc-dev` first. If that fails, nothing
else will — it is an AWS CLI config problem, not a tool problem.

**`aws: command not found`**
```bash
curl -fsSL -o /tmp/awscli.pkg https://awscli.amazonaws.com/AWSCLIV2.pkg
installer -pkg /tmp/awscli.pkg -target CurrentUserHomeDirectory
```
Needs no admin rights with `CurrentUserHomeDirectory`.

**The CLI returns YAML and something fails to parse**
Some configs set `output = yaml`. The tool passes `--output json` on every call
so it is immune — but if *you* are piping `aws` into `jq`, pass `--output json`
explicitly. The CLI exits 0 and writes valid YAML, so the failure surfaces later
and somewhere else.

**It worked yesterday and now says AccessDenied**
Re-run `preflight`. SSO sessions expire, and an expired session reports as a
denial. The tool separates DENIED from FAILED for exactly this reason — a
throttle, an expired token, or a service absent in the region are not permission
problems and are not fixed by granting anything.

---

## What to hand them at the end of week one

Not the JSON. One page, in their language:

1. **What is actually there** — functions, roles, API, stages, what each is wired to.
2. **What no CloudFormation stack owns.** This is the finding. A hand-made resource
   is the thing that will not come across, and it is invisible to anyone reading
   the templates.
3. **Where the secrets live, and in what form.** A literal in an environment
   variable is a blocker, not a note.
4. **Everything pointing AT the API that nobody has written down** — the Slack
   slash commands and the interactivity URL, the Jira webhook, runbooks,
   bookmarks. ⚠️ The invoke URL contains the API id, so a new account means a new
   URL. **That list IS the cutover.**
