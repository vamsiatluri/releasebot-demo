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
	EnvNonSecret  map[string]string `json:"env_non_secret"`
	SecretLooking []string          `json:"secret_looking_keys"`
	Aliases       []Alias           `json:"aliases"`
	Versions      []string          `json:"versions"`
	Tags          map[string]string `json:"tags"`
	DLQ           string            `json:"dlq_arn,omitempty"`
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
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Type        string  `json:"type"` // REST or HTTP
	Endpoint    string  `json:"endpoint_type"`
	Routes      []Route `json:"routes"`
	Stages      []Stage `json:"stages"`
	StackOwned  bool    `json:"stack_owned"`
	OwningStack string  `json:"owning_stack,omitempty"`
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
