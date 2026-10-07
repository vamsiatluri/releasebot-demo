package baseline

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Generate emits a CloudFormation template that recreates the captured account.
//
// ⚠️ READ THIS BEFORE APPLYING ANYTHING IT PRODUCES.
//
// This is a STARTING POINT for a human, not an artifact to apply. Generated
// infrastructure-as-code faithfully reproduces whatever accidents the source
// account accumulated -- the memory size somebody bumped during an incident, the
// inline policy nobody can explain, the log group with no retention. Carrying
// those forward is the opposite of what a migration is for.
//
// So the template is written to be READ: every account-specific value becomes a
// parameter, every secret becomes a placeholder that will fail loudly if left
// unfilled, and anything the review flagged is emitted as a TODO comment right
// above the resource it concerns.
//
// The honest workflow is: generate, read the whole thing, delete what should not
// come across, fill in the parameters, deploy to a sandbox, then `compare` the
// sandbox against the original baseline until the diff is empty-by-intent.
func Generate(w io.Writer, b *Baseline, findings []Finding) {
	byResource := map[string][]Finding{}
	for _, f := range findings {
		byResource[f.Resource] = append(byResource[f.Resource], f)
	}

	fmt.Fprintf(w, `AWSTemplateFormatVersion: '2010-09-09'
Description: >
  GENERATED from a capture of account %s (%s) on %s.

  ⚠️ This is a starting point, not a deployable artifact. It reproduces what the
  source account actually contained, including anything that got there by hand.
  Read it, delete what should not come across, then deploy to a sandbox and use
  `+"`baseline compare`"+` against the original until the only remaining
  differences are ones you intended.

  Secrets are NOT carried across. Every secret-looking environment variable is a
  parameter with no default, so the stack fails to deploy until a human supplies
  it deliberately.

Parameters:
  ArtifactBucket:
    Type: String
    Description: Bucket holding the deployment packages in the TARGET account.
`, b.Account, b.Region, b.CapturedAt.Format("2006-01-02"))

	// One parameter per secret-looking variable, NoEcho, no default.
	secretParams := map[string]bool{}
	for _, fn := range b.Functions {
		for _, k := range fn.SecretLooking {
			p := paramName(k)
			if secretParams[p] {
				continue
			}
			secretParams[p] = true
			fmt.Fprintf(w, `  %s:
    Type: String
    NoEcho: true
    Description: >
      Value for %s. NOT copied from the source account -- supply it deliberately.
      Consider holding a Secrets Manager reference here rather than the value
      itself, so that rolling a function back does not roll the secret back too.
`, p, k)
		}
	}

	fmt.Fprint(w, "\nResources:\n\n")

	for _, fn := range b.Functions {
		emitTODOs(w, byResource["lambda/"+fn.Name])

		fmt.Fprintf(w, "  %s:\n    Type: AWS::Lambda::Function\n    Properties:\n", logicalID(fn.Name))
		fmt.Fprintf(w, "      FunctionName: %s\n", fn.Name)
		fmt.Fprintf(w, "      Runtime: %s\n", fn.Runtime)
		fmt.Fprintf(w, "      Handler: %s\n", fn.Handler)
		if len(fn.Architectures) > 0 {
			fmt.Fprintf(w, "      Architectures: [%s]\n", strings.Join(fn.Architectures, ", "))
		}
		fmt.Fprintf(w, "      MemorySize: %d\n      Timeout: %d\n", fn.MemorySize, fn.Timeout)
		if fn.ReservedConc != nil {
			fmt.Fprintf(w, "      ReservedConcurrentExecutions: %d\n", *fn.ReservedConc)
		}
		roleName := shortRole(fn.RoleArn)
		fmt.Fprintf(w, "      Role: !GetAtt %s.Arn\n", logicalID(roleName))
		fmt.Fprintf(w, "      Code:\n        S3Bucket: !Ref ArtifactBucket\n")
		fmt.Fprintf(w, "        S3Key: releasebot/%s.zip   # TODO confirm the key in the target account\n", fn.Name)
		if len(fn.Layers) > 0 {
			fmt.Fprintln(w, "      Layers:")
			for _, l := range fn.Layers {
				fmt.Fprintf(w, "        - %s   # TODO layer ARNs are account+region specific\n", l)
			}
		}
		if len(fn.EnvKeys) > 0 {
			fmt.Fprintln(w, "      Environment:\n        Variables:")
			for _, k := range fn.EnvKeys {
				if IsSecretish(k) {
					fmt.Fprintf(w, "          %s: !Ref %s\n", k, paramName(k))
				} else {
					fmt.Fprintf(w, "          %s: %s\n", k, yamlValue(fn.EnvNonSecret[k]))
				}
			}
		}
		if len(fn.Tags) > 0 {
			fmt.Fprintln(w, "      Tags:")
			for _, k := range sortedKeys(fn.Tags) {
				fmt.Fprintf(w, "        - {Key: %s, Value: %s}\n", k, yamlValue(fn.Tags[k]))
			}
		}
		fmt.Fprintln(w)

		// Aliases are emitted even where the source had none, because they are
		// additive, break nothing, and give the target account a one-step
		// rollback the source never had.
		if len(fn.Aliases) == 0 {
			fmt.Fprintf(w, "  # The source function had NO alias, so nothing there has a one-step\n")
			fmt.Fprintf(w, "  # rollback. Adding one is additive and changes no behaviour.\n")
			fmt.Fprintf(w, "  %sVersion:\n    Type: AWS::Lambda::Version\n    Properties: {FunctionName: !Ref %s}\n",
				logicalID(fn.Name), logicalID(fn.Name))
			fmt.Fprintf(w, "  %sAlias:\n    Type: AWS::Lambda::Alias\n    Properties:\n      FunctionName: !Ref %s\n      FunctionVersion: !GetAtt %sVersion.Version\n      Name: live\n\n",
				logicalID(fn.Name), logicalID(fn.Name), logicalID(fn.Name))
		} else {
			for _, a := range fn.Aliases {
				fmt.Fprintf(w, "  %s%sVersion:\n    Type: AWS::Lambda::Version\n    Properties: {FunctionName: !Ref %s}\n",
					logicalID(fn.Name), logicalID(a.Name), logicalID(fn.Name))
				fmt.Fprintf(w, "  %s%sAlias:\n    Type: AWS::Lambda::Alias\n    Properties:\n      FunctionName: !Ref %s\n      FunctionVersion: !GetAtt %s%sVersion.Version\n      Name: %s\n\n",
					logicalID(fn.Name), logicalID(a.Name), logicalID(fn.Name),
					logicalID(fn.Name), logicalID(a.Name), a.Name)
			}
		}

		// A declared log group, always -- even if the source relied on the
		// implicit one. This is the single cheapest correction the migration can
		// make, and it is the residue that otherwise outlives the decommission.
		fmt.Fprintf(w, "  # Declared deliberately. The source account's group was created by Lambda\n")
		fmt.Fprintf(w, "  # with no expiry and owned by no stack, so it would survive decommissioning.\n")
		fmt.Fprintf(w, "  %sLogGroup:\n    Type: AWS::Logs::LogGroup\n    DeletionPolicy: Delete\n    Properties:\n      LogGroupName: /aws/lambda/%s\n      RetentionInDays: 30   # TODO agree a retention period\n\n",
			logicalID(fn.Name), fn.Name)
	}

	for _, r := range b.Roles {
		emitTODOs(w, byResource["iam/"+r.Name])
		fmt.Fprintf(w, "  %s:\n    Type: AWS::IAM::Role\n    Properties:\n      RoleName: %s\n",
			logicalID(r.Name), r.Name)
		fmt.Fprintf(w, "      AssumeRolePolicyDocument: %s\n", inlineJSON(r.TrustPolicy))
		if len(r.ManagedPolicies) > 0 {
			fmt.Fprintln(w, "      ManagedPolicyArns:")
			for _, p := range sorted(r.ManagedPolicies) {
				fmt.Fprintf(w, "        - %s\n", p)
			}
		}
		if len(r.InlinePolicies) > 0 {
			fmt.Fprintln(w, "      Policies:")
			for _, n := range sortedKeys(r.InlinePolicies) {
				fmt.Fprintf(w, "        # TODO review: inline policy carried over verbatim from the\n")
				fmt.Fprintf(w, "        # source account. Inline policy is where console edits hide.\n")
				fmt.Fprintf(w, "        - PolicyName: %s\n          PolicyDocument: %s\n",
					n, inlineJSON(r.InlinePolicies[n]))
			}
		}
		fmt.Fprintln(w)
	}

	for _, api := range b.APIs {
		emitTODOs(w, byResource["apigateway/"+api.Name])
		fmt.Fprintf(w, "  # API '%s' had %d routes and %d stages in the source account.\n",
			api.Name, len(api.Routes), len(api.Stages))
		fmt.Fprintf(w, "  # Routes: %s\n", routeStr(api.Routes))
		for _, s := range api.Stages {
			fmt.Fprintf(w, "  #   stage %s: variables %s | logging %s | throttle %.0f/%d\n",
				s.Name, kvStr(s.Variables), orNone(s.LoggingLevel), s.ThrottleRate, s.ThrottleBurst)
		}
		fmt.Fprintf(w, "  # ⚠️ The API is NOT generated as resources. Its integration URIs embed the\n")
		fmt.Fprintf(w, "  # source account id, and its invoke URL embeds the API id -- both change on\n")
		fmt.Fprintf(w, "  # migration, which is the single biggest thing this project has to manage.\n")
		fmt.Fprintf(w, "  # Hand-write the API against infra/30-apigw.yaml and use the facts above as\n")
		fmt.Fprintf(w, "  # the specification to match.\n\n")
	}

	fmt.Fprintln(w, "Outputs:")
	for _, fn := range b.Functions {
		fmt.Fprintf(w, "  %sArn: {Value: !GetAtt %s.Arn}\n", logicalID(fn.Name), logicalID(fn.Name))
	}
}

func emitTODOs(w io.Writer, fs []Finding) {
	for _, f := range fs {
		if f.Severity == Info {
			continue
		}
		fmt.Fprintf(w, "  # %s: %s\n", f.Severity, f.What)
		fmt.Fprintf(w, "  #   %s\n", wrap(f.Why, 70, "  #   "))
	}
}

func logicalID(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			if up {
				b.WriteString(strings.ToUpper(string(r)))
				up = false
			} else {
				b.WriteRune(r)
			}
		default:
			up = true
		}
	}
	return b.String()
}

func paramName(envKey string) string { return logicalID(strings.ToLower(envKey)) + "Param" }

func shortRole(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

func yamlValue(s string) string {
	if s == "" {
		return `''`
	}
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`,") || strings.TrimSpace(s) != s {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

func inlineJSON(j string) string {
	if j == "" {
		return "{}"
	}
	return strings.Join(strings.Fields(j), " ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

var _ = sort.Strings
