// Package cloudformation — route66 fork addition.
//
// materialize.go turns CreateStack from a bookkeeping facade into a real
// provisioner for the resource types route66's local-test CoreData stack
// uses: on CreateStack the template's Resources are walked in dependency
// order and dispatched into the sibling kumo service stores (dynamodb, s3,
// ssm) IN-PROCESS via the service registry — no self-HTTP. Resource types
// that are legitimately inert for our stack (IAM, Glue, Athena) are tracked
// with an explicit Materialization marker instead of being silently green.
// Any resource type outside the known set FAILS the stack loudly: the
// silent-skip facade is exactly the bug this fork fixes.
package cloudformation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/sivchari/kumo/internal/service"
	"github.com/sivchari/kumo/internal/service/dynamodb"
	"github.com/sivchari/kumo/internal/service/s3"
	"github.com/sivchari/kumo/internal/service/ssm"
)

// Materialization marker values recorded on each StackResource.
const (
	// MaterializationReal — the resource exists in the backing kumo store.
	MaterializationReal = "MATERIALIZED"
	// MaterializationInert — the resource is status-tracked only. Legit for
	// IAM/Glue/Athena in a local emulator (nothing enforces them), but the
	// marker makes that visible instead of silently green.
	MaterializationInert = "TRACKED_INERT"
)

// inertTypes are the resource types we deliberately do not materialize.
var inertTypes = map[string]bool{
	"AWS::IAM::Role":          true,
	"AWS::IAM::ManagedPolicy": true,
	"AWS::Glue::Database":     true,
	"AWS::Athena::WorkGroup":  true,
	// CDK metadata is pure bookkeeping when present.
	"AWS::CDK::Metadata": true,
}

// evalCtx carries everything needed to evaluate Ref / Fn::Sub in a template.
type evalCtx struct {
	region    string
	accountID string
	partition string
	urlSuffix string
	// params: template parameter values (request overrides or defaults).
	params map[string]string
	// physical: logicalID -> physical resource name, filled as resources
	// materialize, so Ref-to-resource resolves to the real name.
	physical map[string]string
}

func (c *evalCtx) pseudo(name string) (string, bool) {
	switch name {
	case "AWS::Region":
		return c.region, true
	case "AWS::AccountId":
		return c.accountID, true
	case "AWS::Partition":
		return c.partition, true
	case "AWS::URLSuffix":
		return c.urlSuffix, true
	case "AWS::NoValue":
		return "", true
	}

	return "", false
}

// resolveRef resolves a Ref target: pseudo param, template param, or an
// already-materialized resource's physical name. Unknown targets are a hard
// error — never a silent passthrough.
func (c *evalCtx) resolveRef(target string) (string, error) {
	if v, ok := c.pseudo(target); ok {
		return v, nil
	}

	if v, ok := c.params[target]; ok {
		return v, nil
	}

	if v, ok := c.physical[target]; ok {
		return v, nil
	}

	return "", fmt.Errorf("unresolvable Ref %q", target)
}

// evalSub evaluates the string form of Fn::Sub over ${...} tokens.
func (c *evalCtx) evalSub(tmpl string) (string, error) {
	var out strings.Builder

	for {
		i := strings.Index(tmpl, "${")
		if i < 0 {
			out.WriteString(tmpl)

			return out.String(), nil
		}

		out.WriteString(tmpl[:i])
		rest := tmpl[i+2:]

		j := strings.Index(rest, "}")
		if j < 0 {
			return "", fmt.Errorf("unterminated ${ in Fn::Sub %q", tmpl)
		}

		token := rest[:j]
		if strings.HasPrefix(token, "!") { // ${!Literal} escape
			out.WriteString("${" + token[1:] + "}")
		} else {
			v, err := c.resolveRef(token)
			if err != nil {
				return "", fmt.Errorf("Fn::Sub: %w", err)
			}

			out.WriteString(v)
		}

		tmpl = rest[j+1:]
	}
}

// eval recursively evaluates a property tree, resolving Ref and Fn::Sub
// (string form). Any other intrinsic (Fn::GetAtt, Fn::If, ...) is a hard
// error: the caller only evaluates properties of resources we materialize,
// and those must be fully resolvable.
func (c *evalCtx) eval(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if ref, ok := t["Ref"]; ok {
				name, ok := ref.(string)
				if !ok {
					return nil, fmt.Errorf("non-string Ref %v", ref)
				}

				return c.resolveRef(name)
			}

			if sub, ok := t["Fn::Sub"]; ok {
				s, ok := sub.(string)
				if !ok {
					return nil, fmt.Errorf("only string-form Fn::Sub is supported, got %T", sub)
				}

				return c.evalSub(s)
			}

			for k := range t {
				if strings.HasPrefix(k, "Fn::") {
					return nil, fmt.Errorf("unsupported intrinsic %s", k)
				}
			}
		}

		out := make(map[string]any, len(t))

		for k, val := range t {
			ev, err := c.eval(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}

			out[k] = ev
		}

		return out, nil
	case []any:
		out := make([]any, len(t))

		for i, val := range t {
			ev, err := c.eval(val)
			if err != nil {
				return nil, err
			}

			out[i] = ev
		}

		return out, nil
	default:
		return v, nil
	}
}

// backends bundles the sibling service stores the materializer writes into.
type backends struct {
	ddb dynamodb.Storage
	s3  s3.Storage
	ssm ssm.Storage
}

// lookupBackends resolves the sibling services from the in-process registry.
func lookupBackends() (*backends, error) {
	b := &backends{}

	if svc, ok := service.Get("dynamodb"); ok {
		if d, ok := svc.(*dynamodb.Service); ok {
			b.ddb = d.StorageBackend()
		}
	}

	if svc, ok := service.Get("s3"); ok {
		if s, ok := svc.(*s3.Service); ok {
			b.s3 = s.StorageBackend()
		}
	}

	if svc, ok := service.Get("ssm"); ok {
		if s, ok := svc.(*ssm.Service); ok {
			b.ssm = s.StorageBackend()
		}
	}

	if b.ddb == nil || b.s3 == nil || b.ssm == nil {
		return nil, fmt.Errorf("materializer: sibling services not registered (dynamodb=%v s3=%v ssm=%v)", b.ddb != nil, b.s3 != nil, b.ssm != nil)
	}

	return b, nil
}

// materializeStack walks the template's Resources in dependency order and
// creates real resources in the sibling stores. On success it rewrites
// stack.Resources with real physical IDs and Materialization markers.
// On any error the caller must roll the stack back — partial creations are
// torn down here before returning.
func materializeStack(ctx context.Context, req *CreateStackRequest, stack *Stack, region string) error {
	var template map[string]any
	if err := json.Unmarshal([]byte(req.TemplateBody), &template); err != nil {
		return fmt.Errorf("template parse: %w", err)
	}

	resources, _ := template["Resources"].(map[string]any)
	if resources == nil {
		return fmt.Errorf("template has no Resources section")
	}

	ec := &evalCtx{
		region:    region,
		accountID: "000000000000",
		partition: "aws",
		urlSuffix: "amazonaws.com",
		params:    map[string]string{},
		physical:  map[string]string{},
	}

	// Parameter values: template defaults overlaid by request parameters.
	if pdefs, ok := template["Parameters"].(map[string]any); ok {
		for name, def := range pdefs {
			if dm, ok := def.(map[string]any); ok {
				if dv, ok := dm["Default"].(string); ok {
					ec.params[name] = dv
				}
			}
		}
	}

	for k, v := range req.Parameters {
		ec.params[k] = v
	}

	b, err := lookupBackends()
	if err != nil {
		return err
	}

	// Reject unknown resource types up front — fail the whole stack before
	// creating anything, so there is nothing to roll back for this case.
	for logicalID, raw := range resources {
		rm, _ := raw.(map[string]any)
		rtype, _ := rm["Type"].(string)

		switch {
		case rtype == "AWS::DynamoDB::Table", rtype == "AWS::S3::Bucket", rtype == "AWS::SSM::Parameter":
		case inertTypes[rtype]:
		default:
			return fmt.Errorf("resource %s has type %q which this emulator cannot materialize; refusing to silently skip it", logicalID, rtype)
		}
	}

	// Dependency order: repeated passes creating every resource whose Ref /
	// DependsOn targets are all satisfied. Our template's only edges are
	// Ref-to-resource (SSM param -> bucket); cycles fail loudly.
	done := map[string]bool{}
	newResources := []StackResource{}

	for len(done) < len(resources) {
		progressed := false

		for logicalID, raw := range resources {
			if done[logicalID] {
				continue
			}

			rm, _ := raw.(map[string]any)
			if !depsSatisfied(rm, resources, done) {
				continue
			}

			sr, err := materializeOne(ctx, b, ec, logicalID, rm, stack)
			if err != nil {
				rollbackMaterialized(ctx, b, newResources)

				return fmt.Errorf("resource %s: %w", logicalID, err)
			}

			newResources = append(newResources, sr)
			done[logicalID] = true
			progressed = true
		}

		if !progressed {
			rollbackMaterialized(ctx, b, newResources)

			return fmt.Errorf("dependency cycle or unresolvable reference among remaining resources")
		}
	}

	stack.Resources = newResources

	return nil
}

// depsSatisfied reports whether every DependsOn entry and every Ref to a
// sibling resource inside rm is already created.
func depsSatisfied(rm map[string]any, all map[string]any, done map[string]bool) bool {
	switch d := rm["DependsOn"].(type) {
	case string:
		if !done[d] {
			return false
		}
	case []any:
		for _, e := range d {
			if s, ok := e.(string); ok && !done[s] {
				return false
			}
		}
	}

	for _, target := range collectRefs(rm["Properties"]) {
		if _, isResource := all[target]; isResource && !done[target] {
			return false
		}
	}

	return true
}

// collectRefs finds all {"Ref": X} targets in a property tree.
func collectRefs(v any) []string {
	var out []string

	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["Ref"].(string); ok && len(t) == 1 {
			return []string{ref}
		}

		for _, val := range t {
			out = append(out, collectRefs(val)...)
		}
	case []any:
		for _, val := range t {
			out = append(out, collectRefs(val)...)
		}
	}

	return out
}

// materializeOne creates a single resource in the right store and returns
// its StackResource record with the Materialization marker set.
func materializeOne(ctx context.Context, b *backends, ec *evalCtx, logicalID string, rm map[string]any, stack *Stack) (StackResource, error) {
	rtype, _ := rm["Type"].(string)

	sr := StackResource{
		LogicalResourceID: logicalID,
		ResourceType:      rtype,
		ResourceStatus:    ResourceStatusCreateComplete,
		StackID:           stack.StackID,
		StackName:         stack.StackName,
		Timestamp:         stack.CreationTime,
	}

	if inertTypes[rtype] {
		// Deliberately not materialized — but say so, loudly and queryably.
		sr.Materialization = MaterializationInert
		sr.PhysicalResourceID = logicalID + "-inert"
		ec.physical[logicalID] = sr.PhysicalResourceID

		return sr, nil
	}

	props, err := ec.eval(rm["Properties"])
	if err != nil {
		return sr, err
	}

	pm, _ := props.(map[string]any)

	switch rtype {
	case "AWS::DynamoDB::Table":
		sr.PhysicalResourceID, err = createTable(ctx, b.ddb, pm)
	case "AWS::S3::Bucket":
		sr.PhysicalResourceID, err = createBucket(ctx, b.s3, pm, stack.StackName, logicalID)
	case "AWS::SSM::Parameter":
		sr.PhysicalResourceID, err = createParameter(ctx, b.ssm, pm)
	default:
		err = fmt.Errorf("unreachable: unvetted type %s", rtype)
	}

	if err != nil {
		return sr, err
	}

	sr.Materialization = MaterializationReal
	ec.physical[logicalID] = sr.PhysicalResourceID

	return sr, nil
}

// createTable maps evaluated CFN table properties onto kumo's CreateTable.
// The CFN property names for the fields we support are byte-identical to
// the DynamoDB API JSON, so a marshal/unmarshal round trip is the mapping.
func createTable(ctx context.Context, ddb dynamodb.Storage, props map[string]any) (string, error) {
	raw, err := json.Marshal(props)
	if err != nil {
		return "", fmt.Errorf("marshal table props: %w", err)
	}

	var req dynamodb.CreateTableRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("map table props: %w", err)
	}

	if req.TableName == "" {
		return "", fmt.Errorf("TableName is required (physical-name generation not implemented in this spike)")
	}

	if _, err := ddb.CreateTable(ctx, &req); err != nil {
		return "", fmt.Errorf("CreateTable: %w", err)
	}

	// TTL is a separate call on the real API; same here.
	if ttl, ok := props["TimeToLiveSpecification"].(map[string]any); ok {
		attr, _ := ttl["AttributeName"].(string)
		enabled, _ := ttl["Enabled"].(bool)

		if err := ddb.UpdateTimeToLive(ctx, req.TableName, attr, enabled); err != nil {
			return "", fmt.Errorf("UpdateTimeToLive: %w", err)
		}
	}

	return req.TableName, nil
}

func createBucket(ctx context.Context, st s3.Storage, props map[string]any, stackName, logicalID string) (string, error) {
	name, _ := props["BucketName"].(string)
	if name == "" {
		// CFN generates a name when BucketName is omitted.
		name = strings.ToLower(stackName + "-" + logicalID)
	}

	if err := st.CreateBucket(ctx, name); err != nil {
		return "", fmt.Errorf("CreateBucket: %w", err)
	}

	return name, nil
}

func createParameter(ctx context.Context, st ssm.Storage, props map[string]any) (string, error) {
	name, _ := props["Name"].(string)
	ptype, _ := props["Type"].(string)
	value, _ := props["Value"].(string)

	if name == "" || value == "" {
		return "", fmt.Errorf("SSM parameter requires Name and Value")
	}

	if _, err := st.PutParameter(ctx, &ssm.PutParameterRequest{
		Name:      name,
		Type:      ptype,
		Value:     value,
		Overwrite: true,
	}); err != nil {
		return "", fmt.Errorf("PutParameter: %w", err)
	}

	return name, nil
}

// rollbackMaterialized best-effort deletes resources created so far, in
// reverse order, when a later resource fails.
func rollbackMaterialized(ctx context.Context, b *backends, created []StackResource) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = teardownOne(ctx, b, created[i])
	}
}

// teardownStack deletes every MATERIALIZED resource of a stack, reverse
// creation order. Inert resources have nothing to tear down.
// NOTE (spike): DeletionPolicy=Retain is deliberately ignored — the local
// emulator's DeleteStack is expected to leave a clean slate.
func teardownStack(ctx context.Context, resources []*StackResource) error {
	b, err := lookupBackends()
	if err != nil {
		return err
	}

	var errs []string

	for i := len(resources) - 1; i >= 0; i-- {
		if resources[i].Materialization != MaterializationReal {
			continue
		}

		if err := teardownOne(ctx, b, *resources[i]); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", resources[i].LogicalResourceID, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("teardown failed: %s", strings.Join(errs, "; "))
	}

	return nil
}

func teardownOne(ctx context.Context, b *backends, sr StackResource) error {
	if sr.Materialization != MaterializationReal {
		return nil
	}

	switch sr.ResourceType {
	case "AWS::DynamoDB::Table":
		_, err := b.ddb.DeleteTable(ctx, sr.PhysicalResourceID)

		return err
	case "AWS::S3::Bucket":
		return b.s3.DeleteBucket(ctx, sr.PhysicalResourceID)
	case "AWS::SSM::Parameter":
		return b.ssm.DeleteParameter(ctx, sr.PhysicalResourceID)
	}

	return nil
}

// regionFromAuthHeader extracts the signing region from a SigV4 Authorization
// header (Credential=AKIA.../20260823/us-west-2/cloudformation/aws4_request).
// Empty string when absent — caller applies a default.
func regionFromAuthHeader(auth string) string {
	i := strings.Index(auth, "Credential=")
	if i < 0 {
		return ""
	}

	parts := strings.Split(strings.SplitN(auth[i+len("Credential="):], ",", 2)[0], "/")
	if len(parts) >= 3 {
		return parts[2]
	}

	return ""
}

// resolveRegion determines the region ${AWS::Region} resolves to.
// Precedence follows kumo's own convention (dynamodb storage reads
// AWS_DEFAULT_REGION): 1) the instance's configured AWS_DEFAULT_REGION,
// 2) the request's SigV4 signing region, 3) us-east-1.
func resolveRegion(r *http.Request) string {
	if v := os.Getenv("AWS_DEFAULT_REGION"); v != "" {
		return v
	}

	if v := regionFromAuthHeader(r.Header.Get("Authorization")); v != "" {
		return v
	}

	return "us-east-1"
}
