package jiraclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recorded struct {
	method, path, query, auth string
	body                      map[string]any
}

func server(t *testing.T, handle func(r recorded, w http.ResponseWriter)) (*Client, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &rec.body)
		mu.Lock()
		seen = append(seen, rec)
		mu.Unlock()
		// ⛔ The removed endpoints: a real Jira answers 410.
		if rec.path == "/rest/api/2/search" || rec.path == "/rest/api/3/search" {
			w.WriteHeader(http.StatusGone)
			return
		}
		handle(rec, w)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "bot@example.test", "tok", false), &seen
}

func assertNoRemovedSearch(t *testing.T, seen []recorded) {
	t.Helper()
	for _, r := range seen {
		if IsRemovedSearchPath(r.path) {
			t.Fatalf("called the removed endpoint %s %s", r.method, r.path)
		}
	}
}

func TestCreateIssueUsesV3AndBasicAuth(t *testing.T) {
	c, seen := server(t, func(r recorded, w http.ResponseWriter) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"1","key":"DEMOREL-5"}`))
	})
	key, err := c.CreateIssue(context.Background(), map[string]any{"summary": "RELEASE: Kettle release v1.0.0"})
	if err != nil || key != "DEMOREL-5" {
		t.Fatalf("%s %v", key, err)
	}
	r := (*seen)[0]
	if r.method != "POST" || r.path != "/rest/api/3/issue" {
		t.Fatalf("%s %s", r.method, r.path)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.test:tok")); r.auth != want {
		t.Fatalf("auth %q", r.auth)
	}
	if r.body["fields"].(map[string]any)["summary"] != "RELEASE: Kettle release v1.0.0" {
		t.Fatalf("body %v", r.body)
	}
	if got := c.BrowseURL("DEMOREL-5"); !strings.HasSuffix(got, "/browse/DEMOREL-5") {
		t.Fatal(got)
	}
}

func TestSearchJQLPaginatesOnTheEnhancedEndpoint(t *testing.T) {
	c, seen := server(t, func(r recorded, w http.ResponseWriter) {
		if r.path != "/rest/api/3/search/jql" || r.method != "POST" {
			w.WriteHeader(404)
			return
		}
		if r.body["nextPageToken"] == nil {
			w.Write([]byte(`{"issues":[{"key":"DEMO-1"}],"nextPageToken":"p2","isLast":false}`))
			return
		}
		w.Write([]byte(`{"issues":[{"key":"DEMO-2"}],"isLast":true}`))
	})
	issues, err := c.SearchJQL(context.Background(), "key in (DEMO-1,DEMO-2)", []string{"issuetype"})
	if err != nil || len(issues) != 2 || issues[1].Key != "DEMO-2" {
		t.Fatalf("%v %v", issues, err)
	}
	if len(*seen) != 2 || (*seen)[1].body["nextPageToken"] != "p2" {
		t.Fatalf("pagination: %+v", *seen)
	}
	assertNoRemovedSearch(t, *seen)
}

func TestRemovedSearchEndpointsAreRefusedLocally(t *testing.T) {
	c, seen := server(t, func(recorded, http.ResponseWriter) {})
	for _, p := range []string{"/rest/api/2/search", "/rest/api/3/search", "/rest/api/3/search?jql=x", "/rest/api/2/search/"} {
		if err := c.do(context.Background(), http.MethodGet, p, nil, nil); !errors.Is(err, ErrRemovedEndpoint) {
			t.Errorf("%s: err = %v", p, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("a removed endpoint reached the server: %+v", *seen)
	}
	if IsRemovedSearchPath("/rest/api/3/search/jql") {
		t.Fatal("the enhanced endpoint is not the removed one")
	}
}

func TestTransitionVersionsAndRead(t *testing.T) {
	c, seen := server(t, func(r recorded, w http.ResponseWriter) {
		switch {
		case r.method == "GET" && r.path == "/rest/api/3/project/10002/versions":
			w.Write([]byte(`[{"id":"7","name":"v0.9.0 Kettle"}]`))
		case r.method == "POST" && r.path == "/rest/api/3/version":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"8"}`))
		case r.method == "GET" && r.path == "/rest/api/3/issue/DEMOREL-5":
			w.Write([]byte(`{"key":"DEMOREL-5","fields":{"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"DEMO-3 lid"}]}]}}}`))
		default:
			w.WriteHeader(204)
		}
	})
	ctx := context.Background()
	if err := c.Transition(ctx, "DEMO-3", "41"); err != nil {
		t.Fatal(err)
	}
	id, err := c.EnsureVersion(ctx, "10002", "v1.0.0 Kettle")
	if err != nil || id != "8" {
		t.Fatalf("EnsureVersion %s %v", id, err)
	}
	if err := c.AddFixVersion(ctx, "DEMO-3", "v1.0.0 Kettle"); err != nil {
		t.Fatal(err)
	}
	is, err := c.GetIssue(ctx, "DEMOREL-5", []string{"description"})
	if err != nil || is.FieldString("description") != "DEMO-3 lid" {
		t.Fatalf("%q %v", is.FieldString("description"), err)
	}
	want := []string{
		"POST /rest/api/3/issue/DEMO-3/transitions",
		"GET /rest/api/3/project/10002/versions",
		"POST /rest/api/3/version",
		"PUT /rest/api/3/issue/DEMO-3",
		"GET /rest/api/3/issue/DEMOREL-5",
	}
	for i, w := range want {
		if got := (*seen)[i].method + " " + (*seen)[i].path; got != w {
			t.Errorf("call %d = %s, want %s", i, got, w)
		}
	}
	if (*seen)[0].body["transition"].(map[string]any)["id"] != "41" {
		t.Errorf("transition body %v", (*seen)[0].body)
	}
	if (*seen)[2].body["projectId"] != float64(10002) {
		t.Errorf("projectId %v", (*seen)[2].body)
	}
	assertNoRemovedSearch(t, *seen)
}

func TestADFRoundTrip(t *testing.T) {
	text := "v1.0.0 - https://github.com/acme-demo/kettle/compare/v0.9.0...v1.0.0\n\nabc -- alice -- DEMO-1 fix"
	raw, _ := json.Marshal(TextToADF(text))
	got := RawToText(raw)
	if !strings.Contains(got, "compare/v0.9.0...v1.0.0") || !strings.Contains(got, "DEMO-1 fix") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(string(raw), `"href":"https://github.com/acme-demo/kettle/compare/v0.9.0...v1.0.0"`) {
		t.Fatalf("link not marked: %s", raw)
	}
	if RawToText(json.RawMessage(`"plain"`)) != "plain" || RawToText(json.RawMessage(`{"value":"Yes"}`)) != "Yes" {
		t.Fatal("scalar/option flattening")
	}
}
