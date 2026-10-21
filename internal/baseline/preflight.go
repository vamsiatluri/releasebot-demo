package baseline

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Preflight answers ONE question, before anything else is attempted:
// which of the reads this tool needs does my role actually have?
//
// # WHY THIS IS THE FIRST THING TO RUN IN SOMEONE ELSE'S ACCOUNT
//
// `capture` dies on the first failure that matters, and on day one in an account
// you have never touched, the likeliest outcome is not "it worked" and not "it
// is broken" -- it is "your role has eleven of the fourteen permissions and
// nobody knows which three are missing". An AccessDenied in the middle of a
// capture reads like the tool is broken. The same AccessDenied, named up front
// against the action that produced it, is a short list to hand the platform team
// on your first morning.
//
// ⚠️ It is also the honest way to open the conversation. "Here is exactly what I
// can and cannot see" is a better first contact than either "give me read-only"
// (too vague to action) or a tool that failed with a stack trace.
//
// ⛔ Every probe here is a READ, and several are deliberately cheap variants --
// `--max-items 1` where the API supports it -- because this runs against
// production and should cost the account nothing noticeable.
type Probe struct {
	Service  string
	Action   string // the IAM action a reader will have to grant
	Required bool   // false = the decommission half, needed only for -estate
	Err      error
}

func (p Probe) OK() bool { return p.Err == nil }

// Denied distinguishes "I am not allowed" from "something else went wrong".
// They need different responses: one is a grant to request, the other is a bug
// or an outage, and reporting the second as the first sends somebody to argue
// with IAM about a problem IAM does not have.
func (p Probe) Denied() bool {
	if p.Err == nil {
		return false
	}
	s := strings.ToLower(p.Err.Error())
	return strings.Contains(s, "accessdenied") ||
		strings.Contains(s, "not authorized") ||
		strings.Contains(s, "explicit deny") ||
		strings.Contains(s, "unauthorizedoperation")
}

func (c *Capturer) Preflight() []Probe {
	probes := []struct {
		service, action string
		required        bool
		args            []string
	}{
		{"sts", "sts:GetCallerIdentity", true, []string{"sts", "get-caller-identity"}},
		{"cloudformation", "cloudformation:ListStacks", true, []string{"cloudformation", "list-stacks", "--max-items", "1"}},
		{"lambda", "lambda:ListFunctions", true, []string{"lambda", "list-functions", "--max-items", "1"}},
		{"iam", "iam:ListRoles", true, []string{"iam", "list-roles", "--max-items", "1"}},
		{"apigateway", "apigateway:GET", true, []string{"apigateway", "get-rest-apis", "--limit", "1"}},
		{"logs", "logs:DescribeLogGroups", true, []string{"logs", "describe-log-groups", "--limit", "1"}},
		{"cloudwatch", "cloudwatch:DescribeAlarms", true, []string{"cloudwatch", "describe-alarms", "--max-records", "1"}},

		// The decommission half. Missing these costs you `-estate` and the
		// teardown plan; it does not stop the migration baseline.
		{"s3", "s3:ListAllMyBuckets", false, []string{"s3api", "list-buckets"}},
		{"dynamodb", "dynamodb:ListTables", false, []string{"dynamodb", "list-tables", "--max-items", "1"}},
		{"ssm", "ssm:DescribeParameters", false, []string{"ssm", "describe-parameters", "--max-results", "1"}},
		{"ec2", "ec2:DescribeVpcs", false, []string{"ec2", "describe-vpcs", "--max-items", "1"}},
		{"sqs", "sqs:ListQueues", false, []string{"sqs", "list-queues"}},
		{"sns", "sns:ListTopics", false, []string{"sns", "list-topics"}},
		{"cloudfront", "cloudfront:ListDistributions", false, []string{"cloudfront", "list-distributions", "--max-items", "1"}},
		{"cognito-idp", "cognito-idp:ListUserPools", false, []string{"cognito-idp", "list-user-pools", "--max-results", "1"}},
		{"wafv2", "wafv2:ListWebACLs", false, []string{"wafv2", "list-web-acls", "--scope", "REGIONAL"}},
	}

	out := make([]Probe, 0, len(probes))
	for _, p := range probes {
		// Discard the body: this asks whether the call is PERMITTED, not what it
		// returns. Passing nil also means an empty account probes identically to
		// a full one, which is the point -- "no buckets" and "may not look" must
		// not be the same answer.
		err := c.aws(nil, p.args...)
		out = append(out, Probe{Service: p.service, Action: p.action, Required: p.required, Err: err})
	}
	return out
}

// WritePreflight prints the table and returns the exit code the CLI should use.
//
//	0  everything needed is readable
//	1  a REQUIRED read is denied -- the migration baseline cannot be taken
//	2  only optional reads are denied -- the baseline works, `-estate` will not
func WritePreflight(w io.Writer, account, region string, ps []Probe) int {
	fmt.Fprintf(w, "\npreflight — account %s · %s\n\n", account, region)

	var missingRequired, missingOptional, broken []Probe
	for _, p := range ps {
		mark, note := "\033[32m✓\033[0m", ""
		switch {
		case p.OK():
		case p.Denied():
			mark = "\033[31m✗\033[0m"
			note = "  denied"
			if p.Required {
				missingRequired = append(missingRequired, p)
			} else {
				missingOptional = append(missingOptional, p)
			}
		default:
			// ⚠️ NOT a permission problem, and saying so matters: a throttle, a
			// region where the service is absent, or an expired token all land
			// here, and none of them is fixed by granting anything.
			mark, note = "\033[33m?\033[0m", "  call failed (NOT a denial)"
			broken = append(broken, p)
		}
		scope := ""
		if !p.Required {
			scope = "  (estate only)"
		}
		fmt.Fprintf(w, "  %s %-34s %s%s\n", mark, p.Action, note, scope)
	}

	fmt.Fprintln(w)
	switch {
	case len(missingRequired) > 0:
		fmt.Fprintf(w, "\033[31m✗ the migration baseline cannot be taken: %d required read(s) denied\033[0m\n\n",
			len(missingRequired))
		fmt.Fprintln(w, "  Ask for exactly these, read-only:")
		for _, p := range sortedActions(missingRequired) {
			fmt.Fprintf(w, "      %s\n", p)
		}
		fmt.Fprintln(w, "\n  ⚠️ AWS's own ReadOnlyAccess or ViewOnlyAccess managed policy covers")
		fmt.Fprintln(w, "     every one of them, and is the smaller ask to make on day one.")
		return 1
	case len(missingOptional) > 0:
		fmt.Fprintf(w, "\033[33m⚠️  the baseline works; the decommission half does not\033[0m\n\n")
		fmt.Fprintln(w, "  `capture -estate` and `teardown` need these as well:")
		for _, p := range sortedActions(missingOptional) {
			fmt.Fprintf(w, "      %s\n", p)
		}
		fmt.Fprintln(w, "\n  Not urgent on day one — this is Phase 7 work.")
		return 2
	case len(broken) > 0:
		fmt.Fprintln(w, "\033[33m⚠️  every permission is present, but some calls failed for another reason.\033[0m")
		fmt.Fprintln(w, "     Check the region and the session, not IAM.")
		return 2
	}
	fmt.Fprintln(w, "\033[32m✓ every read this tool needs is permitted\033[0m")
	return 0
}

func sortedActions(ps []Probe) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Action)
	}
	sort.Strings(out)
	return out
}
