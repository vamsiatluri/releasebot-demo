package baseline

import "testing"

// ⛔ 12 of 31 differences in a real parity run were this: the same role from the
// same template, with CloudFormation's random suffix, reported as absent in one
// account and present in the other. A comparator with a permanent floor of
// noise can never reach zero differences, which is the only number the parallel
// run exists to produce.
func TestLogicalNameStripsGeneratedSuffixes(t *testing.T) {
	cases := []struct {
		in, want   string
		stackOwned bool
	}{
		// The real pair that produced two spurious blockers.
		{"releasebot-dashboard-ApiRole-X1Lyy4HP4DBG", "releasebot-dashboard-ApiRole", true},
		{"releasebot-dashboard-ApiRole-ttjoWT4DNnuB", "releasebot-dashboard-ApiRole", true},
		{"releasebot-slack-approve-ExecutionRole-3j42Cx6xDYj3", "releasebot-slack-approve-ExecutionRole", true},

		// ⚠️ A NAMED resource must be untouched — these are the ones a template
		// controls, and truncating them would be a false MATCH.
		{"releasebot-exec-cutrelease", "releasebot-exec-cutrelease", true},
		{"releasebot-exec-mergeback-test", "releasebot-exec-mergeback-test", true},

		// ⛔ A HAND-MADE role keeps its whole name whatever it looks like. A
		// false match between two genuinely different roles is the expensive
		// direction; a false difference is merely noisy.
		{"someones-ApiRole-X1Lyy4HP4DBG", "someones-ApiRole-X1Lyy4HP4DBG", false},

		// Real words must survive: single-case, or containing a separator.
		{"my-role-executionrole", "my-role-executionrole", true},
		{"my-role-EXECUTIONROLE", "my-role-EXECUTIONROLE", true},
		{"my-role-with_underscore", "my-role-with_underscore", true},
		{"short-AbC1", "short-AbC1", true},
		{"noseparator", "noseparator", true},
	}
	for _, c := range cases {
		if got := logicalName(c.in, c.stackOwned); got != c.want {
			t.Errorf("logicalName(%q, stackOwned=%v) = %q, want %q", c.in, c.stackOwned, got, c.want)
		}
	}
}

// The two halves of the real pair must now collide — that IS the fix.
func TestTheRealPairNowMatches(t *testing.T) {
	a := logicalName("releasebot-dashboard-ApiRole-X1Lyy4HP4DBG", true)
	b := logicalName("releasebot-dashboard-ApiRole-ttjoWT4DNnuB", true)
	if a != b {
		t.Fatalf("the same role in two accounts must compare equal: %q vs %q", a, b)
	}
}
