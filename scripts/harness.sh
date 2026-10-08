#!/usr/bin/env bash
# Start the local mock GitHub/Slack and both ReleaseBot handlers, and guarantee
# they die when the caller does.
#
# Built binaries, not `go run`. `go run` compiles to a temp binary and execs it
# as a CHILD; killing the `go run` PID leaves that child holding the port, and
# the next run then silently talks to a stale server with the old seed data.
# That cost a debugging cycle on 2026-10-07 -- the fix belongs here, once.
#
# Usage:  source scripts/harness.sh && harness_up    (harness_down on EXIT)
set -euo pipefail

# Forced, not defaulted. The harness exists to point at the mock, so a caller's
# value is never what we want -- and GITHUB_API_URL in particular is set by
# GitHub Actions itself, which is how this silently aimed at the real GitHub.
export RELEASEBOT_GITHUB_API_URL="http://localhost:9099"
export SLACK_API_URL="http://localhost:9099"
export GITHUB_TOKEN="${GITHUB_TOKEN:-local-dev-github-token}"
export SLACK_TOKEN="${SLACK_TOKEN:-local-dev-slack-token}"
export SLACK_SIGNING_SECRET="${SLACK_SIGNING_SECRET:-local-dev-signing-secret}"
export JIRA_WEBHOOK_SECRET="${JIRA_WEBHOOK_SECRET:-local-dev-jira-secret}"
export DEFAULT_OWNER="${DEFAULT_OWNER:-msnbc}"
export SLACK_CHANNEL="${SLACK_CHANNEL:-C0RELEASE}"
export RARC_ENV="${RARC_ENV:-test}"

HARNESS_PIDS=()

harness_down() {
  for pid in "${HARNESS_PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  # Belt and braces: anything still holding the ports is ours and is stale.
  pkill -f 'releasebot-harness-mockapis' 2>/dev/null || true
  pkill -f 'releasebot-harness-localdev' 2>/dev/null || true
}

harness_up() {
  trap harness_down EXIT INT TERM
  harness_down          # clear any orphan from a previous run before binding
  sleep 0.3

  mkdir -p build/harness
  go build -o build/harness/releasebot-harness-mockapis ./cmd/mockapis
  go build -o build/harness/releasebot-harness-localdev ./cmd/localdev

  ./build/harness/releasebot-harness-mockapis >/tmp/releasebot-mocks.log 2>&1 &
  HARNESS_PIDS+=($!)
  ./build/harness/releasebot-harness-localdev >/tmp/releasebot-app.log 2>&1 &
  HARNESS_PIDS+=($!)

  for _ in $(seq 1 60); do
    curl -sf http://localhost:8080/beta/health >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  echo "harness did not come up; see /tmp/releasebot-app.log" >&2
  cat /tmp/releasebot-app.log >&2
  return 1
}
