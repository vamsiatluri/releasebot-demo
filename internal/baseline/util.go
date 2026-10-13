package baseline

import (
	"encoding/json"
	"sort"
	"strings"
)

func sortStrings(s []string) { sort.Strings(s) }

// policyCidrs pulls every source-IP CIDR an API Gateway resource policy names.
//
// ⚠️ The policy arrives as a STRING containing JSON, and API Gateway escapes
// the inner quotes -- so a plain Unmarshal of the field gives you a string that
// still has \" in it and parses as nothing. Both forms are tried, and an
// unparseable policy is reported as no CIDRs rather than as no policy: the raw
// text is kept in ResourcePolicy either way, so a human can always read it.
//
// Returning no CIDRs for a non-empty policy is a real answer, not a failure --
// it means the policy restricts by something other than source IP (a VPC
// endpoint, an account, a service principal), which is worth knowing.
func policyCidrs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		un := strings.ReplaceAll(raw, `\"`, `"`)
		if json.Unmarshal([]byte(un), &doc) != nil {
			return nil
		}
	}
	seen := map[string]bool{}
	var out []string
	// Walk the whole document rather than assuming the shape: a policy may hold
	// IpAddress, NotIpAddress, several statements, and either a string or a
	// list for aws:SourceIp. A parser that assumes one shape silently reports
	// "no restriction" on a policy that restricts.
	var walk func(any, string)
	walk = func(n any, key string) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				walk(child, k)
			}
		case []any:
			for _, child := range v {
				walk(child, key)
			}
		case string:
			if key == "aws:SourceIp" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	walk(doc, "")
	sort.Strings(out)
	return out
}
