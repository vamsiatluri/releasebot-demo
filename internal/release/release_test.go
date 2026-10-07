package release

import "testing"

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name, text string
		wantRepo   string
		wantVer    string
		wantErr    bool
	}{
		{"bare repo takes the default owner", "news-app 5.4.0", "msnbc/news-app", "5.4.0", false},
		{"explicit owner wins", "other/news-app 5.4.0", "other/news-app", "5.4.0", false},
		{"a leading v is tolerated", "news-app v5.4.0", "msnbc/news-app", "5.4.0", false},
		{"prerelease is allowed", "news-app 5.4.0-rc.1", "msnbc/news-app", "5.4.0-rc.1", false},
		{"extra words are ignored", "news-app 5.4.0 please", "msnbc/news-app", "5.4.0", false},
		// A two-part version is the dangerous one: "5.4" would silently create
		// release/5.4, which no downstream tooling expects.
		{"two-part version is rejected", "news-app 5.4", "", "", true},
		{"missing version", "news-app", "", "", true},
		{"empty", "", "", "", true},
		{"path traversal in the repo name", "../../etc 5.4.0", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCommand(c.text, "msnbc")
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Repo != c.wantRepo || got.Version != c.wantVer {
				t.Fatalf("got %s %s, want %s %s", got.Repo, got.Version, c.wantRepo, c.wantVer)
			}
		})
	}
}

func TestParseCommandWithoutDefaultOwner(t *testing.T) {
	if _, err := ParseCommand("news-app 5.4.0", ""); err == nil {
		t.Fatal("expected an error when the repo has no owner and none is configured")
	}
}
