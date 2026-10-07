// Command mockapis stands in for GitHub and Slack during local runs.
//
// It holds branch and pull-request state in memory so the idempotency
// behaviour -- re-cutting the same branch, re-opening the same mergeback PR --
// can actually be observed rather than asserted.
//
//	go run ./cmd/mockapis    # listens on :9099
//	GET /_state              # dump what the fake GitHub currently believes
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu       sync.Mutex
	branches map[string]string // "owner/repo@branch" -> sha
	prs      map[string]pr     // "owner/repo@head->base"
	nextPR   int
	slack    []map[string]any
}

type pr struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Title   string `json:"title"`
	Head    string `json:"-"`
	Base    string `json:"-"`
}

func main() {
	s := &state{
		branches: map[string]string{
			"msnbc/news-app@main":      "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
			"msnbc/payments-core@main": "99887766554433221100aabbccddeeff00112233",
			// The shadow repo the contract suite drives. A real migration wants
			// the same thing in the real GitHub org: an empty repository with
			// production's branch protection that a test bot may cut branches in.
			"msnbc/news-app-shadow@main": "5ad0w5ad0w5ad0w5ad0w5ad0w5ad0w5ad0w5ad0w",
		},
		prs:    map[string]pr{},
		nextPR: 480,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", s.repos)
	mux.HandleFunc("/chat.postMessage", s.slackPost)
	mux.HandleFunc("/_response_url", s.slackPost)
	mux.HandleFunc("/_state", s.dump)

	fmt.Println("mock github+slack listening on :9099")
	log.Fatal((&http.Server{Addr: ":9099", Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}

func (s *state) repos(w http.ResponseWriter, r *http.Request) {
	// /repos/{owner}/{repo}/...
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 3)
	if len(parts) < 3 {
		http.Error(w, "bad path", 404)
		return
	}
	repo := parts[0] + "/" + parts[1]
	rest := parts[2]

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "git/ref/heads/"):
		branch := strings.TrimPrefix(rest, "git/ref/heads/")
		sha, ok := s.branches[repo+"@"+branch]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha}})

	case r.Method == http.MethodPost && rest == "git/refs":
		var in struct{ Ref, SHA string }
		var raw map[string]string
		json.NewDecoder(r.Body).Decode(&raw)
		in.Ref, in.SHA = raw["ref"], raw["sha"]
		branch := strings.TrimPrefix(in.Ref, "refs/heads/")
		key := repo + "@" + branch
		if _, exists := s.branches[key]; exists {
			writeJSON(w, 422, map[string]string{"message": "Reference already exists"})
			return
		}
		s.branches[key] = in.SHA
		writeJSON(w, 201, map[string]any{"ref": in.Ref, "object": map[string]string{"sha": in.SHA}})

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "pulls"):
		q := r.URL.Query()
		head := q.Get("head")
		if i := strings.Index(head, ":"); i >= 0 {
			head = head[i+1:]
		}
		key := repo + "@" + head + "->" + q.Get("base")
		if p, ok := s.prs[key]; ok {
			writeJSON(w, 200, []pr{p})
			return
		}
		writeJSON(w, 200, []pr{})

	case r.Method == http.MethodPost && rest == "pulls":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if _, ok := s.branches[repo+"@"+in["head"]]; !ok {
			writeJSON(w, 422, map[string]string{"message": "Validation Failed"})
			return
		}
		s.nextPR++
		p := pr{Number: s.nextPR, State: "open", Title: in["title"],
			HTMLURL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, s.nextPR)}
		s.prs[repo+"@"+in["head"]+"->"+in["base"]] = p
		writeJSON(w, 201, p)

	default:
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
	}
}

func (s *state) slackPost(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	s.slack = append(s.slack, in)
	s.mu.Unlock()
	fmt.Printf("  [slack] %v\n", in["text"])
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *state) dump(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"branches": s.branches, "pull_requests": s.prs, "slack": s.slack})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
