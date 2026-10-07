package baseline

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Diff is one difference between two accounts.
type Diff struct {
	Severity Severity
	Resource string
	Field    string
	A, B     string
}

// Compare is the reason this tool exists.
//
// Capturing one account is mildly useful. Capturing both and printing the
// differences is the production-readiness evidence the migration's validation
// phase is asking for. "The production account matches the baseline" stops being
// an assurance somebody gives in a meeting and becomes a diff somebody reads.
//
// Fields that are EXPECTED to differ between accounts -- account ids inside
// ARNs, API ids, version numbers, code hashes that move independently -- are
// normalised rather than reported, otherwise the real differences drown in
// noise. That normalisation is the difference between a tool people use and a
// tool people ignore.
func Compare(a, b *Baseline) []Diff {
	var d []Diff
	norm := func(s string) string {
		s = strings.ReplaceAll(s, a.Account, "<ACCOUNT>")
		s = strings.ReplaceAll(s, b.Account, "<ACCOUNT>")
		if a.Region != "" {
			s = strings.ReplaceAll(s, a.Region, "<REGION>")
		}
		if b.Region != "" {
			s = strings.ReplaceAll(s, b.Region, "<REGION>")
		}
		return s
	}
	add := func(sev Severity, res, field, av, bv string) {
		if norm(av) != norm(bv) {
			d = append(d, Diff{sev, res, field, av, bv})
		}
	}

	// --- functions ---
	fa, fb := indexFunctions(a), indexFunctions(b)
	for _, name := range union(keysF(fa), keysF(fb)) {
		x, inA := fa[name]
		y, inB := fb[name]
		res := "lambda/" + name
		if !inA {
			d = append(d, Diff{Blocker, res, "exists", "absent", "present"})
			continue
		}
		if !inB {
			d = append(d, Diff{Blocker, res, "exists", "present", "absent"})
			continue
		}
		add(Blocker, res, "runtime", x.Runtime, y.Runtime)
		add(Blocker, res, "handler", x.Handler, y.Handler)
		add(Blocker, res, "architectures", strings.Join(x.Architectures, ","), strings.Join(y.Architectures, ","))
		add(Warn, res, "memory", fmt.Sprint(x.MemorySize), fmt.Sprint(y.MemorySize))
		add(Warn, res, "timeout", fmt.Sprint(x.Timeout), fmt.Sprint(y.Timeout))
		add(Warn, res, "reserved concurrency", concStr(x.ReservedConc), concStr(y.ReservedConc))
		add(Warn, res, "layers", strings.Join(x.Layers, ","), strings.Join(y.Layers, ","))
		add(Warn, res, "dead letter queue", x.DLQ, y.DLQ)

		// ★ The most valuable single comparison in the whole tool. Identical
		// code hashes across two accounts is the proof that "build once, deploy
		// many" actually happened, rather than each account having built its own
		// artifact from the same commit and hoped.
		add(Blocker, res, "code sha256", x.CodeSha256, y.CodeSha256)

		// Env var KEYS are compared; values never are, because secret values are
		// never captured and non-secret values legitimately differ by
		// environment. A missing KEY, though, is a function that will fail to
		// start in one account and not the other.
		add(Blocker, res, "environment variable names",
			strings.Join(x.EnvKeys, ","), strings.Join(y.EnvKeys, ","))

		add(Blocker, res, "aliases", aliasStr(x.Aliases), aliasStr(y.Aliases))
		add(Warn, res, "stack owned", ownStr(x.StackOwned, x.OwningStack), ownStr(y.StackOwned, y.OwningStack))
	}

	// --- roles ---
	ra, rb := indexRoles(a), indexRoles(b)
	for _, name := range union(keysR(ra), keysR(rb)) {
		x, inA := ra[name]
		y, inB := rb[name]
		res := "iam/" + name
		if !inA || !inB {
			d = append(d, Diff{Blocker, res, "exists", presence(inA), presence(inB)})
			continue
		}
		add(Blocker, res, "managed policies",
			strings.Join(sorted(x.ManagedPolicies), ","), strings.Join(sorted(y.ManagedPolicies), ","))
		add(Blocker, res, "inline policy names",
			strings.Join(sortedKeys(x.InlinePolicies), ","), strings.Join(sortedKeys(y.InlinePolicies), ","))
		add(Blocker, res, "trust policy", x.TrustPolicy, y.TrustPolicy)
	}

	// --- apis (matched by NAME; the id necessarily differs between accounts) ---
	aa, ab := indexAPIs(a), indexAPIs(b)
	for _, name := range union(keysA(aa), keysA(ab)) {
		x, inA := aa[name]
		y, inB := ab[name]
		res := "apigateway/" + name
		if !inA || !inB {
			d = append(d, Diff{Blocker, res, "exists", presence(inA), presence(inB)})
			continue
		}
		add(Blocker, res, "routes", routeStr(x.Routes), routeStr(y.Routes))
		add(Blocker, res, "stages", stageNames(x.Stages), stageNames(y.Stages))
		for _, sx := range x.Stages {
			for _, sy := range y.Stages {
				if sx.Name != sy.Name {
					continue
				}
				sres := res + "/" + sx.Name
				add(Blocker, sres, "stage variables", kvStr(sx.Variables), kvStr(sy.Variables))
				add(Warn, sres, "logging level", sx.LoggingLevel, sy.LoggingLevel)
				add(Blocker, sres, "full request logging", fmt.Sprint(sx.DataTrace), fmt.Sprint(sy.DataTrace))
				add(Warn, sres, "throttle rate", fmt.Sprint(sx.ThrottleRate), fmt.Sprint(sy.ThrottleRate))
				add(Warn, sres, "access logging", present(sx.AccessLogDest), present(sy.AccessLogDest))
			}
		}
	}

	// --- log retention: cheap to get wrong, expensive to leave wrong ---
	la, lb := indexLogs(a), indexLogs(b)
	for _, name := range union(keysL(la), keysL(lb)) {
		x, inA := la[name]
		y, inB := lb[name]
		if !inA || !inB {
			d = append(d, Diff{Info, "logs/" + name, "exists", presence(inA), presence(inB)})
			continue
		}
		add(Warn, "logs/"+name, "retention days", retStr(x.RetentionDays), retStr(y.RetentionDays))
	}

	sort.SliceStable(d, func(i, j int) bool { return rank(d[i].Severity) < rank(d[j].Severity) })
	return d
}

func WriteCompare(w io.Writer, a, b *Baseline, diffs []Diff) {
	fmt.Fprintf(w, "\nComparing\n  A: %s (account %s)\n  B: %s (account %s)\n\n",
		a.Label, a.Account, b.Label, b.Account)
	if len(diffs) == 0 {
		fmt.Fprintln(w, "  No differences. The two accounts are configured identically.")
		fmt.Fprintln(w, "  This is the production-readiness evidence: not 'both are green',")
		fmt.Fprintln(w, "  but 'both are green in the same way'.")
		return
	}
	for _, x := range diffs {
		fmt.Fprintf(w, "  [%-7s] %s  —  %s\n", x.Severity, x.Resource, x.Field)
		fmt.Fprintf(w, "            A: %s\n            B: %s\n\n", trunc(x.A), trunc(x.B))
	}
	var bl int
	for _, x := range diffs {
		if x.Severity == Blocker {
			bl++
		}
	}
	fmt.Fprintf(w, "  %d differences, %d of them blockers.\n\n", len(diffs), bl)
}

// --- small helpers -----------------------------------------------------------

func indexFunctions(b *Baseline) map[string]Function {
	m := map[string]Function{}
	for _, f := range b.Functions {
		m[f.Name] = f
	}
	return m
}
func indexRoles(b *Baseline) map[string]Role {
	m := map[string]Role{}
	for _, r := range b.Roles {
		m[r.Name] = r
	}
	return m
}
func indexAPIs(b *Baseline) map[string]API {
	m := map[string]API{}
	for _, a := range b.APIs {
		m[a.Name] = a
	}
	return m
}
func indexLogs(b *Baseline) map[string]LogGroup {
	m := map[string]LogGroup{}
	for _, l := range b.LogGroups {
		m[l.Name] = l
	}
	return m
}

func keysF(m map[string]Function) []string {
	k := []string{}
	for x := range m {
		k = append(k, x)
	}
	return k
}
func keysR(m map[string]Role) []string {
	k := []string{}
	for x := range m {
		k = append(k, x)
	}
	return k
}
func keysA(m map[string]API) []string {
	k := []string{}
	for x := range m {
		k = append(k, x)
	}
	return k
}
func keysL(m map[string]LogGroup) []string {
	k := []string{}
	for x := range m {
		k = append(k, x)
	}
	return k
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(a, b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string { c := append([]string{}, s...); sort.Strings(c); return c }
func sortedKeys(m map[string]string) []string {
	k := []string{}
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
func presence(b bool) string {
	if b {
		return "present"
	}
	return "absent"
}
func present(s string) string {
	if s == "" {
		return "none"
	}
	return "configured"
}
func concStr(p *int) string {
	if p == nil {
		return "unset"
	}
	return fmt.Sprint(*p)
}
func retStr(d int) string {
	if d == 0 {
		return "NEVER EXPIRES"
	}
	return fmt.Sprintf("%d days", d)
}
func ownStr(owned bool, stack string) string {
	if !owned {
		return "NOT in a stack"
	}
	return "stack " + stack
}
func aliasStr(as []Alias) string {
	var s []string
	for _, a := range as {
		s = append(s, a.Name)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}
func routeStr(rs []Route) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.Method+" "+r.Path)
	}
	sort.Strings(s)
	return strings.Join(s, " | ")
}
func stageNames(ss []Stage) string {
	var s []string
	for _, x := range ss {
		s = append(s, x.Name)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}
func kvStr(m map[string]string) string {
	var s []string
	for k, v := range m {
		if IsSecretish(k) {
			v = "(withheld)"
		}
		s = append(s, k+"="+v)
	}
	sort.Strings(s)
	return strings.Join(s, " ")
}
func trunc(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
