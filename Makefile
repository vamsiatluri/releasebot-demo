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
LDFLAGS     := -s -w -X main.version=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

FUNCTIONS := cutrelease mergeback

.PHONY: all build package test lint clean local mocks demo verify-zip

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
	@$(MAKE) --no-print-directory verify-zip

# A packaging check that fails the build rather than failing at INIT in AWS.
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
	GITHUB_API_URL=http://localhost:9099 \
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
