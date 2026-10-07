package baseline

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Capture reads an account through the AWS CLI.
//
// Shelling out to the CLI rather than linking an SDK is a deliberate trade:
//
//   - it uses whatever profile, SSO session or assumed role the operator already
//     has working, instead of a second credential path to debug
//   - every call this tool makes is a command a human can run themselves and
//     check, which matters for a tool whose job is reading a production account
//   - the repo stays dependency-free, so the binary that reads production has no
//     third-party supply chain
//
// The cost is that the CLI must be present and the JSON shapes are matched
// loosely. That is the right way round for an audit tool.
type Capturer struct {
	Profile string
	Region  string
	Prefix  string // only resources whose name contains this are captured
	Verbose bool
}

func (c *Capturer) aws(out any, args ...string) error {
	full := append([]string{}, args...)
	if c.Profile != "" {
		full = append(full, "--profile", c.Profile)
	}
	if c.Region != "" {
		full = append(full, "--region", c.Region)
	}
	full = append(full, "--output", "json", "--no-cli-pager")
	if c.Verbose {
		fmt.Printf("  aws %s\n", strings.Join(args, " "))
	}
	cmd := exec.Command("aws", full...)
	raw, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("aws %s: %s", strings.Join(args, " "),
				strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("aws %s: %w", strings.Join(args, " "), err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *Capturer) match(name string) bool {
	return c.Prefix == "" || strings.Contains(strings.ToLower(name), strings.ToLower(c.Prefix))
}

func (c *Capturer) Capture(label string) (*Baseline, error) {
	b := &Baseline{CapturedAt: time.Now().UTC(), Label: label, Region: c.Region}

	var who struct{ Account string }
	if err := c.aws(&who, "sts", "get-caller-identity"); err != nil {
		return nil, err
	}
	b.Account = who.Account

	// Ownership first: every later section asks "does a stack claim this?", and
	// the answer is the most useful single fact in the capture. A resource no
	// stack owns was made by hand, and a hand-made resource is exactly what a
	// template-derived baseline misses.
	owners, err := c.stackOwnership()
	if err != nil {
		b.Notes = append(b.Notes, "could not read stack ownership: "+err.Error())
		owners = map[string]string{}
	}

	if err := c.captureFunctions(b, owners); err != nil {
		return nil, err
	}
	if err := c.captureRoles(b, owners); err != nil {
		b.Notes = append(b.Notes, "roles: "+err.Error())
	}
	if err := c.captureAPIs(b, owners); err != nil {
		b.Notes = append(b.Notes, "apis: "+err.Error())
	}
	if err := c.captureLogGroups(b); err != nil {
		b.Notes = append(b.Notes, "log groups: "+err.Error())
	}
	if err := c.captureAlarms(b, owners); err != nil {
		b.Notes = append(b.Notes, "alarms: "+err.Error())
	}
	return b, nil
}

// stackOwnership maps physical resource id -> stack name.
func (c *Capturer) stackOwnership() (map[string]string, error) {
	owners := map[string]string{}
	var stacks struct {
		StackSummaries []struct{ StackName, StackStatus string }
	}
	if err := c.aws(&stacks, "cloudformation", "list-stacks",
		"--stack-status-filter", "CREATE_COMPLETE", "UPDATE_COMPLETE", "UPDATE_ROLLBACK_COMPLETE"); err != nil {
		return owners, err
	}
	for _, s := range stacks.StackSummaries {
		var res struct {
			StackResourceSummaries []struct{ PhysicalResourceId, LogicalResourceId, ResourceType string }
		}
		if err := c.aws(&res, "cloudformation", "list-stack-resources", "--stack-name", s.StackName); err != nil {
			continue
		}
		for _, r := range res.StackResourceSummaries {
			if r.PhysicalResourceId != "" {
				owners[r.PhysicalResourceId] = s.StackName
			}
		}
	}
	return owners, nil
}

func (c *Capturer) captureFunctions(b *Baseline, owners map[string]string) error {
	var list struct {
		Functions []struct {
			FunctionName, Runtime, Handler, Role, CodeSha256, LastModified string
			MemorySize, Timeout                                            int
			CodeSize                                                       int64
			Architectures                                                  []string
			Environment                                                    struct{ Variables map[string]string }
			Layers                                                         []struct{ Arn string }
			DeadLetterConfig                                               struct{ TargetArn string }
		}
	}
	if err := c.aws(&list, "lambda", "list-functions"); err != nil {
		return err
	}

	for _, f := range list.Functions {
		if !c.match(f.FunctionName) {
			continue
		}
		keys, safe, secretLooking := SplitEnv(f.Environment.Variables)
		fn := Function{
			Name: f.FunctionName, Runtime: f.Runtime, Handler: f.Handler,
			Architectures: f.Architectures, MemorySize: f.MemorySize, Timeout: f.Timeout,
			RoleArn: f.Role, CodeSha256: f.CodeSha256, CodeSize: f.CodeSize,
			LastModified: f.LastModified,
			EnvKeys:      keys, EnvNonSecret: safe, SecretLooking: secretLooking,
			DLQ:  f.DeadLetterConfig.TargetArn,
			Tags: map[string]string{},
		}
		for _, l := range f.Layers {
			fn.Layers = append(fn.Layers, l.Arn)
		}
		if st, ok := owners[f.FunctionName]; ok {
			fn.StackOwned, fn.OwningStack = true, st
		}

		var conc struct{ ReservedConcurrentExecutions *int }
		if err := c.aws(&conc, "lambda", "get-function-concurrency", "--function-name", f.FunctionName); err == nil {
			fn.ReservedConc = conc.ReservedConcurrentExecutions
		}

		var aliases struct {
			Aliases []struct {
				Name, FunctionVersion, Description string
				RoutingConfig                      *json.RawMessage
			}
		}
		if err := c.aws(&aliases, "lambda", "list-aliases", "--function-name", f.FunctionName); err == nil {
			for _, a := range aliases.Aliases {
				al := Alias{Name: a.Name, Version: a.FunctionVersion, Description: a.Description}
				if a.RoutingConfig != nil {
					al.RoutingConfig = string(*a.RoutingConfig)
				}
				fn.Aliases = append(fn.Aliases, al)
			}
		}

		var vers struct{ Versions []struct{ Version string } }
		if err := c.aws(&vers, "lambda", "list-versions-by-function", "--function-name", f.FunctionName); err == nil {
			for _, v := range vers.Versions {
				fn.Versions = append(fn.Versions, v.Version)
			}
		}

		// Who may invoke it. Recreating a function without this gives you a
		// function that exists and that nothing can call -- a failure mode that
		// looks like "the API is broken".
		//
		// ⚠️ The policy must be read per QUALIFIER, not just on the bare
		// function. A permission granted on `cutRelease:live` does not appear on
		// `cutRelease`, so querying only the unqualified name reports "no
		// resource policy" for a function that is perfectly reachable -- which
		// is exactly the sort of confident-and-wrong finding that makes people
		// stop trusting a tool. Found by running this against a real account.
		var pol struct{ Policy string }
		if err := c.aws(&pol, "lambda", "get-policy", "--function-name", f.FunctionName); err == nil {
			fn.ResourcePolicy = pol.Policy
		}
		for _, al := range fn.Aliases {
			var apol struct{ Policy string }
			if err := c.aws(&apol, "lambda", "get-policy",
				"--function-name", f.FunctionName+":"+al.Name); err == nil && apol.Policy != "" {
				if fn.ResourcePolicy == "" {
					fn.ResourcePolicy = apol.Policy
				} else {
					fn.ResourcePolicy += "\n" + apol.Policy
				}
			}
		}

		var tags struct{ Tags map[string]string }
		arn := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", b.Region, b.Account, f.FunctionName)
		if err := c.aws(&tags, "lambda", "list-tags", "--resource", arn); err == nil {
			fn.Tags = tags.Tags
		}

		b.Functions = append(b.Functions, fn)
	}
	return nil
}

func (c *Capturer) captureRoles(b *Baseline, owners map[string]string) error {
	seen := map[string]bool{}
	for _, f := range b.Functions {
		name := f.RoleArn
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		var got struct {
			Role struct {
				Arn                      string
				AssumeRolePolicyDocument any
			}
		}
		if err := c.aws(&got, "iam", "get-role", "--role-name", name); err != nil {
			b.Notes = append(b.Notes, "role "+name+": "+err.Error())
			continue
		}
		trust, _ := json.Marshal(got.Role.AssumeRolePolicyDocument)
		r := Role{Name: name, Arn: got.Role.Arn, TrustPolicy: string(trust),
			InlinePolicies: map[string]string{}}

		var managed struct{ AttachedPolicies []struct{ PolicyArn string } }
		if err := c.aws(&managed, "iam", "list-attached-role-policies", "--role-name", name); err == nil {
			for _, p := range managed.AttachedPolicies {
				r.ManagedPolicies = append(r.ManagedPolicies, p.PolicyArn)
			}
		}

		// Inline policies are where console edits hide. A role carrying inline
		// policy that no template mentions is drift, every time.
		var inline struct{ PolicyNames []string }
		if err := c.aws(&inline, "iam", "list-role-policies", "--role-name", name); err == nil {
			for _, pn := range inline.PolicyNames {
				var doc struct{ PolicyDocument any }
				if err := c.aws(&doc, "iam", "get-role-policy", "--role-name", name, "--policy-name", pn); err == nil {
					j, _ := json.Marshal(doc.PolicyDocument)
					r.InlinePolicies[pn] = string(j)
				}
			}
		}
		if st, ok := owners[name]; ok {
			r.StackOwned, r.OwningStack = true, st
		}
		b.Roles = append(b.Roles, r)
	}
	return nil
}

func (c *Capturer) captureAPIs(b *Baseline, owners map[string]string) error {
	var apis struct {
		Items []struct {
			Id, Name              string
			EndpointConfiguration struct{ Types []string }
		}
	}
	if err := c.aws(&apis, "apigateway", "get-rest-apis"); err != nil {
		return err
	}
	for _, a := range apis.Items {
		if !c.match(a.Name) {
			continue
		}
		api := API{ID: a.Id, Name: a.Name, Type: "REST"}
		if len(a.EndpointConfiguration.Types) > 0 {
			api.Endpoint = a.EndpointConfiguration.Types[0]
		}
		if st, ok := owners[a.Id]; ok {
			api.StackOwned, api.OwningStack = true, st
		}

		var res struct {
			Items []struct {
				Id, Path        string
				ResourceMethods map[string]any
			}
		}
		if err := c.aws(&res, "apigateway", "get-resources", "--rest-api-id", a.Id); err == nil {
			for _, r := range res.Items {
				for method := range r.ResourceMethods {
					var m struct {
						AuthorizationType string
						MethodIntegration struct{ Type, Uri string }
					}
					if err := c.aws(&m, "apigateway", "get-method", "--rest-api-id", a.Id,
						"--resource-id", r.Id, "--http-method", method); err == nil {
						api.Routes = append(api.Routes, Route{
							Path: r.Path, Method: method,
							Authorization:   m.AuthorizationType,
							IntegrationType: m.MethodIntegration.Type,
							IntegrationURI:  m.MethodIntegration.Uri,
						})
					}
				}
			}
		}

		var stages struct {
			Item []struct {
				StageName, DeploymentId string
				Variables               map[string]string
				TracingEnabled          bool
				AccessLogSettings       struct{ DestinationArn string }
				MethodSettings          map[string]struct {
					LoggingLevel         string
					DataTraceEnabled     bool
					ThrottlingRateLimit  float64
					ThrottlingBurstLimit int
				}
			}
		}
		if err := c.aws(&stages, "apigateway", "get-stages", "--rest-api-id", a.Id); err == nil {
			for _, s := range stages.Item {
				st := Stage{Name: s.StageName, DeploymentID: s.DeploymentId,
					Variables: s.Variables, TracingEnabled: s.TracingEnabled,
					AccessLogDest: s.AccessLogSettings.DestinationArn}
				for _, ms := range s.MethodSettings {
					st.LoggingLevel, st.DataTrace = ms.LoggingLevel, ms.DataTraceEnabled
					st.ThrottleRate, st.ThrottleBurst = ms.ThrottlingRateLimit, ms.ThrottlingBurstLimit
					break
				}
				api.Stages = append(api.Stages, st)
			}
		}
		b.APIs = append(b.APIs, api)
	}
	return nil
}

func (c *Capturer) captureLogGroups(b *Baseline) error {
	var groups struct {
		LogGroups []struct {
			LogGroupName    string
			RetentionInDays int
			StoredBytes     int64
		}
	}
	if err := c.aws(&groups, "logs", "describe-log-groups"); err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, f := range b.Functions {
		declared["/aws/lambda/"+f.Name] = true
	}
	for _, g := range groups.LogGroups {
		if !c.match(g.LogGroupName) && !declared[g.LogGroupName] {
			continue
		}
		b.LogGroups = append(b.LogGroups, LogGroup{
			Name: g.LogGroupName, RetentionDays: g.RetentionInDays,
			StoredBytes: g.StoredBytes,
			// No retention set is the signature of a group Lambda created for
			// itself rather than one a template declared.
			Implicit: g.RetentionInDays == 0,
		})
	}
	return nil
}

func (c *Capturer) captureAlarms(b *Baseline, owners map[string]string) error {
	var alarms struct {
		MetricAlarms []struct {
			AlarmName, Namespace, MetricName, ComparisonOperator, TreatMissingData string
			Threshold                                                              float64
			ActionsEnabled                                                         bool
			AlarmActions                                                           []string
		}
	}
	if err := c.aws(&alarms, "cloudwatch", "describe-alarms"); err != nil {
		return err
	}
	for _, a := range alarms.MetricAlarms {
		if !c.match(a.AlarmName) {
			continue
		}
		_, owned := owners[a.AlarmName]
		b.Alarms = append(b.Alarms, Alarm{
			Name: a.AlarmName, Namespace: a.Namespace, MetricName: a.MetricName,
			Threshold: a.Threshold, Comparison: a.ComparisonOperator,
			TreatMissing: a.TreatMissingData, Actions: a.AlarmActions,
			ActionsEnabled: a.ActionsEnabled, StackOwned: owned,
		})
	}
	return nil
}
