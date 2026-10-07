// Command contracttest runs the ReleaseBot behaviour suite against any target.
//
//	go run ./cmd/contracttest -base http://localhost:8080
//	go run ./cmd/contracttest -base https://abc.execute-api.us-east-1.amazonaws.com/test \
//	    -env prod-account-test-stage -junit out.xml -summary out.md -metrics out.json
//
// Exit codes are what CI reads:
//
//	0  everything passed
//	1  at least one CRITICAL check failed  -- block the deploy, do not cut over
//	2  only non-critical checks failed     -- visible, but does not block
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/contract"
)

func main() {
	var (
		base     = flag.String("base", "http://localhost:8080", "base URL including the stage path")
		env      = flag.String("env", "local", "label for reports and metrics")
		repo     = flag.String("repo", "msnbc/releasebot-shadow", "repository the mutating cases act on")
		mutating = flag.Bool("mutating", false, "run the cases that write to GitHub")
		junit    = flag.String("junit", "", "write JUnit XML here")
		summary  = flag.String("summary", "", "write a markdown summary here (use $GITHUB_STEP_SUMMARY)")
		metrics  = flag.String("metrics", "", "write a CloudWatch put-metric-data payload here")
		timeout  = flag.Duration("timeout", 2*time.Minute, "overall timeout")
	)
	flag.Parse()

	signing := os.Getenv("SLACK_SIGNING_SECRET")
	if signing == "" {
		fatal("SLACK_SIGNING_SECRET is required: the suite signs its own requests so it " +
			"exercises the real verification path rather than bypassing it")
	}

	// Guard: the mutating cases create branches and pull requests on a REAL
	// repository. During a migration's parallel-run window there are two live
	// bots with write credentials, so a suite pointed at the wrong repo is not
	// a failed test -- it is a corrupted release. The name must look like a
	// shadow repo, and -mutating must be explicit.
	if *mutating && !isShadow(*repo) {
		fatal(fmt.Sprintf("refusing to run mutating cases against %q: the repository name must "+
			"contain 'shadow', 'sandbox', 'test' or 'scratch'", *repo))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	r := contract.NewRunner(*base, signing, os.Getenv("JIRA_WEBHOOK_SECRET"))
	r.AllowMutating = *mutating

	started := time.Now()
	cases := contract.Build(*repo, contract.RunToken(started))
	results := r.Run(ctx, cases)
	s := contract.Summarise(*base, *env, started, results)

	s.WriteText(os.Stdout)

	if *junit != "" {
		write(*junit, func(f *os.File) error { return s.WriteJUnit(f) })
	}
	if *summary != "" {
		appendTo(*summary, func(f *os.File) error { s.WriteMarkdown(f); return nil })
	}
	if *metrics != "" {
		write(*metrics, func(f *os.File) error { return s.WriteMetrics(f) })
	}

	switch {
	case s.CriticalFails > 0:
		os.Exit(1)
	case s.Failed > 0:
		os.Exit(2)
	}
}

func isShadow(repo string) bool {
	r := strings.ToLower(repo)
	for _, ok := range []string{"shadow", "sandbox", "test", "scratch"} {
		if strings.Contains(r, ok) {
			return true
		}
	}
	return false
}

func write(path string, fn func(*os.File) error) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	if err := fn(f); err != nil {
		fatal(err.Error())
	}
}

// appendTo is used for $GITHUB_STEP_SUMMARY, which accumulates across steps.
func appendTo(path string, fn func(*os.File) error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal(err.Error())
	}
	defer f.Close()
	if err := fn(f); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "contracttest: "+msg)
	os.Exit(3)
}
