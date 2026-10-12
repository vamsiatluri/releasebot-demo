package handler

import (
	"encoding/json"
	"testing"
)

// An async job is recognised ONLY by its explicit marker.
//
// This is a security boundary, not a parsing convenience. A self-dispatched job
// arrives over lambda:InvokeFunction with no Slack signature on it; an API
// Gateway event arrives over HTTP and is signature-checked. If a malformed or
// truncated proxy event could be mistaken for a job, an unsigned request would
// reach the code that cuts a release.
func TestIsAsyncJob(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"a real job", `{"releasebotAsyncJob":true,"action":"cut","command":{"Repo":"msnbc/news-app"}}`, true},
		{"marker false", `{"releasebotAsyncJob":false,"action":"cut"}`, false},
		{"marker absent", `{"action":"cut","command":{"Repo":"msnbc/news-app"}}`, false},
		// The shapes that matter: anything an HTTP path can produce must NOT
		// be read as a job.
		{"an api gateway event", `{"path":"/beta/cut","httpMethod":"POST","body":"text=news-app+5.4.0"}`, false},
		{"an empty object", `{}`, false},
		{"empty bytes", ``, false},
		{"not json at all", `text=news-app+5.4.0`, false},
		{"null", `null`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := IsAsyncJob([]byte(c.payload)); got != c.want {
				t.Fatalf("IsAsyncJob(%q) = %v, want %v", c.payload, got, c.want)
			}
		})
	}
}

// The job must survive a round trip, because the two ends are different
// invocations: a field that marshals but does not unmarshal would lose, say,
// the actor -- and the ledger row would then name nobody.
func TestAsyncJobRoundTrip(t *testing.T) {
	in := AsyncJob{Marker: true, Action: ActionMergeback, ResponseURL: "https://hooks.slack.com/x"}
	in.Command.Repo = "msnbc/today-web"
	in.Command.Version = "2026.10.2"
	in.Command.Actor = "jane.doe"
	in.Command.Source = "slack"

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := IsAsyncJob(b)
	if !ok {
		t.Fatal("a job we just marshalled was not recognised as one")
	}
	if out.Action != in.Action || out.ResponseURL != in.ResponseURL ||
		out.Command.Repo != in.Command.Repo || out.Command.Version != in.Command.Version ||
		out.Command.Actor != in.Command.Actor || out.Command.Source != in.Command.Source {
		t.Fatalf("round trip lost data:\n in: %+v\nout: %+v", in, out)
	}
}
