// Package baseline reads a running AWS account and turns it into something you
// can diff, review, and regenerate from.
//
// # WHY THIS EXISTS
//
// Their Phase 1 deliverable is an "approved implementation baseline". The whole
// risk of that phase is that people take the baseline from the TEMPLATES rather
// than from the RUNNING ACCOUNT. On an application that has been live for years,
// somebody changed something in the console during an incident and never put it
// back in the template -- and that difference stays invisible until the copy
// behaves differently in production.
//
// # WHAT THIS IS NOT
//
// It is not a "clone the account" button, and anyone who promises you one is
// selling something. Three things it deliberately will not do:
//
//  1. It never reads a secret VALUE. Environment variables are captured as KEYS
//     with the values replaced, and anything whose key looks like a credential
//     is reported so a human decides how it moves. A tool that copies production
//     secrets between accounts is a liability, not a feature.
//  2. It does not emit a template you apply blindly. It emits a STARTING point
//     that a person reads and edits. Generated infrastructure-as-code encodes
//     whatever accidents the source account had; the point of migrating is to
//     stop carrying those.
//  3. It does not move the code artifact. In a real migration the artifact is
//     rebuilt from source and promoted through the pipeline, so the thing that
//     ships is the thing that was tested.
//
// # THE PART THAT ACTUALLY EARNS ITS KEEP
//
// `compare`. Capturing one account is mildly useful; capturing BOTH and printing
// the differences is the evidence the migration's parallel-run phase is asking
// for. "The production account matches the baseline" stops being an assurance
// and becomes a diff somebody can read.
//
// AWS does ship something adjacent -- CloudFormation's IaC generator will scan an
// account and write templates from existing resources. It is worth using. It
// also will not tell you what differs between two accounts, will not flag which
// resources are owned by no stack, and will happily carry a secret value into a
// template. This fills those three gaps.
package baseline

import "time"

type Baseline struct {
	CapturedAt time.Time  `json:"captured_at"`
	Account    string     `json:"account"`
	Region     string     `json:"region"`
	Label      string     `json:"label"`
	Functions  []Function `json:"functions"`
	Roles      []Role     `json:"roles"`
	APIs       []API      `json:"apis"`
	LogGroups  []LogGroup `json:"log_groups"`
	Alarms     []Alarm    `json:"alarms"`
	Notes      []string   `json:"notes"`

	// ── The decommission half ────────────────────────────────────────────────
	// Populated only by `capture -estate`. Absent on a migration capture, which
	// is why `teardown` refuses to run on one rather than reporting an account
	// with no buckets, no tables and no VPCs.
	Estate     bool        `json:"estate"`
	Buckets    []Bucket    `json:"buckets,omitempty"`
	Tables     []Table     `json:"tables,omitempty"`
	Parameters []Parameter `json:"parameters,omitempty"`
	Networks   []Network   `json:"networks,omitempty"`
	Simples    []Simple    `json:"simple,omitempty"`

	// ⛔ WHAT THE NAME FILTER THREW AWAY, per resource class.
	//
	// `--prefix releasebot` is the obvious thing to type for a project called
	// ReleaseBot, and it silently excluded FIVE of six functions, because they
	// are named cutRelease, cutReleaseTest, releaseAutomationMergeback,
	// releaseAutomationMergebackTest and slackApprove. The report then read
	// "1 functions, 1 roles, 1 APIs · 2 blockers, 7 warnings" -- a confident,
	// complete-looking assessment of one sixth of the system.
	//
	// A capture that narrows its own scope and does not say so is the same
	// failure as a teardown that reports an empty account: the output looks
	// identical whether the account holds one function or six. Recorded so the
	// review can say it out loud.
	Prefix      string         `json:"prefix,omitempty"`
	FilteredOut map[string]int `json:"filtered_out,omitempty"`
}

type Function struct {
	Name          string   `json:"name"`
	Runtime       string   `json:"runtime"`
	Handler       string   `json:"handler"`
	Architectures []string `json:"architectures"`
	MemorySize    int      `json:"memory_size"`
	Timeout       int      `json:"timeout"`
	RoleArn       string   `json:"role_arn"`
	Layers        []string `json:"layers"`
	ReservedConc  *int     `json:"reserved_concurrency"`
	CodeSha256    string   `json:"code_sha256"`
	CodeSize      int64    `json:"code_size"`
	LastModified  string   `json:"last_modified"`
	// EnvKeys holds KEY NAMES ONLY. Values are never read into this struct.
	EnvKeys []string `json:"env_keys"`
	// EnvNonSecret holds values for keys judged non-sensitive, because an
	// environment name or a URL is part of the baseline you actually want to
	// diff. The classifier is in secrets.go and errs towards redaction.
	EnvNonSecret map[string]string `json:"env_non_secret"`
	// A secret-looking name holding a LITERAL value — the finding.
	SecretLooking []string `json:"secret_looking_keys"`
	// A secret-looking name holding an ssm:/secretsmanager: POINTER — the fix.
	// Recorded as a shape, never as a value.
	SecretByReference []string          `json:"secret_by_reference_keys,omitempty"`
	Aliases           []Alias           `json:"aliases"`
	Versions          []string          `json:"versions"`
	Tags              map[string]string `json:"tags"`
	DLQ               string            `json:"dlq_arn,omitempty"`
	// StackOwned is false when no CloudFormation stack claims this function --
	// i.e. somebody created it by hand. That is the single most useful field in
	// the whole capture.
	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
	// ResourcePolicy is who may invoke it. A migration that recreates the
	// function but forgets this produces a function nothing can call.
	ResourcePolicy string `json:"resource_policy,omitempty"`
}

type Alias struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// RoutingConfig non-empty means traffic is split between two versions --
	// a canary somebody may have left running.
	RoutingConfig string `json:"routing_config,omitempty"`
}

type Role struct {
	Name            string   `json:"name"`
	Arn             string   `json:"arn"`
	TrustPolicy     string   `json:"trust_policy"`
	ManagedPolicies []string `json:"managed_policies"`
	// InlinePolicies are where console edits hide. A role with inline policies
	// that no template mentions is drift, every time.
	InlinePolicies map[string]string `json:"inline_policies"`
	StackOwned     bool              `json:"stack_owned"`
	OwningStack    string            `json:"owning_stack,omitempty"`
}

type API struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`          // REST or HTTP
	Endpoint string  `json:"endpoint_type"` // EDGE | REGIONAL | PRIVATE
	Routes   []Route `json:"routes"`
	Stages   []Stage `json:"stages"`

	// ⚠️ ENDPOINT TYPE IS NOT REACHABILITY, and conflating the two is the
	// mistake this field exists to prevent. A REGIONAL api has public DNS; a
	// resource policy can still refuse every source but CloudFront, a corporate
	// CIDR, or a webhook provider's published ranges. "REGIONAL" alone tells
	// you nothing about who can actually invoke it.
	//
	// Captured raw, because the policy is the authority. Without it the capture
	// reports endpoint_type and leaves the only question that matters -- is
	// this reachable from the internet -- to be guessed at.
	ResourcePolicy string `json:"resource_policy,omitempty"`
	// CIDRs the policy names, extracted for the review step. Empty with a
	// non-empty ResourcePolicy means the policy restricts by something other
	// than source IP (a VPC endpoint, an account, a service principal).
	PolicyCidrs []string `json:"policy_cidrs,omitempty"`

	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
}

type Route struct {
	Path            string `json:"path"`
	Method          string `json:"method"`
	Authorization   string `json:"authorization"`
	IntegrationType string `json:"integration_type"`
	IntegrationURI  string `json:"integration_uri"`
}

type Stage struct {
	Name           string            `json:"name"`
	DeploymentID   string            `json:"deployment_id"`
	Variables      map[string]string `json:"variables"`
	TracingEnabled bool              `json:"tracing_enabled"`
	AccessLogDest  string            `json:"access_log_destination,omitempty"`
	LoggingLevel   string            `json:"logging_level,omitempty"`
	DataTrace      bool              `json:"data_trace_enabled"`
	ThrottleRate   float64           `json:"throttle_rate"`
	ThrottleBurst  int               `json:"throttle_burst"`
	// ⚠️ PER STAGE, not per API. A WAF web ACL is associated with a STAGE, so
	// "the API is behind WAF" is not a statement that can be true -- prod can
	// be protected while test is wide open, which is usually what you want and
	// always what you need to know.
	WebACLArn string `json:"web_acl_arn,omitempty"`
}

type LogGroup struct {
	Name          string `json:"name"`
	RetentionDays int    `json:"retention_days"` // 0 means NEVER EXPIRE
	StoredBytes   int64  `json:"stored_bytes"`
	// Implicit means Lambda created it, not a template. Implicit groups never
	// expire and are owned by no stack, so they survive the decommission and
	// keep billing.
	Implicit bool `json:"implicit"`
}

type Alarm struct {
	Name           string   `json:"name"`
	Namespace      string   `json:"namespace"`
	MetricName     string   `json:"metric_name"`
	Threshold      float64  `json:"threshold"`
	Comparison     string   `json:"comparison"`
	TreatMissing   string   `json:"treat_missing_data"`
	Actions        []string `json:"alarm_actions"`
	ActionsEnabled bool     `json:"actions_enabled"`
	StackOwned     bool     `json:"stack_owned"`
}

// ── The decommission half of the capture ─────────────────────────────────────
//
// ⛔ WHY THESE EXIST SEPARATELY FROM EVERYTHING ABOVE.
//
// The inventory you take to MIGRATE a system is not the inventory you need to
// RETIRE one. The sections above answer "what do I have to rebuild?" -- which is
// Lambda, IAM, API Gateway and the observability around them. A decommission
// asks a different question: "what will still be here, and still billing, after
// I delete the stacks?" That question reaches eleven more services, and the
// expensive answers are all in resources nobody thinks of as part of the app --
// a NAT gateway inside a VPC the stack happened to create, a user pool, a
// distribution, a queue.
//
// ⚠️ AND SOME TEARDOWN TRAPS ARE NOT RESOURCES AT ALL, so they are deliberately
// NOT modelled here. Object versions live INSIDE a bucket; a detached Lambda ENI
// does not exist until AFTER its function is deleted; dependency order is a
// sequence rather than a list. Those belong to the executor, not the inventory
// -- see teardown.go. Pretending a scan can find them is how you get a teardown
// that reports success and leaves the expensive half running.

// Bucket is an S3 bucket, with the one property that decides whether a teardown
// succeeds or fails on it.
type Bucket struct {
	Name string `json:"name"`
	// ⛔ THE FIELD THIS TYPE EXISTS FOR. `aws s3 rm --recursive` removes CURRENT
	// objects only; a versioned bucket keeps every old version plus a delete
	// marker per removed object, and a bucket holding either is NOT empty. So
	// `s3 ls` shows nothing and the stack delete still fails with a 409. And
	// CloudFormation never empties a bucket for you -- emptying must happen
	// BEFORE the stack delete, not after.
	Versioning  string `json:"versioning,omitempty"` // Enabled | Suspended | ""
	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
}

// Table is a DynamoDB table. Two separate things can stop it being deleted, and
// they fail at different moments.
type Table struct {
	Name string `json:"name"`
	// DeletionProtection refuses the delete outright, with a clear error.
	DeletionProtection bool `json:"deletion_protection"`
	// ⚠️ Retain is the quiet one: a correct `DeletionPolicy: Retain` on an audit
	// table means NO stack delete ever removes it, so the teardown reports
	// success and the table stays. It then blocks the next CREATE of that stack
	// with AWS::EarlyValidation::ResourceExistenceCheck and no resource-level
	// event naming it -- every failed attempt recreating the condition that
	// fails the next one.
	Retain      bool   `json:"deletion_policy_retain"`
	ItemCount   int64  `json:"item_count"`
	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
}

// Simple is any resource whose teardown question is only "does a stack own it?"
// -- queues, topics, distributions, user pools, web ACLs, canaries. Modelling
// each in its own struct would add types without adding answers.
type Simple struct {
	Class       string `json:"class"` // sqs | sns | cloudfront | cognito | wafv2 | synthetics
	Name        string `json:"name"`
	ID          string `json:"id,omitempty"`
	Detail      string `json:"detail,omitempty"`
	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
}

// Parameter is an SSM parameter -- NAME AND TYPE ONLY, never a value.
//
// ⛔ These are the configuration nothing owns. bootstrap.sh writes them with
// put-parameter deliberately, so a published Lambda version freezes a POINTER to
// its config rather than the values. The consequence at teardown: no stack owns
// them, so no stack delete removes them, and sixteen of them survived every
// delete in the sandbox.
type Parameter struct {
	Name string `json:"name"`
	Type string `json:"type"` // String | StringList | SecureString
}

// Network is the VPC estate. The NAT gateway is the line item that matters:
// it is the most expensive thing a forgotten stack can leave behind.
type Network struct {
	VpcID       string `json:"vpc_id"`
	CidrBlock   string `json:"cidr_block,omitempty"`
	IsDefault   bool   `json:"is_default"`
	Subnets     int    `json:"subnets"`
	NatGateways int    `json:"nat_gateways"`
	Endpoints   int    `json:"vpc_endpoints"`
	// ⚠️ A COUNT TAKEN NOW, which is not the count that will block the delete.
	// A Lambda's hyperplane ENI is `in-use` while its function exists and only
	// becomes `available` AFTER the function is gone -- so a scan before the
	// teardown sees every one attached and skips them all. Recorded for context,
	// never as the basis for a decision. The reaping happens mid-teardown.
	ENIsNow     int    `json:"enis_at_capture"`
	StackOwned  bool   `json:"stack_owned"`
	OwningStack string `json:"owning_stack,omitempty"`
}
