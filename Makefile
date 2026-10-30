# ReleaseBot build + local environment.
#
# The packaging rules are the ones that matter for provided.al2:
#   - the executable MUST be named `bootstrap`
#   - it MUST sit at the ROOT of the zip, not under a directory
#   - it MUST be linux/amd64 (this repo is developed on arm64 macOS, so GOARCH
#     is explicit; a darwin/arm64 binary in the zip fails at INIT with
#     "fork/exec /var/task/bootstrap: exec format error" and nothing else)
#   - CGO off, so it does not link against a glibc the sandbox may not have

GOOS        ?= linux
GOARCH      ?= amd64
BUILD_DIR   ?= build
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
BUILT       ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG         := github.com/vamsiatluri/releasebot-migration/internal/handler
LDFLAGS     := -s -w -X $(PKG).BuildCommit=$(COMMIT) -X $(PKG).BuildTime=$(BUILT)

# ⛔ FUNCTIONS IS NOT EVERY cmd/. slackapprove and dashboardapi are deployed by
# their own scripts, which build fresh into a temp directory -- deliberately,
# because they are owned by different stacks with different parameters.
#
# THE TRAP THAT COST AN HOUR: `build/slackapprove.zip` existed on disk from a
# hand-build weeks earlier. `make package` did not rebuild it (not in this
# list) and did not remove it, so a later hand-upload took a STALE artifact.
# Its content-addressed digest matched the old one -- correctly, the bytes
# really were the old bytes -- so CloudFormation saw no change and the function
# kept running code from before internal/secretref existed. Meanwhile the
# template had switched its env var to `ssm:/releasebot/.../slack-signing-secret`,
# and the OLD code read that PATH as the signing secret. Every Slack signature
# failed with a 401 and nothing anywhere said why.
#
# ★ Content addressing proves an artifact is CONSISTENT. It cannot tell you it
# is CURRENT. The `prune-stale` target below is what closes that gap.
FUNCTIONS := cutrelease mergeback
# Built elsewhere, listed here so prune-stale knows they are not abandoned.
EXTERNALLY_BUILT := slackapprove dashboardapi

.PHONY: all build package package-e2e test lint clean local mocks demo verify-zip prune-stale

all: test build

build:
	@mkdir -p $(BUILD_DIR)
	@for f in $(FUNCTIONS); do \
	  echo "building $$f ($(GOOS)/$(GOARCH))"; \
	  CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	    go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$$f/bootstrap ./cmd/$$f; \
	done

package: build
	@for f in $(FUNCTIONS); do \
	  (cd $(BUILD_DIR)/$$f && zip -q -X ../$$f.zip bootstrap); \
	  echo "packaged $(BUILD_DIR)/$$f.zip"; \
	done
	@$(MAKE) --no-print-directory prune-stale
	@$(MAKE) --no-print-directory verify-zip

# The three zips the migration Terraform deploys, for the end-to-end rehearsal
# (docs/E2E-REHEARSAL.md): cutrelease + mergeback (provided.al2) and
# slackapprove (provided.al2023). Same rules: bootstrap at the root,
# linux/amd64, CGO off. slackapprove is in FUNCTIONS for THIS build only, so
# prune-stale keeps the copy it just built.
package-e2e:
	@$(MAKE) --no-print-directory package FUNCTIONS="cutrelease mergeback slackapprove"
	@ls -l $(BUILD_DIR)/*.zip

# A packaging check that fails the build rather than failing at INIT in AWS.
# Remove any zip this build did not produce.
#
# ⚠️ DELETE, do not warn. A warning in the middle of a build log is exactly
# what nobody reads, and the failure mode is silent: the stale file is still
# there, still uploadable, still looking current. A missing file forces whoever
# needs it to run the script that builds it properly.
prune-stale:
	@for z in $(BUILD_DIR)/*.zip; do \
	  [ -e "$$z" ] || continue; \
	  n=$$(basename $$z .zip); \
	  keep=""; \
	  for f in $(FUNCTIONS); do [ "$$n" = "$$f" ] && keep=1; done; \
	  if [ -z "$$keep" ]; then \
	    for f in $(EXTERNALLY_BUILT); do \
	      if [ "$$n" = "$$f" ]; then \
	        echo "removing $$z — $$n is built by its own deploy script, and a stale"; \
	        echo "  copy here is indistinguishable from a current one"; \
	      fi; \
	    done; \
	    rm -f "$$z"; \
	  fi; \
	done

verify-zip:
	@for f in $(FUNCTIONS); do \
	  entry=$$(unzip -Z1 $(BUILD_DIR)/$$f.zip | head -1); \
	  if [ "$$entry" != "bootstrap" ]; then \
	    echo "FAIL: $$f.zip root entry is '$$entry', must be 'bootstrap'"; exit 1; fi; \
	  arch=$$(file -b $(BUILD_DIR)/$$f/bootstrap); \
	  case "$$arch" in *x86-64*|*amd64*) ;; \
	    *) echo "FAIL: $$f bootstrap is '$$arch', must be x86-64"; exit 1;; esac; \
	  echo "ok: $$f.zip (bootstrap at root, x86-64)"; \
	done

test:
	go test ./... -count=1

lint:
	go vet ./...

mocks:
	go run ./cmd/mockapis

local:
	RELEASEBOT_GITHUB_API_URL=http://localhost:9099 \
	SLACK_API_URL=http://localhost:9099 \
	GITHUB_TOKEN=local-dev-github-token \
	SLACK_TOKEN=local-dev-slack-token \
	SLACK_SIGNING_SECRET=local-dev-signing-secret \
	JIRA_WEBHOOK_SECRET=local-dev-jira-secret \
	DEFAULT_OWNER=msnbc \
	SLACK_CHANNEL=C0RELEASE \
	RARC_ENV=test \
	go run ./cmd/localdev

demo:
	./scripts/demo.sh

clean:
	rm -rf $(BUILD_DIR)

# Contract suite against the local harness, including the mutating cases.
contract-local:
	@bash -c 'source scripts/harness.sh && harness_up && \
	  go run ./cmd/contracttest -base http://localhost:8080 -env local \
	    -repo msnbc/news-app-shadow -mutating'

# Parse every workflow before pushing. A GitHub expression inside a YAML FLOW
# mapping -- `with: {name: x-${{ y }}}` -- makes the file unparseable, and
# GitHub reports only "workflow file issue" with no line number. Catching it
# here costs a second; catching it in CI costs a push and a guess.
lint-shell:
	@python3 scripts/lint-shell.py

lint-workflows:
	@python3 -c "import yaml,glob,sys; \
	bad=[(f,str(e)[:120]) for f in glob.glob('.github/workflows/*.yml') \
	     for e in [None] if (lambda: False)()] ; \
	ok=True; \
	[ (yaml.safe_load(open(f)), print('  ok   '+f)) for f in sorted(glob.glob('.github/workflows/*.yml')) ]" \
	|| { echo 'workflow YAML is invalid'; exit 1; }
