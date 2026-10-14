package baseline

import (
	"fmt"
	"strings"
)

// captureEstate reads the eleven services the MIGRATION half never needed, so a
// decommission can be planned from the same inventory as the build.
//
// ⚠️ Every call here is a get-, list- or describe-. The whole tool's claim is
// that it is safe to point at production on day one, and that claim dies the
// moment one mutating verb appears in this file.
//
// ⚠️ A failure in any one service is a NOTE, never a fatal error. Accounts
// differ in which services are even enabled, and an SCP can refuse a list call
// outright; losing the CloudFront section must not cost you the other ten. But
// the note has to be loud, because "no distributions found" and "I was not
// allowed to look" are the same empty list.
func (c *Capturer) captureEstate(b *Baseline, owners map[string]string) {
	b.Estate = true
	type step struct {
		name string
		fn   func(*Baseline, map[string]string) error
	}
	for _, s := range []step{
		{"buckets", c.captureBuckets},
		{"tables", c.captureTables},
		{"parameters", c.captureParameters},
		{"networks", c.captureNetworks},
		{"queues and topics", c.captureMessaging},
		{"edge and identity", c.captureEdgeIdentity},
	} {
		if err := s.fn(b, owners); err != nil {
			b.Notes = append(b.Notes, "estate/"+s.name+": "+err.Error()+
				"  ⚠️ this section is UNKNOWN, not empty")
		}
	}
}

// owner looks up stack ownership by any of the identifiers CloudFormation might
// have recorded as the physical id.
//
// ⛔ MATCH EXACTLY, NEVER FUZZILY, and the asymmetry is the reason. A false
// "unowned" is noise -- it sends someone to delete by hand something the stack
// delete already removed, and they find it gone. A false "OWNED" says "the stack
// handles this", nothing deletes it, and it keeps billing silently. That is
// precisely the failure this whole tool exists to prevent, so every candidate
// here is a full identifier and substring matching is deliberately absent.
func owner(owners map[string]string, ids ...string) (bool, string) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if st, ok := owners[id]; ok {
			return true, st
		}
	}
	return false, ""
}

func (c *Capturer) captureBuckets(b *Baseline, owners map[string]string) error {
	var out struct {
		Buckets []struct{ Name string }
	}
	if err := c.aws(&out, "s3api", "list-buckets"); err != nil {
		return err
	}
	for _, bk := range out.Buckets {
		if !c.matchOrSkip("buckets", bk.Name) {
			continue
		}
		// ⚠️ A bucket is GLOBAL but lives in one region, and list-buckets returns
		// every bucket in the account regardless of --region. Reading the
		// location keeps a capture of us-east-1 from claiming a eu-west-1 bucket
		// -- which would put it in a teardown plan for the wrong region.
		var loc struct {
			LocationConstraint string
		}
		if err := c.aws(&loc, "s3api", "get-bucket-location", "--bucket", bk.Name); err != nil {
			continue // almost always a cross-account or denied bucket; skip quietly
		}
		region := loc.LocationConstraint
		if region == "" {
			region = "us-east-1" // the API reports us-east-1 as an empty constraint
		}
		if c.Region != "" && region != c.Region {
			continue
		}
		var ver struct{ Status string }
		_ = c.aws(&ver, "s3api", "get-bucket-versioning", "--bucket", bk.Name)
		st, name := owner(owners, bk.Name)
		b.Buckets = append(b.Buckets, Bucket{
			Name: bk.Name, Versioning: ver.Status, StackOwned: st, OwningStack: name,
		})
	}
	return nil
}

func (c *Capturer) captureTables(b *Baseline, owners map[string]string) error {
	var list struct{ TableNames []string }
	if err := c.aws(&list, "dynamodb", "list-tables"); err != nil {
		return err
	}
	for _, n := range list.TableNames {
		if !c.matchOrSkip("tables", n) {
			continue
		}
		var d struct {
			Table struct {
				ItemCount                 int64
				DeletionProtectionEnabled bool
				TableArn                  string
			}
		}
		if err := c.aws(&d, "dynamodb", "describe-table", "--table-name", n); err != nil {
			continue
		}
		st, stack := owner(owners, n, d.Table.TableArn)
		b.Tables = append(b.Tables, Table{
			Name:               n,
			DeletionProtection: d.Table.DeletionProtectionEnabled,
			// ⚠️ Retain is inferred, and deliberately so. DeletionPolicy lives in
			// the TEMPLATE, not on the resource -- there is no API that reports
			// it. A table a stack owns, that survived a stack delete, was
			// retained; but at capture time all we can say is "a stack owns
			// this", so the teardown plan flags stack-owned tables for a human
			// to check the template rather than asserting a policy it cannot see.
			ItemCount: d.Table.ItemCount, StackOwned: st, OwningStack: stack,
		})
	}
	return nil
}

func (c *Capturer) captureParameters(b *Baseline, _ map[string]string) error {
	// ⛔ describe-parameters, NOT get-parameters. This lists NAMES and TYPES and
	// never touches a value -- the same rule the environment-variable capture
	// follows. A decommission needs to know a SecureString exists so it can be
	// deleted; it never needs to know what is in it.
	var out struct {
		Parameters []struct{ Name, Type string }
	}
	if err := c.aws(&out, "ssm", "describe-parameters", "--max-results", "50"); err != nil {
		return err
	}
	for _, p := range out.Parameters {
		if !c.matchOrSkip("parameters", p.Name) {
			continue
		}
		b.Parameters = append(b.Parameters, Parameter{Name: p.Name, Type: p.Type})
	}
	return nil
}

func (c *Capturer) captureNetworks(b *Baseline, owners map[string]string) error {
	var vpcs struct {
		Vpcs []struct {
			VpcId, CidrBlock string
			IsDefault        bool
		}
	}
	if err := c.aws(&vpcs, "ec2", "describe-vpcs"); err != nil {
		return err
	}
	for _, v := range vpcs.Vpcs {
		// ⛔ The DEFAULT VPC is never in scope for a teardown. It is not part of
		// the application, deleting it breaks unrelated things, and AWS does not
		// replace it on request. It is recorded so the plan can say "left alone,
		// deliberately" rather than silently omitting it.
		n := Network{VpcID: v.VpcId, CidrBlock: v.CidrBlock, IsDefault: v.IsDefault}
		var subs struct{ Subnets []struct{ SubnetId string } }
		if err := c.aws(&subs, "ec2", "describe-subnets",
			"--filters", "Name=vpc-id,Values="+v.VpcId); err == nil {
			n.Subnets = len(subs.Subnets)
		}
		var nats struct {
			NatGateways []struct{ NatGatewayId, State string }
		}
		if err := c.aws(&nats, "ec2", "describe-nat-gateways",
			"--filter", "Name=vpc-id,Values="+v.VpcId); err == nil {
			for _, g := range nats.NatGateways {
				if g.State != "deleted" {
					n.NatGateways++
				}
			}
		}
		var eps struct {
			VpcEndpoints []struct{ VpcEndpointId string }
		}
		if err := c.aws(&eps, "ec2", "describe-vpc-endpoints",
			"--filters", "Name=vpc-id,Values="+v.VpcId); err == nil {
			n.Endpoints = len(eps.VpcEndpoints)
		}
		var enis struct {
			NetworkInterfaces []struct{ NetworkInterfaceId string }
		}
		if err := c.aws(&enis, "ec2", "describe-network-interfaces",
			"--filters", "Name=vpc-id,Values="+v.VpcId); err == nil {
			n.ENIsNow = len(enis.NetworkInterfaces)
		}
		n.StackOwned, n.OwningStack = owner(owners, v.VpcId)
		b.Networks = append(b.Networks, n)
	}
	return nil
}

func (c *Capturer) captureMessaging(b *Baseline, owners map[string]string) error {
	var qs struct{ QueueUrls []string }
	if err := c.aws(&qs, "sqs", "list-queues"); err == nil {
		for _, u := range qs.QueueUrls {
			name := u[strings.LastIndex(u, "/")+1:]
			if !c.matchOrSkip("queues", name) {
				continue
			}
			st, stack := owner(owners, u, name)
			b.Simples = append(b.Simples, Simple{
				Class: "sqs", Name: name, ID: u, StackOwned: st, OwningStack: stack})
		}
	}
	var ts struct {
		Topics []struct{ TopicArn string }
	}
	if err := c.aws(&ts, "sns", "list-topics"); err != nil {
		return err
	}
	for _, t := range ts.Topics {
		name := t.TopicArn[strings.LastIndex(t.TopicArn, ":")+1:]
		if !c.matchOrSkip("topics", name) {
			continue
		}
		st, stack := owner(owners, t.TopicArn, name)
		b.Simples = append(b.Simples, Simple{
			Class: "sns", Name: name, ID: t.TopicArn, StackOwned: st, OwningStack: stack})
	}
	return nil
}

func (c *Capturer) captureEdgeIdentity(b *Baseline, owners map[string]string) error {
	// ⚠️ CloudFront and WAF(global) are GLOBAL services: they answer only in
	// us-east-1 regardless of where the rest of the estate lives. A capture of
	// another region that silently reported zero distributions would tell a
	// decommission there is no CDN to disable.
	global := c.Region == "" || c.Region == "us-east-1"

	if global {
		var d struct {
			DistributionList struct {
				Items []struct {
					Id, ARN, Comment, Status string
					Enabled                  bool
				}
			}
		}
		if err := c.aws(&d, "cloudfront", "list-distributions"); err == nil {
			for _, dist := range d.DistributionList.Items {
				if !c.matchOrSkip("distributions", dist.Comment+" "+dist.Id) {
					continue
				}
				st, stack := owner(owners, dist.Id, dist.ARN)
				// ⛔ A distribution must be DISABLED and fully propagated before
				// it can be deleted, and propagation takes 15-20 minutes. That is
				// a scheduling fact, not an error -- the plan has to start it
				// early rather than discover it at the end.
				b.Simples = append(b.Simples, Simple{
					Class: "cloudfront", Name: dist.Comment, ID: dist.Id,
					Detail:     fmt.Sprintf("enabled=%t status=%s", dist.Enabled, dist.Status),
					StackOwned: st, OwningStack: stack})
			}
		}
		var w struct {
			WebACLs []struct{ Name, Id, ARN string }
		}
		if err := c.aws(&w, "wafv2", "list-web-acls", "--scope", "CLOUDFRONT"); err == nil {
			for _, a := range w.WebACLs {
				if !c.matchOrSkip("web acls", a.Name) {
					continue
				}
				st, stack := owner(owners, wafPhysicalID(a.Name, a.Id, "CLOUDFRONT"), a.ARN, a.Id)
				b.Simples = append(b.Simples, Simple{Class: "wafv2", Name: a.Name,
					ID: a.ARN, Detail: "scope=CLOUDFRONT", StackOwned: st, OwningStack: stack})
			}
		}
	}

	var wr struct {
		WebACLs []struct{ Name, Id, ARN string }
	}
	if err := c.aws(&wr, "wafv2", "list-web-acls", "--scope", "REGIONAL"); err == nil {
		for _, a := range wr.WebACLs {
			if !c.matchOrSkip("web acls", a.Name) {
				continue
			}
			st, stack := owner(owners, wafPhysicalID(a.Name, a.Id, "REGIONAL"), a.ARN, a.Id)
			b.Simples = append(b.Simples, Simple{Class: "wafv2", Name: a.Name,
				ID: a.ARN, Detail: "scope=REGIONAL", StackOwned: st, OwningStack: stack})
		}
	}

	var pools struct {
		UserPools []struct{ Id, Name string }
	}
	if err := c.aws(&pools, "cognito-idp", "list-user-pools", "--max-results", "60"); err != nil {
		return err
	}
	for _, p := range pools.UserPools {
		if !c.matchOrSkip("user pools", p.Name) {
			continue
		}
		st, stack := owner(owners, p.Id)
		b.Simples = append(b.Simples, Simple{Class: "cognito", Name: p.Name, ID: p.Id,
			StackOwned: st, OwningStack: stack})
	}
	return nil
}

// wafPhysicalID rebuilds the identifier CloudFormation actually stores for a
// WAFv2 web ACL.
//
// ⚠️ A PHYSICAL RESOURCE ID IS NOT ONE CANONICAL FORM. Most resource types
// record a name or an ARN, but some record a COMPOSITE -- and WAFv2 records
// `name|uuid|scope`:
//
//	releasebot-edge|acfde826-4731-4cb4-81d7-83e2fc3ded39|REGIONAL
//
// So looking it up by its ARN, its id, or its name all miss, and a web ACL a
// stack plainly owns is reported as hand-made. Found by reading a real plan and
// not believing a line in it -- which is the only way this class of bug surfaces.
func wafPhysicalID(name, id, scope string) string {
	if name == "" || id == "" {
		return ""
	}
	return name + "|" + id + "|" + scope
}
