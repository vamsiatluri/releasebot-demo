// Command dashboardapi answers one question for the management dashboard:
// what is each environment running right now, and do they agree?
//
// It reads AWS directly rather than trusting anything the pipeline wrote down,
// for the same reason the baseline tool reads a running account rather than its
// templates: a dashboard that renders what a deploy step *claimed* will keep
// saying so long after it stops being true.
//
// ⚠️ This endpoint sits behind a Cognito JWT authorizer on the HTTP API. It
// does no authentication of its own, and must not be reachable any other way.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vamsiatluri/releasebot-migration/internal/awssig"
	"github.com/vamsiatluri/releasebot-migration/internal/events"
	"github.com/vamsiatluri/releasebot-migration/internal/runtime"
)

// A pair is one logical function: the production copy and its test twin.
type pair struct {
	Name string  `json:"name"`
	Prod fnState `json:"prod"`
	Test fnState `json:"test"`
	Same bool    `json:"same"`
}

type fnState struct {
	Function string `json:"function"`
	Version  string `json:"version"`
	SHA      string `json:"sha"`
	// Network posture, read from the same call that reads the fingerprint --
	// "is this function actually on a private subnet" is a question a reviewer
	// asks and a template cannot answer, because a template describes intent.
	Vpc     string `json:"vpc,omitempty"`
	Subnets int    `json:"subnets,omitempty"`
	Error   string `json:"error,omitempty"`
}

type health struct {
	Stage  string `json:"stage"`
	OK     bool   `json:"ok"`
	Env    string `json:"env"`
	Commit string `json:"commit"`
	Built  string `json:"built"`
	// ⚠️ WHERE the running function got its configuration and its SECRETS,
	// reported as a mechanism and never as a value. Without these two a
	// hardened function is indistinguishable from one holding plaintext
	// secrets in its environment, short of reading the secrets -- which is
	// precisely how a half-finished rollout reads as finished.
	ConfigSource string `json:"configSource,omitempty"` // ssm | env
	SecretSource string `json:"secretSource,omitempty"` // reference | plaintext | mixed | unset
	// ⚠️ "unrestricted" or "N permitted". Surfaced because an EMPTY cut
	// allowlist admits the whole workspace -- the deliberate default, and
	// exactly the kind of optional control that is never switched on if nothing
	// ever says it is off.
	CutAllowlist string `json:"cutAllowlist,omitempty"`
	// ⛔ WHICH DEPLOYMENT this is. Two environments produce two dashboards that
	// look identical, and a reader who cannot tell them apart is the same
	// hazard as two identical approve buttons in one Slack channel.
	Account string `json:"account,omitempty"`
	Error   string `json:"error,omitempty"`
}

// A step inside a job. This is the detail GitHub's own run page shows only
// after you click into a job -- surfacing it on one screen is the point of the
// dashboard: somebody watching a release should not have to navigate to learn
// which of eleven steps is the slow one.
type stepState struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Seconds    int    `json:"seconds"` // elapsed; 0 while queued
}

// A job as GitHub reports it, plus where it sits in the release story.
type jobState struct {
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	Seconds    int         `json:"seconds"`
	URL        string      `json:"url"`
	Steps      []stepState `json:"steps"`
}

type runState struct {
	Number     int        `json:"number"`
	ID         int64      `json:"id"`
	Commit     string     `json:"commit"`
	Title      string     `json:"title"`
	Actor      string     `json:"actor"`
	Status     string     `json:"status"`
	Conclusion string     `json:"conclusion"`
	URL        string     `json:"url"`
	Jobs       []jobState `json:"jobs"`
	Change     *changeset `json:"change,omitempty"`
	ReadAt     string     `json:"readAt"`
	Stale      bool       `json:"stale,omitempty"`
	// ⚠️ WHY it is stale. Without this the UI said only "GitHub unreachable",
	// because the stale path copied the last-good value and DROPPED the fresh
	// error -- so the one piece of information that distinguishes "we are rate
	// limited, it will recover" from "something is broken" was discarded at
	// exactly the moment it was needed.
	StaleReason string `json:"staleReason,omitempty"`
	Error       string `json:"error,omitempty"`
}

// One published version, and whether it is actually reachable as a rollback
// target. "Published" and "usable" are different facts: a version published
// before a secret rotation still carries the old secret and dies during INIT.
type versionState struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Published string `json:"published"`
	Live      bool   `json:"live"`
	Stale     bool   `json:"stale"` // config no longer matches current
}

type rollbackTargets struct {
	Function string         `json:"function"`
	Env      string         `json:"env"`
	Live     string         `json:"live"`
	Versions []versionState `json:"versions"`
	Error    string         `json:"error,omitempty"`
}

// One thing ReleaseBot did, read back from the ledger it wrote.
type release struct {
	At       string   `json:"at"`
	Env      string   `json:"env"`
	Action   string   `json:"action"`
	Repo     string   `json:"repo"`
	Property string   `json:"property"`
	Version  string   `json:"version"`
	Branch   string   `json:"branch"`
	Actor    string   `json:"actor"`
	Source   string   `json:"source"`
	Issues   []string `json:"issues"`
	Commits  int      `json:"commits"`
	URL      string   `json:"url"`
	DryRun   bool     `json:"dryRun"`
	// Which build of ReleaseBot performed this. ⚠️ NOT an Actions run number --
	// the ledger records what the BOT did (Slack/Jira-triggered), and run
	// numbers belong to the pipeline that DEPLOYS the bot. The commit is the
	// honest link between the two: from it you can find the run.
	BotVersion string `json:"botVersion,omitempty"`
	BotCommit  string `json:"botCommit,omitempty"`
}

// What is in THIS pipeline run, relative to the last one that reached
// production. Not the same question as the ledger's -- that one is about the
// releases ReleaseBot cuts for other people; this one is about ReleaseBot's
// own change.
type changeset struct {
	Base     string   `json:"base"`
	Head     string   `json:"head"`
	Commits  int      `json:"commits"`
	Messages []string `json:"messages"`
	Issues   []string `json:"issues"`
	Error    string   `json:"error,omitempty"`
}

// platform is everything about the system that is NOT a release: the controls
// around it. A dashboard that only shows versions answers "what is running" and
// leaves "is it protected" to a conversation.
type platform struct {
	DlqDepth   int      `json:"dlqDepth"`
	DlqError   string   `json:"dlqError,omitempty"`
	Alarms     []alarm  `json:"alarms,omitempty"`
	AlarmError string   `json:"alarmError,omitempty"`
	WafProd    string   `json:"wafProd,omitempty"`
	WafTest    string   `json:"wafTest,omitempty"`
	ProdRate   float64  `json:"prodRate,omitempty"`
	TestRate   float64  `json:"testRate,omitempty"`
	WafError   string   `json:"wafError,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

type alarm struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type payload struct {
	Account string `json:"account"`
	Region  string `json:"region"`
	ReadAt  string `json:"readAt"`
	// The account LABEL as the RUNNING functions report it -- not as the
	// dashboard was configured. ⚠️ Deliberately distinct from Account above,
	// which is the 12-digit AWS id: if the two disagree, the dashboard is
	// pointed somewhere other than where it thinks it is.
	AccountLabel string            `json:"accountLabel,omitempty"`
	Pairs        []pair            `json:"pairs"`
	Health       []health          `json:"health"`
	InSync       bool              `json:"inSync"`
	Run          *runState         `json:"run,omitempty"`
	Rollback     []rollbackTargets `json:"rollback,omitempty"`
	Releases     []release         `json:"releases,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
	Notes        []string          `json:"notes"`
}

func main() {
	runtime.Start(func(ctx context.Context, inv runtime.Invocation, p []byte) ([]byte, error) {
		var req events.APIGatewayProxyRequest
		if err := json.Unmarshal(p, &req); err != nil {
			return nil, fmt.Errorf("not an API Gateway event: %w", err)
		}
		body, _ := json.Marshal(build(ctx))
		return json.Marshal(events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "application/json",
				// The page is served from CloudFront and this API lives on a
				// different origin, so the browser requires this. The allowed
				// origin is configured on the HTTP API itself; echoing "*" here
				// as well would widen it.
				"Cache-Control": "no-store",
			},
			Body: string(body),
		})
	})
}

func build(ctx context.Context) payload {
	region := os.Getenv("AWS_REGION")
	out := payload{
		Account: os.Getenv("RELEASEBOT_ACCOUNT"),
		Region:  region,
		ReadAt:  time.Now().UTC().Format(time.RFC3339),
	}

	names := [][2]string{
		{"cutRelease", "cutReleaseTest"},
		{"releaseAutomationMergeback", "releaseAutomationMergebackTest"},
	}

	var wg sync.WaitGroup
	out.Pairs = make([]pair, len(names))
	for i, n := range names {
		wg.Add(1)
		go func(i int, prodName, testName string) {
			defer wg.Done()
			pr := readFn(ctx, prodName)
			te := readFn(ctx, testName)
			out.Pairs[i] = pair{
				Name: prodName,
				Prod: pr, Test: te,
				// Equal fingerprints mean the same bytes are deployed on both
				// sides. Unequal is NOT a fault -- it is the normal state while
				// a release is held at the gate -- so the UI must say which.
				Same: pr.SHA != "" && pr.SHA == te.SHA,
			}
		}(i, n[0], n[1])
	}

	base := os.Getenv("RELEASEBOT_HEALTH_BASE")
	stages := strings.Split(os.Getenv("RELEASEBOT_STAGES"), ",")
	out.Health = make([]health, 0, len(stages))
	var mu sync.Mutex
	for _, s := range stages {
		s = strings.TrimSpace(s)
		if s == "" || base == "" {
			continue
		}
		wg.Add(1)
		go func(stage string) {
			defer wg.Done()
			h := readHealth(ctx, base, stage)
			mu.Lock()
			out.Health = append(out.Health, h)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	// The pipeline's own state. Read live from GitHub rather than from anything
	// a deploy step wrote down -- a dashboard that renders what a step CLAIMED
	// keeps saying it long after it stops being true.
	if r := cachedRun(ctx); r != nil {
		out.Run = r
	}

	// Where could we go back to, and which of those targets would actually
	// start? Read live, because the answer changes every time a secret rotates
	// and nothing emits an event when it does.
	for _, fn := range []struct{ name, env string }{
		{"cutRelease", "prod"}, {"releaseAutomationMergeback", "prod"},
		{"cutReleaseTest", "test"}, {"releaseAutomationMergebackTest", "test"},
	} {
		out.Rollback = append(out.Rollback, readRollback(ctx, fn.name, fn.env))
	}

	// What ReleaseBot has shipped, for whom, closing what.
	out.Releases = readLedger(ctx)

	// The controls around the release, not the release itself.
	out.Platform = readPlatform(ctx)
	for _, h := range out.Health {
		if h.Account != "" {
			out.AccountLabel = h.Account
			break
		}
	}

	out.InSync = true
	for _, p := range out.Pairs {
		if !p.Same {
			out.InSync = false
		}
	}
	if !out.InSync {
		out.Notes = append(out.Notes,
			"Test is ahead of production. That is the approval gate holding a release, not drift.")
	}
	return out
}

// readFn asks the Lambda control plane what the `live` alias actually points at.
//
// It signs the call itself (see internal/awssig) rather than shelling out to
// the AWS CLI -- the CLI is NOT present in a provided.al2 deployment package,
// so exec'ing it would compile cleanly, deploy cleanly, and fail only at the
// first real request.
func readFn(ctx context.Context, name string) fnState {
	st := fnState{Function: name}

	creds, err := awssig.FromEnvironment()
	if err != nil {
		st.Error = "no credentials"
		return st
	}
	region := os.Getenv("AWS_REGION")
	endpoint := "https://lambda." + region + ".amazonaws.com" +
		"/2015-03-31/functions/" + url.PathEscape(name) + "/configuration?Qualifier=live"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		st.Error = "bad request"
		return st
	}
	awssig.SignGET(req, creds, "lambda", region, time.Now())

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		st.Error = "unreachable"
		return st
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		// Never echo the body: an AWS error can carry the ARN and the role.
		st.Error = fmt.Sprintf("lambda returned %d", resp.StatusCode)
		return st
	}
	var cfg struct {
		Version    string `json:"Version"`
		CodeSha256 string `json:"CodeSha256"`
		VpcConfig  struct {
			VpcId     string   `json:"VpcId"`
			SubnetIds []string `json:"SubnetIds"`
		} `json:"VpcConfig"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		st.Error = "unreadable configuration"
		return st
	}
	st.Version, st.SHA = cfg.Version, cfg.CodeSha256
	st.Vpc, st.Subnets = cfg.VpcConfig.VpcId, len(cfg.VpcConfig.SubnetIds)
	return st
}

func readHealth(ctx context.Context, base, stage string) health {
	h := health{Stage: stage}
	url := strings.TrimSuffix(base, "/") + "/" + stage + "/beta/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		h.Error = "bad health URL"
		return h
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		h.Error = "unreachable"
		return h
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var got struct {
		OK           bool   `json:"ok"`
		Env          string `json:"env"`
		Commit       string `json:"commit"`
		Built        string `json:"built"`
		ConfigSource string `json:"configSource"`
		SecretSource string `json:"secretSource"`
		CutAllowlist string `json:"cutAllowlist"`
		Account      string `json:"account"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		h.Error = fmt.Sprintf("HTTP %d, unparseable", resp.StatusCode)
		return h
	}
	h.OK, h.Env, h.Commit, h.Built = got.OK, got.Env, got.Commit, got.Built
	h.ConfigSource, h.SecretSource = got.ConfigSource, got.SecretSource
	h.CutAllowlist = got.CutAllowlist
	h.Account = got.Account
	return h
}

// ── The pipeline panel, and why it is cached ────────────────────────────────
//
// Every browser tab polls this endpoint. Without a cache, N tabs at a 15s
// refresh is 8N GitHub calls a minute, and GitHub's limit is per CREDENTIAL,
// not per viewer -- so the dashboard would be the thing that exhausts the quota
// the release pipeline itself depends on. One cached read serves every tab.
//
// ⚠️ The repository is public, so this works with NO token at all -- but
// unauthenticated reads are capped at 60/hour PER EGRESS IP, and a Lambda's
// egress IP is shared. So the TTL is deliberately different for the two cases:
// a token buys 5,000/hour and a live-feeling refresh; without one the panel
// stays correct but refreshes slowly, and says so.
var runCache struct {
	mu  sync.Mutex
	at  time.Time
	val *runState
}

// ── Conditional requests ────────────────────────────────────────────────────
//
// WHY THIS EXISTS: so the dashboard needs NO GitHub credential at all.
//
// The repository is public, so every read here works unauthenticated -- the
// only thing a token ever bought was headroom, because anonymous reads are
// capped at 60/hour per egress IP. The dashboard was therefore borrowing the
// APPROVAL token, a classic PAT with `repo` scope: the power to push to every
// repository the owner can reach, held by a page that only ever reads.
//
// ⚠️ A request that returns 304 Not Modified DOES NOT COUNT against GitHub's
// rate limit. So caching the ETag and sending If-None-Match makes an idle
// dashboard cost nothing at all, and a busy one cost only its genuine changes.
// The over-grant is not reduced, it is removed: there is no credential left to
// scope wrongly.
var etags struct {
	mu sync.Mutex
	m  map[string]struct{ etag, body string }
}

func cachedETag(path string) (etag, body string) {
	etags.mu.Lock()
	defer etags.mu.Unlock()
	e := etags.m[path]
	return e.etag, e.body
}

func storeETag(path, etag, body string) {
	if etag == "" {
		return
	}
	etags.mu.Lock()
	defer etags.mu.Unlock()
	if etags.m == nil {
		etags.m = map[string]struct{ etag, body string }{}
	}
	etags.m[path] = struct{ etag, body string }{etag, body}
}

// runTTL is ADAPTIVE, and the reason is the whole point of the conditional
// requests above: a 304 Not Modified does not count against GitHub's rate
// limit, so only a CHANGED response costs anything.
//
// ⚠️ A flat 120s was the wrong trade in exactly the moment that matters. While
// a release is held at the approval gate, nothing about the run changes -- so
// every read is a 304 and FREE -- and that is precisely when somebody is
// watching the screen waiting for their approval to register. A uniform TTL
// made the panel look stuck ("it is not moving after I approved") at the one
// time it needed to look live, while spending its budget on runs nobody was
// looking at.
//
// So: refresh quickly while the run is in a non-terminal state, slowly once it
// has finished. An active run lasts two or three minutes, so a 15s TTL costs
// at most a dozen changed reads per release -- well inside 60/hour -- and a
// held gate costs nothing at all.
func runTTL() time.Duration {
	if os.Getenv("GITHUB_TOKEN") != "" {
		return 10 * time.Second
	}
	if runCache.val != nil && !terminal(runCache.val.Status) {
		return 15 * time.Second
	}
	return 120 * time.Second
}

// terminal reports whether a run has reached a state that cannot change again.
// ⚠️ "waiting" is NOT terminal -- it is a run held at the approval gate, which
// is the state most worth refreshing. Treating it as finished is the bug this
// function exists to prevent.
func terminal(status string) bool {
	return status == "completed"
}

// cachedRun returns the pipeline panel, refreshing it at most once per TTL.
//
// On a refresh failure it serves the LAST GOOD value marked stale rather than
// an error. A dashboard that blanks the panel the moment GitHub rate-limits it
// has turned a slow refresh into an apparent outage.
func cachedRun(ctx context.Context) *runState {
	runCache.mu.Lock()
	defer runCache.mu.Unlock()

	if runCache.val != nil && time.Since(runCache.at) < runTTL() {
		return runCache.val
	}
	fresh := readRun(ctx)
	if fresh == nil {
		return runCache.val
	}
	if fresh.Error != "" && runCache.val != nil && runCache.val.Error == "" {
		stale := *runCache.val
		stale.Stale, stale.StaleReason = true, fresh.Error
		return &stale
	}
	fresh.ReadAt = time.Now().UTC().Format(time.RFC3339)
	runCache.at, runCache.val = time.Now(), fresh
	return fresh
}

// readRun fetches the most recent run of the release workflow and its jobs.
//
// ⚠️ A failure here must not fail the whole page. The environment state above
// is read from AWS and is still true; losing the pipeline panel is a degraded
// view, not an outage, and the UI says which part is missing.
func readRun(ctx context.Context) *runState {
	owner, repo := os.Getenv("GITHUB_OWNER"), os.Getenv("GITHUB_REPO")
	token, wf := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_WORKFLOW_FILE")
	if owner == "" || repo == "" || wf == "" {
		return nil
	}

	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"https://api.github.com/repos/"+owner+"/"+repo+path, nil)
		if err != nil {
			return err
		}
		// Omitted entirely when empty -- an `Authorization: Bearer ` header with
		// no value is a 401, which would read as a bad token rather than as the
		// anonymous read it is meant to be.
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if et, _ := cachedETag(path); et != "" {
			req.Header.Set("If-None-Match", et)
		}
		resp, err := (&http.Client{Timeout: 6 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

		// 304: nothing changed, the body is empty, and this request was FREE.
		if resp.StatusCode == http.StatusNotModified {
			if _, cached := cachedETag(path); cached != "" {
				return json.Unmarshal([]byte(cached), out)
			}
			return fmt.Errorf("github sent 304 with nothing cached")
		}
		if resp.StatusCode == 200 {
			storeETag(path, resp.Header.Get("ETag"), string(raw))
		}
		if resp.StatusCode != 200 {
			// Never echo the body: a GitHub error names the repo and the token's
			// owner. Rate limiting is called out by name because "403" on its own
			// sends people looking for a permissions problem that is not there.
			if (resp.StatusCode == 403 || resp.StatusCode == 429) &&
				resp.Header.Get("X-RateLimit-Remaining") == "0" {
				// ⚠️ Anonymous reads are 60/hour PER EGRESS IP, and a Lambda's
				// egress is a shared NAT address -- so this is reachable during
				// an ordinary busy afternoon, not only under abuse. Naming the
				// reset turns "it is broken" into "it is back in 12 minutes".
				mins := ""
				if ts, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
					if d := time.Until(time.Unix(ts, 0)); d > 0 {
						mins = fmt.Sprintf(", resets in %dm", int(d.Minutes())+1)
					}
				}
				return fmt.Errorf("github rate limit reached (60/hour unauthenticated, shared egress IP)%s", mins)
			}
			return fmt.Errorf("github returned %d", resp.StatusCode)
		}
		return json.Unmarshal(raw, out)
	}

	var runs struct {
		WorkflowRuns []struct {
			ID           int64  `json:"id"`
			RunNumber    int    `json:"run_number"`
			HeadSHA      string `json:"head_sha"`
			DisplayTitle string `json:"display_title"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			HTMLURL      string `json:"html_url"`
			Actor        struct {
				Login string `json:"login"`
			} `json:"actor"`
		} `json:"workflow_runs"`
	}
	if err := get("/actions/workflows/"+wf+"/runs?per_page=1", &runs); err != nil {
		return &runState{Error: err.Error()}
	}
	if len(runs.WorkflowRuns) == 0 {
		return nil
	}
	r := runs.WorkflowRuns[0]
	st := &runState{
		Number: r.RunNumber, ID: r.ID, Commit: shortSHA(r.HeadSHA),
		Title: r.DisplayTitle, Actor: r.Actor.Login,
		Status: r.Status, Conclusion: r.Conclusion, URL: r.HTMLURL,
	}

	var jobs struct {
		Jobs []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			HTMLURL     string `json:"html_url"`
			StartedAt   string `json:"started_at"`
			CompletedAt string `json:"completed_at"`
			Steps       []struct {
				Name        string `json:"name"`
				Number      int    `json:"number"`
				Status      string `json:"status"`
				Conclusion  string `json:"conclusion"`
				StartedAt   string `json:"started_at"`
				CompletedAt string `json:"completed_at"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	st.Change = readChange(ctx, get, r.HeadSHA)

	if err := get(fmt.Sprintf("/actions/runs/%d/jobs", r.ID), &jobs); err == nil {
		for _, j := range jobs.Jobs {
			js := jobState{
				Name: j.Name, Status: j.Status, Conclusion: j.Conclusion,
				URL: j.HTMLURL, Seconds: elapsed(j.StartedAt, j.CompletedAt),
			}
			for _, sp := range j.Steps {
				js.Steps = append(js.Steps, stepState{
					Name: sp.Name, Number: sp.Number,
					Status: sp.Status, Conclusion: sp.Conclusion,
					Seconds: elapsed(sp.StartedAt, sp.CompletedAt),
				})
			}
			st.Jobs = append(st.Jobs, js)
		}
	}
	return st
}

// elapsed returns how long something took, or how long it has been running.
//
// ⚠️ A step that has not started has a null started_at, which unmarshals to "".
// Treating that as the zero time would report 56 years of elapsed time, which
// is the kind of absurd number a dashboard renders without blinking.
func elapsed(start, end string) int {
	if start == "" {
		return 0
	}
	t0, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return 0
	}
	t1 := time.Now().UTC()
	if end != "" {
		if p, err := time.Parse(time.RFC3339, end); err == nil {
			t1 = p
		}
	}
	d := int(t1.Sub(t0).Seconds())
	if d < 0 {
		return 0
	}
	return d
}

// readRollback lists a function's recent versions and marks the ones that
// could not start if the alias were moved to them.
//
// ⚠️ "Stale" here is decided by comparing the SECRET-BEARING environment keys
// on the published version against the function's current configuration. The
// values are never read into the response -- only whether they differ. A
// dashboard that renders a secret to prove it changed has leaked it.
func readRollback(ctx context.Context, name, env string) rollbackTargets {
	rt := rollbackTargets{Function: name, Env: env}

	creds, err := awssig.FromEnvironment()
	if err != nil {
		rt.Error = "no credentials"
		return rt
	}
	region := os.Getenv("AWS_REGION")
	host := "https://lambda." + region + ".amazonaws.com"

	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+path, nil)
		if err != nil {
			return err
		}
		awssig.SignGET(req, creds, "lambda", region, time.Now())
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != 200 {
			return fmt.Errorf("lambda returned %d", resp.StatusCode)
		}
		return json.Unmarshal(raw, out)
	}

	var alias struct {
		FunctionVersion string `json:"FunctionVersion"`
	}
	if err := get("/2015-03-31/functions/"+url.PathEscape(name)+"/aliases/live", &alias); err != nil {
		rt.Error = err.Error()
		return rt
	}
	rt.Live = alias.FunctionVersion

	type cfg struct {
		Version      string                                `json:"Version"`
		Description  string                                `json:"Description"`
		LastModified string                                `json:"LastModified"`
		Environment  struct{ Variables map[string]string } `json:"Environment"`
	}
	var list struct {
		Versions []cfg `json:"Versions"`
	}
	if err := get("/2015-03-31/functions/"+url.PathEscape(name)+"/versions?MaxItems=50", &list); err != nil {
		rt.Error = err.Error()
		return rt
	}

	// $LATEST carries the configuration a rollback target is compared against:
	// it is whatever the template most recently applied, i.e. "current".
	var current map[string]string
	for _, v := range list.Versions {
		if v.Version == "$LATEST" {
			current = v.Environment.Variables
		}
	}

	var pub []cfg
	for _, v := range list.Versions {
		if v.Version != "$LATEST" {
			pub = append(pub, v)
		}
	}
	if len(pub) > 8 {
		pub = pub[len(pub)-8:]
	}
	for _, v := range pub {
		rt.Versions = append(rt.Versions, versionState{
			Version:   v.Version,
			Commit:    commitOf(v.Description),
			Published: v.LastModified,
			Live:      v.Version == rt.Live,
			Stale:     secretsDiffer(current, v.Environment.Variables),
		})
	}
	return rt
}

// secretKeys are the values whose rotation strands an older version.
var secretKeys = []string{
	"GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_SIGNING_SECRET", "JIRA_WEBHOOK_SECRET",
}

func secretsDiffer(current, frozen map[string]string) bool {
	if current == nil || frozen == nil {
		return false // unknown is not the same as stale; do not cry wolf
	}
	for _, k := range secretKeys {
		if current[k] != frozen[k] {
			return true
		}
	}
	return false
}

// commitOf reads the commit out of a version description.
//
// Only a description this pipeline wrote names a commit. Anything else came
// from a hand-run or from Lambda, and inventing a commit out of it would print
// a false record -- an em dash is the honest rendering of "not known".
func commitOf(desc string) string {
	const p = "releasebot "
	if strings.HasPrefix(desc, p) {
		return strings.TrimSpace(desc[len(p):])
	}
	return "\u2014"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// ── The release ledger, read back ───────────────────────────────────────────

// readLedger returns the most recent releases across both environments.
//
// ⚠️ Query, never Scan. A Scan reads the whole table to return ten rows, and
// it gets slower every week the product succeeds -- the classic dashboard that
// is fine at launch and times out at the end of the year.
func readLedger(ctx context.Context) []release {
	region := os.Getenv("AWS_REGION")
	creds, err := awssig.FromEnvironment()
	if err != nil || region == "" {
		return nil
	}

	var all []release
	for _, env := range []string{"prod", "test"} {
		body, _ := json.Marshal(map[string]any{
			"TableName":              "releasebot-ledger",
			"KeyConditionExpression": "pk = :p",
			"ExpressionAttributeValues": map[string]any{
				":p": map[string]any{"S": "ENV#" + env},
			},
			// Newest first, and a hard cap: this panel is "what shipped
			// lately", not an archive browser.
			"ScanIndexForward": false,
			"Limit":            12,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://dynamodb."+region+".amazonaws.com/", bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "DynamoDB_20120810.Query")
		awssig.Sign(req, body, creds, "dynamodb", region, time.Now())

		resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		var out struct {
			Items []map[string]struct {
				S    string   `json:"S"`
				N    string   `json:"N"`
				BOOL bool     `json:"BOOL"`
				SS   []string `json:"SS"`
			} `json:"Items"`
		}
		if json.Unmarshal(raw, &out) != nil {
			continue
		}
		for _, it := range out.Items {
			r := release{
				At: it["at"].S, Env: env, Action: it["action"].S,
				Repo: it["repo"].S, Property: it["property"].S,
				Version: it["version"].S, Branch: it["branch"].S,
				Actor: it["actor"].S, Source: it["source"].S,
				Issues: it["issues"].SS, URL: it["url"].S,
				DryRun:     it["dryRun"].BOOL,
				BotVersion: it["botVersion"].S,
				BotCommit:  it["botCommit"].S,
			}
			fmt.Sscanf(it["commits"].N, "%d", &r.Commits)
			all = append(all, r)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At > all[j].At })
	if len(all) > 14 {
		all = all[:14]
	}
	return all
}

// issueKey matches a Jira key. Deliberately strict: a loose pattern turns
// "UTF-8" and "COVID-19" into ticket references and the release notes stop
// being worth reading.
var issueKey = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[0-9]{1,6}\b`)

// readChange asks GitHub what is in this run that was not in the last one to
// reach production.
//
// ⚠️ The base is the last run that actually PROMOTED, not the last run that
// went green. A run can succeed without reaching production -- it can be
// cancelled at the gate -- and using it as the base would report an empty
// changeset for a release that contains real work.
func readChange(ctx context.Context, get func(string, any) error, head string) *changeset {
	var runs struct {
		WorkflowRuns []struct {
			HeadSHA string `json:"head_sha"`
			Status  string `json:"status"`
		} `json:"workflow_runs"`
	}
	wf := os.Getenv("GITHUB_WORKFLOW_FILE")
	if err := get("/actions/workflows/"+wf+"/runs?status=success&per_page=10", &runs); err != nil {
		return &changeset{Head: head, Error: err.Error()}
	}
	base := ""
	for _, r := range runs.WorkflowRuns {
		if r.HeadSHA != "" && !strings.HasPrefix(r.HeadSHA, head) && head != "" &&
			!strings.HasPrefix(head, r.HeadSHA[:7]) {
			base = r.HeadSHA
			break
		}
	}
	if base == "" {
		return nil
	}

	var cmp struct {
		TotalCommits int `json:"total_commits"`
		Commits      []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := get("/compare/"+base+"..."+head, &cmp); err != nil {
		return &changeset{Base: shortSHA(base), Head: head, Error: err.Error()}
	}
	ch := &changeset{Base: shortSHA(base), Head: head, Commits: cmp.TotalCommits}
	seen := map[string]bool{}
	for _, c := range cmp.Commits {
		m := c.Commit.Message
		if i := strings.IndexByte(m, '\n'); i >= 0 {
			m = m[:i]
		}
		ch.Messages = append(ch.Messages, m)
		for _, k := range issueKey.FindAllString(m, -1) {
			if !seen[k] {
				seen[k] = true
				ch.Issues = append(ch.Issues, k)
			}
		}
	}
	sort.Strings(ch.Issues)
	return ch
}

// ── The controls, read live ─────────────────────────────────────────────────

// awsJSON signs and sends an AWS-JSON request. Three different AWS services
// here speak three different dialects, and the dialect is not cosmetic: the
// wrong Content-Type is rejected with an UnknownOperationException that names
// neither the operation nor the reason.
func awsJSON(ctx context.Context, service, host, target, contentType string, body []byte, out any) error {
	region := os.Getenv("AWS_REGION")
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	awssig.Sign(req, body, creds, service, region, time.Now())
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		// Type, never body: an AWS error body names the ARN and the caller.
		kind := resp.Header.Get("X-Amzn-Errortype")
		if i := strings.IndexByte(kind, ':'); i > 0 {
			kind = kind[:i]
		}
		return fmt.Errorf("%s returned %d (%s)", service, resp.StatusCode, kind)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// awsGET signs an ordinary REST GET. Several AWS services are plain REST and
// need none of the JSON-protocol ceremony.
func awsGET(ctx context.Context, service, host, path string, out any) error {
	region := os.Getenv("AWS_REGION")
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	awssig.SignGET(req, creds, service, region, time.Now())
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s returned %d", service, resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func readPlatform(ctx context.Context) *platform {
	region := os.Getenv("AWS_REGION")
	acct := os.Getenv("RELEASEBOT_ACCOUNT")
	p := &platform{}

	// ── 1. Is anything sitting in the dead-letter queue? ────────────────────
	// One message means a slash command was acknowledged in Slack and the work
	// never happened. It is the single most important number on this page.
	var q struct {
		Attributes map[string]string `json:"Attributes"`
	}
	body, _ := json.Marshal(map[string]any{
		"QueueUrl":       "https://sqs." + region + ".amazonaws.com/" + acct + "/releasebot-async-failures",
		"AttributeNames": []string{"ApproximateNumberOfMessages"},
	})
	if err := awsJSON(ctx, "sqs", "sqs."+region+".amazonaws.com",
		"AmazonSQS.GetQueueAttributes", "application/x-amz-json-1.0", body, &q); err != nil {
		p.DlqError = err.Error()
	} else {
		fmt.Sscanf(q.Attributes["ApproximateNumberOfMessages"], "%d", &p.DlqDepth)
	}

	// ── 2. Alarm state ──────────────────────────────────────────────────────
	// ⚠️ CloudWatch speaks the old QUERY protocol -- form-encoded, no
	// X-Amz-Target, and an XML response. It is the one AWS service here that
	// does not accept JSON, and sending it JSON fails in a way that reads like
	// a credentials problem.
	form := []byte("Action=DescribeAlarms&Version=2010-08-01&AlarmNamePrefix=releasebot&MaxRecords=10")
	if err := awsJSONRaw(ctx, "monitoring", "monitoring."+region+".amazonaws.com", form, p); err != nil {
		p.AlarmError = err.Error()
	}

	// ── 3. Is the edge protected? ───────────────────────────────────────────
	// ⚠️ Read from API GATEWAY, not from wafv2. The obvious call is
	// wafv2:GetWebACLForResource, and it is the one the AWS CLI makes -- but
	// hand-rolled it kept returning 400, and the stage object already carries
	// `webAclArn` AND the throttle settings in a single ordinary signed GET.
	// One call, one dialect, two answers.
	api := apiIDFromBase(os.Getenv("RELEASEBOT_HEALTH_BASE"))
	if api != "" {
		for _, st := range []struct {
			stage string
			dst   *string
			rate  *float64
		}{
			{"prod", &p.WafProd, &p.ProdRate},
			{"test", &p.WafTest, &p.TestRate},
		} {
			var stage struct {
				WebAclArn      string `json:"webAclArn"`
				MethodSettings map[string]struct {
					ThrottlingRateLimit  float64 `json:"throttlingRateLimit"`
					ThrottlingBurstLimit int     `json:"throttlingBurstLimit"`
				} `json:"methodSettings"`
			}
			if err := awsGET(ctx, "apigateway", "apigateway."+region+".amazonaws.com",
				"/restapis/"+api+"/stages/"+st.stage, &stage); err != nil {
				p.WafError = err.Error()
				continue
			}
			if i := strings.LastIndex(stage.WebAclArn, "/"); i >= 0 {
				*st.dst = stage.WebAclArn[strings.LastIndex(stage.WebAclArn[:i], "/")+1 : i]
			}
			for _, ms := range stage.MethodSettings {
				*st.rate = ms.ThrottlingRateLimit
			}
		}
	}

	// Test being unprotected is DELIBERATE and the page should say so rather
	// than render a red cross somebody has to come and ask about.
	if p.WafProd != "" && p.WafTest == "" {
		p.Notes = append(p.Notes,
			"Test has no WAF on purpose — the behaviour suite runs from CI and would trip a rate rule written for humans.")
	}
	return p
}

// awsJSONRaw sends a CloudWatch query-protocol request and scrapes the alarm
// states out of the XML. A full XML decoder for three fields would be more code
// than the thing it parses.
func awsJSONRaw(ctx context.Context, service, host string, form []byte, p *platform) error {
	region := os.Getenv("AWS_REGION")
	creds, err := awssig.FromEnvironment()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/", bytes.NewReader(form))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	awssig.Sign(req, form, creds, service, region, time.Now())
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("cloudwatch returned %d", resp.StatusCode)
	}
	// ⚠️ A real decoder, not a string split. The first version split on
	// `<member>` -- and an alarm's Dimensions are ALSO `<member>` elements, so
	// any alarm with a dimension was cut in half and lost its StateValue. It
	// reported two of three alarms with an empty state, which reads as "the
	// alarm is broken" rather than "the parser is".
	var parsed struct {
		Alarms []struct {
			Name   string `xml:"AlarmName"`
			State  string `xml:"StateValue"`
			Reason string `xml:"StateReason"`
		} `xml:"DescribeAlarmsResult>MetricAlarms>member"`
	}
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("unreadable DescribeAlarms response")
	}
	for _, a := range parsed.Alarms {
		if a.Name != "" {
			p.Alarms = append(p.Alarms, alarm{Name: a.Name, State: a.State, Reason: a.Reason})
		}
	}
	sort.Slice(p.Alarms, func(i, j int) bool { return p.Alarms[i].Name < p.Alarms[j].Name })
	return nil
}

// apiIDFromBase pulls the REST API id out of an execute-api URL.
func apiIDFromBase(base string) string {
	base = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.Index(base, "."); i > 0 {
		return base[:i]
	}
	return ""
}
