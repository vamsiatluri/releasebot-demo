package baseline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Teardown turns a captured account into an ORDERED decommission plan.
//
// # WHY THIS IS GENERATED AND NOT WRITTEN BY HAND
//
// The first version of scripts/sandbox-down.sh enumerated a hand-written list of
// stacks and resource classes. The list predated three stacks, so it would have
// left a CloudFront distribution, a Cognito user pool, a VPC with a NAT gateway,
// two interface endpoints, a DynamoDB table and an SQS queue running -- and then
// printed `TEARDOWN VERIFIED: the account is empty`, because it verified only the
// classes it already knew about.
//
// ★ A verification that enumerates a hand-written list proves the LIST, not the
// ACCOUNT -- and it fails in the most expensive direction, since what it misses
// is exactly what keeps billing. A plan derived from a capture taken on the day
// cannot go stale that way.
//
// # WHAT IT DELIBERATELY DOES NOT DO
//
// It does not delete anything, and it does not emit a script you are meant to
// run unread. The output is a plan for a human, because the single irreversible
// step in a migration is this one: it is the moment the rollback stops existing.
//
// # THE TRAPS IT CARRIES THAT NO CAPTURE COULD FIND
//
// Three of the four things that actually broke the sandbox teardown are not
// resources and cannot be scanned for -- object versions live INSIDE a bucket, a
// detached Lambda ENI does not exist until AFTER its function is deleted, and
// dependency order is a sequence. They are encoded here as ORDERING and as
// warnings attached to the step that hits them.

type Step struct {
	N       int
	Title   string
	Why     string   // why it is HERE and not somewhere else in the order
	Actions []string // concrete, in order
	Warn    []string
}

type Plan struct {
	Account string
	Region  string
	Label   string
	Steps   []Step
	// Unowned is every resource no CloudFormation stack claims -- the ones a
	// stack delete will not touch, which is the whole reason this tool exists.
	Unowned []string
	Stacks  []string
}

// ErrNotEstate is returned for a capture taken without -estate.
var ErrNotEstate = errors.New(
	"this capture has no estate section, so it cannot see buckets, tables, " +
		"parameters, VPCs, queues, topics, distributions or user pools.\n" +
		"  Re-run:  baseline capture -estate ...\n" +
		"  ⛔ Planning a teardown from a migration capture would produce a plan " +
		"that looks complete and silently omits everything that keeps billing.")

// ErrFiltered is returned for a capture narrowed by -prefix.
//
// ⛔ THIS REFUSAL IS THE POINT, not an inconvenience. `--prefix releasebot` once
// excluded five of six functions because they are named cutRelease,
// releaseAutomationMergeback and so on -- and the review then read like a
// confident assessment of the whole system. On an ASSESSMENT that produces a
// wrong number. On a TEARDOWN it deletes what matched and leaves everything else
// running, with a report that says the account is clean.
var ErrFiltered = errors.New(
	"this capture was narrowed with -prefix, so it is a view of part of the " +
		"account and not an inventory of it.\n" +
		"  A teardown built from a filtered capture removes what matched and " +
		"leaves the rest billing, while reporting success.\n" +
		"  Re-run the capture with no -prefix.")

func BuildTeardown(b *Baseline) (*Plan, error) {
	if !b.Estate {
		return nil, ErrNotEstate
	}
	if b.Prefix != "" {
		return nil, ErrFiltered
	}

	p := &Plan{Account: b.Account, Region: b.Region, Label: b.Label}

	p.Stacks = orderStacks(b)

	p.Unowned = unownedResources(b)

	versioned, allBuckets := bucketLists(b)
	protected, stackTables := tableLists(b)
	dists := simplesOf(b, "cloudfront")
	natVPCs := natVPCs(b)

	p.Steps = []Step{
		{
			N:     1,
			Title: "Make it inert, not gone",
			Why: "This is the only irreversible phase in the migration -- it is the " +
				"moment the rollback stops existing. Repointing the triggers makes the " +
				"old account unreachable while leaving it fully intact, which IS the " +
				"rollback. Nothing here deletes anything.",
			Actions: []string{
				"Repoint the Slack slash commands (/cut, /merge) at the new account",
				"Repoint the Slack interactivity URL -- separately, and it is the one people forget",
				"Repoint the Jira webhook",
				"Soak. Agree the length in writing; this window is the rollback.",
			},
			Warn: []string{
				"A slash command works while buttons silently do nothing when the " +
					"interactivity URL is still pointed at the old account.",
			},
		},
		{
			N:     2,
			Title: "Revoke the credentials -- starting OUTSIDE AWS",
			Why: "The GitHub token is not an AWS resource, so nothing you delete in " +
				"AWS touches it. If the new account was given a copy of the old token, " +
				"that is a credential with production reach living in an account nobody " +
				"watches any more.",
			Actions: []string{
				"Rotate the GitHub token at GitHub -- deleting an AWS role does not revoke it",
				"Rotate the Slack signing secret, and PROVE an old-secret signature is now rejected",
				"Rotate the Jira webhook shared secret",
			},
			Warn: []string{
				"Rotation that leaves the old secret valid is not rotation. Verify the refusal.",
			},
		},
		bucketStep(versioned, allBuckets),
		cloudFrontStep(dists),
		{
			N:     5,
			Title: "Delete the stacks, deepest dependency first",
			Why: "Everything a stack owns goes with the stack. The order is ranked by " +
				"what each stack OWNS -- APIs and distributions first, then compute, then " +
				"the IAM roles and VPCs everything else sits on. It is a starting order, " +
				"not a dependency graph: read it before you run it.",
			Actions: stackActions(p.Stacks),
			Warn: []string{
				"EXPECT one or more network deletes to fail here. That is step 6, not a problem.",
				"A failed create leaves ROLLBACK_COMPLETE, which can be neither updated " +
					"nor created -- it must be deleted before any retry.",
			},
		},
		eniStep(natVPCs),
		{
			N:     7,
			Title: "Sweep what NO STACK OWNS -- where the money hides",
			Why: "A stack delete removes what a stack owns. Everything below was " +
				"created by hand, by a bootstrap script, or by AWS itself, so no stack " +
				"delete will ever touch it. This list came from the capture, not from " +
				"anyone's memory.",
			Actions: sweepActions(b),
			Warn: []string{
				"/aws/apigateway/welcome is created by AWS the first time an account " +
					"enables API Gateway logging. It is owned by nothing, never expires, " +
					"and survives every delete.",
			},
		},
		verifyStep(b, protected, stackTables),
	}
	return p, nil
}

func bucketStep(versioned, all []string) Step {
	s := Step{
		N:     3,
		Title: "Empty the buckets -- BEFORE the stacks, not after",
		Why: "CloudFormation never empties a bucket for you; it fails the stack " +
			"delete with 'The bucket you tried to delete is not empty (409)'. So " +
			"emptying has to happen before the delete, which puts it here.",
	}
	if len(all) == 0 {
		s.Actions = []string{"(no buckets captured in this region)"}
		return s
	}
	for _, b := range all {
		s.Actions = append(s.Actions, "scripts/_empty-bucket.py "+b)
	}
	if len(versioned) > 0 {
		s.Warn = append(s.Warn, fmt.Sprintf(
			"⛔ VERSIONED, so `aws s3 rm --recursive` CANNOT empty them: %s. "+
				"It removes current objects only, leaving every old version plus a "+
				"delete marker per removed object -- so `s3 ls` shows an empty bucket "+
				"and the stack delete still fails. Page list-object-versions and delete "+
				"BOTH Versions and DeleteMarkers.", strings.Join(versioned, ", ")))
	}
	return s
}

func cloudFrontStep(dists []Simple) Step {
	s := Step{
		N:     4,
		Title: "Disable the CloudFront distributions",
		Why: "A distribution must be DISABLED and fully propagated before it can be " +
			"deleted, and propagation takes 15-20 minutes. That is a scheduling fact, " +
			"so it starts here rather than being discovered at the end of the window.",
	}
	if len(dists) == 0 {
		s.Actions = []string{"(no distributions captured)"}
		return s
	}
	for _, d := range dists {
		s.Actions = append(s.Actions, fmt.Sprintf(
			"disable %s (%s) and WAIT for Deployed  [%s]", d.ID, d.Name, d.Detail))
	}
	return s
}

func eniStep(natVPCs []string) Step {
	s := Step{
		N:     6,
		Title: "Reap the detached Lambda ENIs, then RETRY what failed",
		Why: "A Lambda's hyperplane ENI is in-use while its function exists and only " +
			"becomes `available` AFTER the function is gone. So reaping before the " +
			"stacks finds every one attached and skips them all -- this step cannot " +
			"move earlier in the order.",
		Actions: []string{
			"aws ec2 describe-network-interfaces --filters Name=status,Values=available \\",
			"  --query \"NetworkInterfaces[?starts_with(Description,'AWS Lambda VPC ENI')].NetworkInterfaceId\"",
			"delete each one, then RE-RUN the stack deletes that failed in step 5",
		},
		Warn: []string{
			"⛔ ONLY status=available. An in-use ENI means something really is attached.",
			"No error message mentions an ENI. It says 'the subnet has dependencies and " +
				"cannot be deleted', and CloudFormation spins on it ~40 minutes before giving up.",
			"Lambda reaps these itself WHILE the function exists. Once the function is " +
				"deleted nothing is left to do the reaping, so waiting never resolves it.",
		},
	}
	if len(natVPCs) > 0 {
		s.Warn = append(s.Warn, "NAT gateway(s) still present in: "+
			strings.Join(natVPCs, ", ")+" -- the most expensive thing a half-finished "+
			"teardown leaves behind. Confirm they are gone, by ID.")
	}
	return s
}

func verifyStep(b *Baseline, protectedTables, stackTables []string) Step {
	s := Step{
		N:     8,
		Title: "Verify by RE-QUERYING, not by reading the script's own report",
		Why: "Decommissioning is a verification job, not a delete job. A console " +
			"showing a deletion time is not proof: only DELETE_COMPLETE, or " +
			"describe-stacks actually failing, proves a stack is gone.",
		Actions: []string{
			"aws cloudformation list-stacks  (INCLUDE DELETE_FAILED and DELETE_IN_PROGRESS)",
			"aws lambda list-functions",
			"aws ec2 describe-vpcs  (non-default)",
			"aws ec2 describe-network-interfaces",
			"aws s3api list-buckets",
			"aws dynamodb list-tables",
			"aws ssm describe-parameters",
			"aws logs describe-log-groups",
			"aws cloudfront list-distributions · aws cognito-idp list-user-pools",
			"Re-run: baseline capture -estate  →  the inventory should be empty",
		},
	}
	if len(protectedTables) > 0 {
		s.Warn = append(s.Warn, "DELETION PROTECTION is on: "+
			strings.Join(protectedTables, ", ")+" -- the delete is refused outright "+
			"until it is turned off. That is the loud failure.")
	}
	if len(stackTables) > 0 {
		s.Warn = append(s.Warn, "⚠️ QUIET ONE: these tables are stack-owned -- "+
			strings.Join(stackTables, ", ")+". If the template carries "+
			"DeletionPolicy: Retain (correct for an audit record) then NO stack delete "+
			"removes them, the teardown reports success, and the orphan then blocks the "+
			"NEXT create with AWS::EarlyValidation::ResourceExistenceCheck and no "+
			"resource-level event naming it. Read the template; the API cannot tell you.")
	}
	return s
}

// ── helpers ──────────────────────────────────────────────────────────────────

// orderStacks puts the stacks in the order they should be DELETED: consumers
// before the foundations they consume.
//
// ⛔ TWO BUGS LIVED HERE, AND BOTH PASSED EVERY OTHER TEST.
//
// The first sorted the names in REVERSE and claimed the numeric prefixes in
// infra/ encoded the order. The TEMPLATE FILES carry those prefixes
// (10-iam, 20-lambda, 30-apigw); the STACK NAMES do not. Reverse-alphabetical
// therefore put `releasebot-lambda` before `releasebot-edge`, which is backwards
// -- the API's integrations point AT the functions.
//
// The second ranked a stack by its DEEPEST member, so any stack owning an IAM
// role sank to the bottom. `releasebot-slack-approve` owns a function AND the
// role that function assumes, and landed last -- after the VPC its function sits
// in. Owning your own role does not make you a foundation.
//
// ★ So rank by the SHALLOWEST thing a stack owns. A stack holding anything that
// CONSUMES -- an API, a distribution, a function -- is a consumer, whatever else
// it also holds. Only a stack owning nothing but roles or networks is a
// foundation, and those go last:
//
//	0  leaf        APIs, distributions, web ACLs, alarms — nothing depends on these
//	1  compute     functions, tables, queues, buckets
//	2  roles       IAM only — every function above assumes one
//	3  network     VPCs only — the floor
//
// ⚠️ STILL A STARTING ORDER, NOT A DEPENDENCY GRAPH. CloudFormation knows the
// real graph through exports and imports; this reads ownership. Read it before
// running it.
func orderStacks(b *Baseline) []string {
	const (
		leaf = iota
		compute
		roles
		network
	)
	depth := map[string]int{}
	note := func(stack string, d int) {
		if stack == "" {
			return
		}
		// MINIMUM, deliberately — see the comment above.
		if cur, seen := depth[stack]; !seen || d < cur {
			depth[stack] = d
		}
	}
	for _, x := range b.APIs {
		if x.StackOwned {
			note(x.OwningStack, leaf)
		}
	}
	for _, x := range b.Alarms {
		if x.StackOwned {
			note("", leaf) // alarms carry no stack name in the capture
		}
	}
	for _, x := range b.Simples {
		if x.StackOwned {
			switch x.Class {
			case "cloudfront", "wafv2":
				note(x.OwningStack, leaf)
			default:
				note(x.OwningStack, compute)
			}
		}
	}
	for _, x := range b.Functions {
		if x.StackOwned {
			note(x.OwningStack, compute)
		}
	}
	for _, x := range b.Tables {
		if x.StackOwned {
			note(x.OwningStack, compute)
		}
	}
	for _, x := range b.Buckets {
		if x.StackOwned {
			note(x.OwningStack, compute)
		}
	}
	for _, x := range b.Roles {
		if x.StackOwned {
			note(x.OwningStack, roles)
		}
	}
	for _, x := range b.Networks {
		if x.StackOwned {
			note(x.OwningStack, network)
		}
	}
	// A stack that owns only log groups or alarms never got a depth; it is a
	// leaf by definition, so pick it up from the full ownership list.
	for _, st := range allStacks(b) {
		if _, seen := depth[st]; !seen {
			depth[st] = leaf
		}
	}
	out := make([]string, 0, len(depth))
	for st := range depth {
		out = append(out, st)
	}
	// Shallowest first; name as the tie-break so the plan is reproducible.
	sort.Slice(out, func(i, j int) bool {
		if depth[out[i]] != depth[out[j]] {
			return depth[out[i]] < depth[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func allStacks(b *Baseline) []string {
	var out []string
	add := func(owned bool, s string) {
		if owned && s != "" {
			out = append(out, s)
		}
	}
	for _, f := range b.Functions {
		add(f.StackOwned, f.OwningStack)
	}
	for _, r := range b.Roles {
		add(r.StackOwned, r.OwningStack)
	}
	for _, a := range b.APIs {
		add(a.StackOwned, a.OwningStack)
	}
	for _, x := range b.Buckets {
		add(x.StackOwned, x.OwningStack)
	}
	for _, x := range b.Tables {
		add(x.StackOwned, x.OwningStack)
	}
	for _, x := range b.Networks {
		add(x.StackOwned, x.OwningStack)
	}
	for _, x := range b.Simples {
		add(x.StackOwned, x.OwningStack)
	}
	return out
}

func unownedResources(b *Baseline) []string {
	var out []string
	for _, f := range b.Functions {
		if !f.StackOwned {
			out = append(out, "lambda/"+f.Name)
		}
	}
	for _, r := range b.Roles {
		if !r.StackOwned {
			out = append(out, "iam/"+r.Name)
		}
	}
	for _, a := range b.APIs {
		if !a.StackOwned {
			out = append(out, "apigateway/"+a.Name)
		}
	}
	for _, x := range b.Buckets {
		if !x.StackOwned {
			out = append(out, "s3/"+x.Name)
		}
	}
	for _, x := range b.Tables {
		if !x.StackOwned {
			out = append(out, "dynamodb/"+x.Name)
		}
	}
	for _, x := range b.Networks {
		if !x.StackOwned && !x.IsDefault {
			out = append(out, "vpc/"+x.VpcID)
		}
	}
	for _, x := range b.Simples {
		if !x.StackOwned {
			out = append(out, x.Class+"/"+x.Name)
		}
	}
	for _, g := range b.LogGroups {
		if g.Implicit {
			out = append(out, "logs/"+g.Name)
		}
	}
	for _, p := range b.Parameters {
		out = append(out, "ssm/"+p.Name)
	}
	sort.Strings(out)
	return out
}

func bucketLists(b *Baseline) (versioned, all []string) {
	for _, x := range b.Buckets {
		all = append(all, x.Name)
		if x.Versioning == "Enabled" || x.Versioning == "Suspended" {
			versioned = append(versioned, x.Name)
		}
	}
	return
}

func tableLists(b *Baseline) (protected, stackOwned []string) {
	for _, t := range b.Tables {
		if t.DeletionProtection {
			protected = append(protected, t.Name)
		}
		if t.StackOwned {
			stackOwned = append(stackOwned, t.Name)
		}
	}
	return
}

func simplesOf(b *Baseline, class string) []Simple {
	var out []Simple
	for _, s := range b.Simples {
		if s.Class == class {
			out = append(out, s)
		}
	}
	return out
}

func natVPCs(b *Baseline) []string {
	var out []string
	for _, n := range b.Networks {
		if n.NatGateways > 0 {
			out = append(out, fmt.Sprintf("%s (%d NAT)", n.VpcID, n.NatGateways))
		}
	}
	return out
}

func stackActions(stacks []string) []string {
	if len(stacks) == 0 {
		return []string{"(no stack-owned resources captured — everything here is hand-made)"}
	}
	var out []string
	for _, s := range stacks {
		out = append(out, "aws cloudformation delete-stack --stack-name "+s+
			"   then WAIT for DELETE_COMPLETE")
	}
	return out
}

func sweepActions(b *Baseline) []string {
	u := unownedResources(b)
	if len(u) == 0 {
		return []string{"(nothing unowned — unusually tidy; re-read the capture before believing it)"}
	}
	out := make([]string, 0, len(u)+1)
	for _, r := range u {
		out = append(out, "delete "+r)
	}
	out = append(out, "⚠️ ssm delete-parameters takes at most 10 names per call")
	return out
}

// Render writes the plan as the thing a person reads before a change window.
func (p *Plan) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Teardown plan — %s\n\n", p.Label)
	fmt.Fprintf(&b, "account %s · region %s\n\n", p.Account, p.Region)
	b.WriteString("⚠️ A PLAN, NOT A SCRIPT. This is the one irreversible phase of the\n")
	b.WriteString("migration — the moment the rollback stops existing. Read every step,\n")
	b.WriteString("confirm no release is in flight, and have a named rollback owner awake.\n\n")

	if len(p.Unowned) > 0 {
		fmt.Fprintf(&b, "## %d resources NO STACK OWNS\n\n", len(p.Unowned))
		b.WriteString("Nothing deletes these for you. This is the list that keeps billing.\n\n")
		for _, r := range p.Unowned {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
		b.WriteString("\n")
	}

	for _, s := range p.Steps {
		fmt.Fprintf(&b, "## %d. %s\n\n%s\n\n", s.N, s.Title, wrap(s.Why, 76, ""))
		for _, a := range s.Actions {
			fmt.Fprintf(&b, "    %s\n", a)
		}
		if len(s.Warn) > 0 {
			b.WriteString("\n")
			for _, w := range s.Warn {
				// The renderer owns the marker. A warning that starts with its own
				// prints "⚠️ ⛔" and reads like a typo.
				w = strings.TrimSpace(strings.TrimLeft(w, "⚠️⛔ "))
				fmt.Fprintf(&b, "  ⚠️  %s\n", wrap(w, 74, "      "))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
