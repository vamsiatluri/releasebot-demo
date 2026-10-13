package baseline

import "testing"

// The shapes a real API Gateway resource policy arrives in. The escaped form is
// what the CLI actually returns, and a parser that only handles clean JSON
// reports "no restriction" on a policy that restricts — which is the dangerous
// direction, because it reads as "this endpoint is open" when it is not.
func TestPolicyCidrs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"empty", "", 0},
		{"whitespace", "   ", 0},
		{"unparseable is not a crash", "{not json", 0},
		{"escaped quotes, as the CLI returns them",
			`{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Deny\",\"Condition\":{\"NotIpAddress\":{\"aws:SourceIp\":[\"9.234.106.48/28\",\"140.82.112.0/20\"]}}}]}`, 2},
		{"clean JSON, single string not a list",
			`{"Statement":[{"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`, 1},
		{"several statements, duplicates collapsed",
			`{"Statement":[{"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}},` +
				`{"Condition":{"NotIpAddress":{"aws:SourceIp":["10.0.0.0/8","192.168.0.0/16"]}}}]}`, 2},
		// A policy that restricts by VPC endpoint names no CIDR. Zero is the
		// correct answer and must not be confused with "no policy" — the raw
		// text is kept either way.
		{"restricts by VPC endpoint, not by IP",
			`{"Statement":[{"Condition":{"StringNotEquals":{"aws:SourceVpce":"vpce-123"}}}]}`, 0},
	}
	for _, c := range cases {
		if got := len(policyCidrs(c.raw)); got != c.want {
			t.Errorf("%s: want %d CIDRs, got %d (%v)", c.name, c.want, got, policyCidrs(c.raw))
		}
	}
}

// Order must be stable or every capture diff shows phantom changes.
func TestPolicyCidrsIsSorted(t *testing.T) {
	got := policyCidrs(`{"Statement":[{"Condition":{"IpAddress":{"aws:SourceIp":["9.9.9.9/32","1.1.1.1/32"]}}}]}`)
	if len(got) != 2 || got[0] != "1.1.1.1/32" {
		t.Fatalf("want sorted, got %v", got)
	}
}
