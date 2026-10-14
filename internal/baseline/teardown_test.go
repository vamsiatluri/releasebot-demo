package baseline

import (
	"errors"
	"strings"
	"testing"
)

// ⛔ The two refusals are the most important behaviour in this file, so they are
// tested FIRST and by the state they exist for. A guard that has never been seen
// to fail has not been tested -- the lesson from a prover that enumerated refusal
// phrases and passed a refusal it had not listed.

func TestRefusesAMigrationCapture(t *testing.T) {
	_, err := BuildTeardown(&Baseline{Account: "1", Estate: false})
	if !errors.Is(err, ErrNotEstate) {
		t.Fatalf("a capture with no estate section must be refused, got %v", err)
	}
}

func TestRefusesAFilteredCapture(t *testing.T) {
	// The exact shape that deleted five of six functions from an assessment:
	// a capture that looks complete and is a view of one sixth of the account.
	_, err := BuildTeardown(&Baseline{Account: "1", Estate: true, Prefix: "releasebot"})
	if !errors.Is(err, ErrFiltered) {
		t.Fatalf("a -prefix capture must be refused, got %v", err)
	}
}

func TestAcceptsAFullEstateCapture(t *testing.T) {
	if _, err := BuildTeardown(&Baseline{Account: "1", Estate: true}); err != nil {
		t.Fatalf("a full estate capture must be planned, got %v", err)
	}
}

func fixture() *Baseline {
	return &Baseline{
		Account: "450315222669", Region: "us-east-1", Label: "msnbc-dev", Estate: true,
		Functions: []Function{
			{Name: "cutRelease", StackOwned: true, OwningStack: "releasebot-lambda"},
			{Name: "handMade", StackOwned: false},
		},
		APIs:      []API{{Name: "releasebot", StackOwned: true, OwningStack: "releasebot-edge"}},
		LogGroups: []LogGroup{{Name: "/aws/apigateway/welcome", Implicit: true}},
		Buckets: []Bucket{
			{Name: "rb-site", Versioning: "Enabled", StackOwned: true, OwningStack: "releasebot-edge"},
			{Name: "rb-plain"},
		},
		Tables: []Table{
			{Name: "releasebot-ledger", StackOwned: true, OwningStack: "releasebot-lambda"},
			{Name: "locked", DeletionProtection: true},
		},
		Parameters: []Parameter{{Name: "/releasebot/prod/secrets/github-token", Type: "SecureString"}},
		Networks: []Network{
			{VpcID: "vpc-abc", NatGateways: 1, StackOwned: true, OwningStack: "releasebot-network"},
			{VpcID: "vpc-default", IsDefault: true},
		},
		Simples: []Simple{{Class: "cloudfront", Name: "rb dashboard", ID: "E123", Detail: "enabled=true status=Deployed"}},
	}
}

func TestTheUnownedListIsWhatNoStackDeleteWillTouch(t *testing.T) {
	p, err := BuildTeardown(fixture())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(p.Unowned, " ")
	for _, want := range []string{
		"lambda/handMade",                           // made by hand
		"s3/rb-plain",                               // bucket no stack owns
		"dynamodb/locked",                           // table no stack owns
		"cloudfront/rb dashboard",                   // distribution no stack owns
		"logs//aws/apigateway/welcome",              // AWS made it, nothing owns it
		"ssm//releasebot/prod/secrets/github-token", // written by a script, no stack owns it
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unowned list must name %q\ngot: %s", want, got)
		}
	}
	// ⛔ The default VPC is NOT a teardown target, ever.
	if strings.Contains(got, "vpc-default") {
		t.Error("the default VPC must never appear in a teardown list")
	}
	// A stack owns these, so a stack delete handles them -- listing them here
	// would send someone deleting by hand what CloudFormation already removes.
	for _, no := range []string{"lambda/cutRelease", "s3/rb-site", "vpc/vpc-abc"} {
		if strings.Contains(got, no) {
			t.Errorf("stack-owned %q must not be in the unowned list", no)
		}
	}
}

func TestTheOrderIsTheWholePoint(t *testing.T) {
	p, _ := BuildTeardown(fixture())
	at := map[string]int{}
	for _, s := range p.Steps {
		at[s.Title] = s.N
	}
	buckets, stacks, enis := 0, 0, 0
	for _, s := range p.Steps {
		switch {
		case strings.Contains(s.Title, "Empty the buckets"):
			buckets = s.N
		case strings.Contains(s.Title, "Delete the stacks"):
			stacks = s.N
		case strings.Contains(s.Title, "Reap"):
			enis = s.N
		}
	}
	if !(buckets < stacks) {
		t.Errorf("buckets must be emptied BEFORE the stacks (CFN never empties one): %d vs %d", buckets, stacks)
	}
	if !(stacks < enis) {
		t.Errorf("ENIs must be reaped AFTER the stacks — one is only `available` once its function is gone: %d vs %d", stacks, enis)
	}
}

func TestTheTrapsAreCarriedIntoTheRightStep(t *testing.T) {
	p, _ := BuildTeardown(fixture())
	find := func(sub string) Step {
		for _, s := range p.Steps {
			if strings.Contains(s.Title, sub) {
				return s
			}
		}
		t.Fatalf("no step matching %q", sub)
		return Step{}
	}
	if w := strings.Join(find("Empty the buckets").Warn, " "); !strings.Contains(w, "rb-site") {
		t.Error("the versioned bucket must be named in the bucket step's warning")
	}
	if w := strings.Join(find("Reap").Warn, " "); !strings.Contains(w, "NAT") {
		t.Error("a surviving NAT gateway must be flagged — it is the expensive leftover")
	}
	v := strings.Join(find("Verify").Warn, " ")
	if !strings.Contains(v, "locked") {
		t.Error("deletion protection must be named: it refuses the delete outright")
	}
	if !strings.Contains(v, "releasebot-ledger") {
		t.Error("a stack-owned table must be flagged for a DeletionPolicy: Retain check")
	}
}

// ⛔ THIS TEST EXISTS BECAUSE THE FIRST ORDERING WAS WRONG AND EVERY OTHER TEST
// PASSED. Reverse-alphabetical put `releasebot-lambda` before `releasebot-edge`,
// which is backwards -- the API's integrations point AT the functions, so the
// edge stack has to go first. Nothing caught it until a real plan was read
// against a real account. Names are deliberately chosen so alphabetical order
// and correct order DISAGREE.
func TestEdgeStacksAreDeletedBeforeTheThingsTheyPointAt(t *testing.T) {
	b := &Baseline{
		Estate:    true,
		APIs:      []API{{Name: "api", StackOwned: true, OwningStack: "releasebot-edge"}},
		Functions: []Function{{Name: "cut", StackOwned: true, OwningStack: "releasebot-lambda"}},
		Roles:     []Role{{Name: "r", StackOwned: true, OwningStack: "releasebot-iam"}},
		Networks:  []Network{{VpcID: "vpc-1", StackOwned: true, OwningStack: "releasebot-network"}},
		Simples: []Simple{{Class: "cloudfront", Name: "d", StackOwned: true,
			OwningStack: "releasebot-dashboard"}},
	}
	p, err := BuildTeardown(b)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, s := range p.Stacks {
		pos[s] = i
	}
	for _, c := range []struct{ first, then string }{
		{"releasebot-edge", "releasebot-lambda"},      // integrations point at the functions
		{"releasebot-dashboard", "releasebot-lambda"}, // a distribution fronts them
		{"releasebot-lambda", "releasebot-iam"},       // functions assume the roles
		{"releasebot-lambda", "releasebot-network"},   // functions sit in the VPC
	} {
		if pos[c.first] >= pos[c.then] {
			t.Errorf("%s must be deleted before %s (got %d, %d)\norder: %v",
				c.first, c.then, pos[c.first], pos[c.then], p.Stacks)
		}
	}
	// And the exact failure the old sort produced, named so a regression is obvious.
	if pos["releasebot-lambda"] < pos["releasebot-edge"] {
		t.Error("REGRESSION: reverse-alphabetical ordering is back")
	}
}

// ⛔ THE SECOND ORDERING BUG: ranking a stack by its DEEPEST member sank any
// stack that owned an IAM role to the bottom. releasebot-slack-approve owns a
// FUNCTION and the role that function assumes -- and ended up scheduled after
// the VPC its function sits in. Owning your own role does not make you a
// foundation.
func TestAStackThatOwnsItsOwnRoleIsStillAConsumer(t *testing.T) {
	b := &Baseline{
		Estate: true,
		// Self-contained: its own function AND its own role, like the real
		// releasebot-slack-approve stack.
		Functions: []Function{{Name: "approve", StackOwned: true, OwningStack: "releasebot-slack-approve"}},
		Roles: []Role{
			{Name: "approveRole", StackOwned: true, OwningStack: "releasebot-slack-approve"},
			{Name: "shared", StackOwned: true, OwningStack: "releasebot-iam"},
		},
		Networks: []Network{{VpcID: "vpc-1", StackOwned: true, OwningStack: "releasebot-network"}},
	}
	p, err := BuildTeardown(b)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, st := range p.Stacks {
		pos[st] = i
	}
	if pos["releasebot-slack-approve"] >= pos["releasebot-iam"] {
		t.Errorf("a stack owning a function is a consumer even when it also owns a role\norder: %v", p.Stacks)
	}
	if pos["releasebot-slack-approve"] >= pos["releasebot-network"] {
		t.Errorf("REGRESSION: a function's stack is scheduled after the VPC it sits in\norder: %v", p.Stacks)
	}
	// The pure foundation really is last.
	if p.Stacks[len(p.Stacks)-1] != "releasebot-network" {
		t.Errorf("the VPC stack is the floor and must be last, got %v", p.Stacks)
	}
}
