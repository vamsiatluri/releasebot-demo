package main

import "testing"

// Pins the adaptive TTL. The case that matters is "waiting": a run held at the
// approval gate must refresh FAST, because a person is watching for their own
// approval to register, and those reads are 304s that cost nothing.
func TestRunTTLIsFastWhileARunIsLive(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	for _, live := range []string{"queued", "in_progress", "waiting", "requested", "pending"} {
		runCache.val = &runState{Status: live}
		if got := runTTL(); got > 20_000_000_000 {
			t.Fatalf("status %q must refresh quickly, got %s", live, got)
		}
	}
	runCache.val = &runState{Status: "completed"}
	if got := runTTL(); got < 60_000_000_000 {
		t.Fatalf("a finished run should refresh slowly, got %s", got)
	}
	runCache.val = nil
}

func TestWaitingIsNotTerminal(t *testing.T) {
	if terminal("waiting") {
		t.Fatal("a run held at the approval gate is NOT finished — it is the state most worth refreshing")
	}
	if !terminal("completed") {
		t.Fatal("completed must be terminal")
	}
}
