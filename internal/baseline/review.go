package baseline

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type Severity string

const (
	Blocker Severity = "BLOCKER" // migrating this as-is causes an incident
	Warn    Severity = "WARN"    // decide deliberately before carrying it over
	Info    Severity = "INFO"    // worth knowing, not a problem
)

type Finding struct {
	Severity Severity
	Resource string
	What     string
	Why      string
}

// Review inspects a single captured account and reports what a person should
// look at before copying it anywhere.
//
// This is the part that makes the capture worth running even if you never
// generate a template from it. The questions it answers are the ones Phase 1 is
// actually asking: what here was made by hand, what will outlive the
// decommission, and what must NOT be copied across as-is.
func Review(b *Baseline) []Finding {
	var f []Finding

	for _, fn := range b.Functions {
		if !fn.StackOwned {
			f = append(f, Finding{Blocker, "lambda/" + fn.Name,
				"not owned by any CloudFormation stack",
				"It was created or last changed by hand, so it is not in any template. A baseline taken from templates would miss it entirely, and whatever was changed in the console is not recorded anywhere."})
		}
		if len(fn.SecretLooking) > 0 {
			f = append(f, Finding{Blocker, "lambda/" + fn.Name,
				"holds secret values in environment variables: " + strings.Join(fn.SecretLooking, ", "),
				"Anyone who can read the function's configuration can read these. They must be moved deliberately, never copied between accounts by this or any tool."})
		}
		if len(fn.Aliases) == 0 {
			f = append(f, Finding{Warn, "lambda/" + fn.Name,
				"has no alias",
				"Whatever invokes it is pointed at a mutable target, so there is no one-step rollback. Adding an alias is additive and breaks nothing."})
		}
		for _, a := range fn.Aliases {
			if a.RoutingConfig != "" && a.RoutingConfig != "null" {
				f = append(f, Finding{Warn, "lambda/" + fn.Name + ":" + a.Name,
					"alias is splitting traffic between versions",
					"Someone left a canary running, or a rollout is half-finished. Copying this to a new account carries the split with it."})
			}
		}
		if fn.ReservedConc == nil {
			f = append(f, Finding{Info, "lambda/" + fn.Name,
				"no reserved concurrency",
				"Nothing caps how many copies can run at once. For a bot that writes to source control, a retry storm is worth bounding."})
		}
		if fn.ResourcePolicy == "" {
			f = append(f, Finding{Warn, "lambda/" + fn.Name,
				"no resource policy captured",
				"If nothing is permitted to invoke it, recreating it elsewhere gives you a function that exists and that nothing can call."})
		}
		if strings.HasPrefix(fn.Runtime, "provided.al2") && fn.Runtime == "provided.al2" {
			f = append(f, Finding{Info, "lambda/" + fn.Name,
				"runtime is provided.al2",
				"The previous generation. provided.al2023 is current and uses the same contract. Worth a separate, approved change after the migration rather than bundled into it."})
		}
	}

	for _, r := range b.Roles {
		if !r.StackOwned {
			f = append(f, Finding{Blocker, "iam/" + r.Name,
				"role is not owned by any stack",
				"The permissions the application runs with exist only in the console. Recreating the account from templates silently produces different permissions."})
		}
		if len(r.InlinePolicies) > 0 {
			names := make([]string, 0, len(r.InlinePolicies))
			for n := range r.InlinePolicies {
				names = append(names, n)
			}
			sort.Strings(names)
			// Severity depends on OWNERSHIP, not on the existence of inline
			// policy. Inline policy on a stack-owned role is a normal design
			// choice and is already in a template. On a role no stack owns, the
			// same thing is undocumented permission that a template-derived
			// baseline would miss entirely. Flagging both the same way trains
			// people to ignore the finding.
			sev := Info
			why := "Inline policy on a stack-owned role is already in a template; recorded so the target account can be checked against it."
			if !r.StackOwned {
				sev = Warn
				why = "This role is owned by no stack, so its inline policy exists only in the console. A template-derived baseline would miss it, and the new account would silently run with different permissions."
			}
			f = append(f, Finding{sev, "iam/" + r.Name,
				"carries inline policies: " + strings.Join(names, ", "), why})
		}
		if strings.Contains(r.TrustPolicy, `"*"`) {
			f = append(f, Finding{Blocker, "iam/" + r.Name,
				"trust policy contains a wildcard principal",
				"Something broader than intended can assume this role. Do not carry it across; narrow it as part of the move."})
		}
	}

	for _, g := range b.LogGroups {
		if g.Implicit {
			f = append(f, Finding{Warn, "logs/" + g.Name,
				fmt.Sprintf("never expires (%s stored)", humanBytes(g.StoredBytes)),
				"Created by Lambda rather than declared in a template, so it is owned by no stack, survives the decommission, and keeps billing after the project is finished."})
		}
	}

	for _, api := range b.APIs {
		if !api.StackOwned {
			f = append(f, Finding{Blocker, "apigateway/" + api.Name,
				"API is not owned by any stack",
				"The public front door was configured by hand. Everything about it -- routes, stages, throttling -- has to be read out of the account, not a template."})
		}
		for _, s := range api.Stages {
			if s.DataTrace {
				f = append(f, Finding{Blocker, "apigateway/" + api.Name + "/" + s.Name,
					"full request/response logging is enabled",
					"This writes request bodies to CloudWatch. These request bodies carry Slack tokens. Turn it off before anything else."})
			}
			if s.AccessLogDest == "" {
				f = append(f, Finding{Warn, "apigateway/" + api.Name + "/" + s.Name,
					"no access logging",
					"There is no record of who called the public endpoint, which is the first thing you want during an incident on an internet-facing route."})
			}
			if s.ThrottleRate == 0 {
				f = append(f, Finding{Warn, "apigateway/" + api.Name + "/" + s.Name,
					"no throttling configured",
					"A public endpoint with no rate limit in front of a bot that writes to source control."})
			}
			for k, v := range s.Variables {
				if IsSecretish(k) {
					f = append(f, Finding{Blocker, "apigateway/" + api.Name + "/" + s.Name,
						"stage variable " + k + " looks like a credential",
						"Stage variables are not a secret store; they are readable by anyone who can describe the stage. Value withheld here: " + redact(v)})
				}
			}
		}
	}

	for _, a := range b.Alarms {
		if !a.ActionsEnabled || len(a.Actions) == 0 {
			f = append(f, Finding{Warn, "alarm/" + a.Name,
				"alarm notifies nobody",
				"It will turn red on a dashboard and page no one. An alarm with no action is decoration."})
		}
	}
	if len(b.Alarms) == 0 {
		f = append(f, Finding{Warn, "account",
			"no alarms found",
			"Nothing tells anyone when this breaks."})
	}

	sort.SliceStable(f, func(i, j int) bool { return rank(f[i].Severity) < rank(f[j].Severity) })
	return f
}

func rank(s Severity) int {
	switch s {
	case Blocker:
		return 0
	case Warn:
		return 1
	}
	return 2
}

func redact(s string) string {
	if len(s) <= 4 {
		return "(withheld)"
	}
	return fmt.Sprintf("(withheld, %d chars)", len(s))
}

func humanBytes(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func WriteReview(w io.Writer, b *Baseline, findings []Finding) {
	fmt.Fprintf(w, "\nBaseline: %s\n  account %s  region %s  captured %s\n",
		b.Label, b.Account, b.Region, b.CapturedAt.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(w, "  %d functions, %d roles, %d APIs, %d log groups, %d alarms\n\n",
		len(b.Functions), len(b.Roles), len(b.APIs), len(b.LogGroups), len(b.Alarms))

	if len(findings) == 0 {
		fmt.Fprintln(w, "  Nothing flagged.")
		return
	}
	for _, f := range findings {
		fmt.Fprintf(w, "  [%-7s] %s\n             %s\n             %s\n\n",
			f.Severity, f.Resource, f.What, wrap(f.Why, 72, "             "))
	}
	var bl, wn int
	for _, f := range findings {
		switch f.Severity {
		case Blocker:
			bl++
		case Warn:
			wn++
		}
	}
	fmt.Fprintf(w, "  %d blockers, %d warnings, %d informational\n\n", bl, wn, len(findings)-bl-wn)
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, w := range words {
		if len(cur)+len(w)+1 > width && cur != "" {
			lines = append(lines, cur)
			cur = w
		} else if cur == "" {
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n"+indent)
}
