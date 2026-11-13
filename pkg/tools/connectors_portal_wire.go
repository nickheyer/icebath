package tools

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ConnectorTool is one tool row of a Portal connector listing
type ConnectorTool struct {
	Slug, Name, Description, Facet string
	Hints, Categories              []string
	NoAuth, Deprecated             bool
}

// ConnectorToolsListing is the Portal tools listing of one connector
type ConnectorToolsListing struct {
	Connector, ToolkitVersion, Etag string
	Tools                           []ConnectorTool
}

// ConnectorCatalogRow is one connector of the Portal catalog, a nil LogoURL being None
type ConnectorCatalogRow struct {
	Slug, Name, Description, Category string
	LogoURL                           *string
}

// ConnectorCatalogResponse is the Portal connector catalog
type ConnectorCatalogResponse struct{ Connectors []ConnectorCatalogRow }

// PolicyTags carries the tag rules of a policy, a nil list being None
type PolicyTags struct{ Enable, Disable []string }

// PolicyToolRule lists the disabled tools of one connector
type PolicyToolRule struct{ Disable []string }

// PolicyBody is one of the unrestricted, deny-all, allow and deny policy bodies picked by Mode
type PolicyBody struct {
	Mode               string
	Connectors         []string
	DisabledConnectors []string
	ToolKeys           []string
	Tools              map[string]PolicyToolRule
	Tags               *PolicyTags
}

// ConnectorPolicyEffective is the effective policy with its version, revision and issue time
type ConnectorPolicyEffective struct {
	PolicyBody
	Version    int64
	Revision   string
	IssuedAtMs int64
}

// ConnectorPolicyLayer is one org, role or member layer of the policy, a nil ID being None
type ConnectorPolicyLayer struct {
	Kind     string
	ID       *string
	Body     PolicyBody
	Revision string
}

// ConnectorPolicyResponse is the Portal policy read answer
type ConnectorPolicyResponse struct {
	Layers    []ConnectorPolicyLayer
	Effective ConnectorPolicyEffective
}

// ConnectorPolicyWriteResponse is the Portal policy write answer
type ConnectorPolicyWriteResponse struct {
	Revision  string
	Effective ConnectorPolicyEffective
}

// Facet values a portal tool row may carry, the ConnectorTool facet Literal
var PortalFacets = []string{"read", "write", "destructive", "unclassified"}

// Value of a field read by its alias first and then by its name, as populate_by_name allows
func wireField(o *pyjson.Object, alias, name string) (any, bool) {
	if v, ok := o.Get(alias); ok {
		return v, true
	}
	if name != alias {
		return o.Get(name)
	}
	return nil, false
}

func wireStr(o *pyjson.Object, alias, name string) (string, error) {
	v, ok := wireField(o, alias, name)
	if !ok {
		return "", &wireError{name + ": Field required"}
	}
	s, isStr := v.(string)
	if !isStr {
		return "", &wireError{name + ": Input should be a valid string"}
	}
	return s, nil
}

func wireBool(o *pyjson.Object, alias, name string) (bool, error) {
	v, ok := wireField(o, alias, name)
	if !ok {
		return false, &wireError{name + ": Field required"}
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, &wireError{name + ": Input should be a valid boolean"}
	}
	return b, nil
}

// Strict int of a field, rejecting floats and bools like the strict model
func wireInt(o *pyjson.Object, alias, name string) (int64, error) {
	v, ok := wireField(o, alias, name)
	if !ok {
		return 0, &wireError{name + ": Field required"}
	}
	switch n := v.(type) {
	case int64:
		return n, nil
	case *big.Int:
		if n.IsInt64() {
			return n.Int64(), nil
		}
	}
	return 0, &wireError{name + ": Input should be a valid integer"}
}

func wireStrListAt(v any, loc string) ([]string, error) {
	items, ok := v.([]any)
	if !ok {
		return nil, &wireError{loc + ": Input should be a valid list"}
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, isStr := item.(string)
		if !isStr {
			return nil, &wireError{fmt.Sprintf("%s.%d: Input should be a valid string", loc, i)}
		}
		out = append(out, s)
	}
	return out, nil
}

func wireReqStrList(o *pyjson.Object, alias, name string) ([]string, error) {
	v, ok := wireField(o, alias, name)
	if !ok {
		return nil, &wireError{name + ": Field required"}
	}
	return wireStrListAt(v, name)
}

// Optional list of strings, nil for a missing key or None
func wireOptStrList(o *pyjson.Object, name string) ([]string, error) {
	v, ok := o.Get(name)
	if !ok || v == nil {
		return nil, nil
	}
	return wireStrListAt(v, name)
}

func wireObj(v any, loc string) (*pyjson.Object, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, &wireError{loc + ": Input should be a valid dictionary or instance of the model"}
	}
	return o, nil
}

// Facet value with any string outside the four known facets read as unclassified, other values left to fail
func unknownFacetIsUnclassified(value any) any {
	s, ok := value.(string)
	if !ok {
		return value
	}
	if slices.Contains(PortalFacets, s) {
		return s
	}
	return "unclassified"
}

// Categories lowercased so filters match regardless of how the Portal cased them
func normalizeCategories(value []string) []string {
	out := make([]string, len(value))
	for i, category := range value {
		out[i] = pystr.Lower(category)
	}
	return out
}

func wireConnectorTool(o *pyjson.Object) (ConnectorTool, error) {
	var t ConnectorTool
	var err error
	if t.Slug, err = wireStr(o, "slug", "slug"); err != nil {
		return t, err
	}
	if t.Name, err = wireStr(o, "name", "name"); err != nil {
		return t, err
	}
	if t.Description, err = wireStr(o, "description", "description"); err != nil {
		return t, err
	}
	facet, ok := o.Get("facet")
	if !ok {
		return t, &wireError{"facet: Field required"}
	}
	s, isStr := unknownFacetIsUnclassified(facet).(string)
	if !isStr {
		return t, &wireError{"facet: Input should be 'read', 'write', 'destructive' or 'unclassified'"}
	}
	t.Facet = s
	if t.Hints, err = wireReqStrList(o, "hints", "hints"); err != nil {
		return t, err
	}
	if t.Categories, err = wireReqStrList(o, "categories", "categories"); err != nil {
		return t, err
	}
	t.Categories = normalizeCategories(t.Categories)
	if t.NoAuth, err = wireBool(o, "noAuth", "no_auth"); err != nil {
		return t, err
	}
	t.Deprecated, err = wireBool(o, "deprecated", "deprecated")
	return t, err
}

func wireToolsListing(o *pyjson.Object) (any, error) {
	var l ConnectorToolsListing
	var err error
	if l.Connector, err = wireStr(o, "connector", "connector"); err != nil {
		return nil, err
	}
	if l.ToolkitVersion, err = wireStr(o, "toolkitVersion", "toolkit_version"); err != nil {
		return nil, err
	}
	if l.Etag, err = wireStr(o, "etag", "etag"); err != nil {
		return nil, err
	}
	raw, ok := o.Get("tools")
	if !ok {
		return nil, &wireError{"tools: Field required"}
	}
	items, isList := raw.([]any)
	if !isList {
		return nil, &wireError{"tools: Input should be a valid list"}
	}
	l.Tools = make([]ConnectorTool, 0, len(items))
	for i, item := range items {
		to, err := wireObj(item, fmt.Sprintf("tools.%d", i))
		if err != nil {
			return nil, err
		}
		tool, err := wireConnectorTool(to)
		if err != nil {
			return nil, err
		}
		l.Tools = append(l.Tools, tool)
	}
	return l, nil
}

func wireCatalog(o *pyjson.Object) (any, error) {
	raw, ok := o.Get("connectors")
	if !ok {
		return nil, &wireError{"connectors: Field required"}
	}
	items, isList := raw.([]any)
	if !isList {
		return nil, &wireError{"connectors: Input should be a valid list"}
	}
	out := ConnectorCatalogResponse{Connectors: make([]ConnectorCatalogRow, 0, len(items))}
	for i, item := range items {
		ro, err := wireObj(item, fmt.Sprintf("connectors.%d", i))
		if err != nil {
			return nil, err
		}
		var row ConnectorCatalogRow
		for _, f := range []struct {
			key string
			dst *string
		}{{"slug", &row.Slug}, {"name", &row.Name}, {"description", &row.Description}, {"category", &row.Category}} {
			if *f.dst, err = wireStr(ro, f.key, f.key); err != nil {
				return nil, err
			}
		}
		if logo, has := wireField(ro, "logoUrl", "logo_url"); has && logo != nil {
			s, isStr := logo.(string)
			if !isStr {
				return nil, &wireError{"logo_url: Input should be a valid string"}
			}
			row.LogoURL = &s
		}
		out.Connectors = append(out.Connectors, row)
	}
	return out, nil
}

// Tool rules keyed by connector in payload order, an empty map when the key is absent
func wireToolRules(o *pyjson.Object) ([]string, map[string]PolicyToolRule, error) {
	rules := map[string]PolicyToolRule{}
	raw, ok := o.Get("tools")
	if !ok {
		return []string{}, rules, nil
	}
	ro, err := wireObj(raw, "tools")
	if err != nil {
		return nil, nil, err
	}
	for _, key := range ro.Keys {
		rule, err := wireObj(ro.Vals[key], "tools."+key)
		if err != nil {
			return nil, nil, err
		}
		disable := []string{}
		if v, has := rule.Get("disable"); has {
			if disable, err = wireStrListAt(v, "tools."+key+".disable"); err != nil {
				return nil, nil, err
			}
		}
		rules[key] = PolicyToolRule{Disable: disable}
	}
	return slices.Clone(ro.Keys), rules, nil
}

// Policy body discriminated by mode, an unknown mode failing validation
func wirePolicyBody(o *pyjson.Object) (PolicyBody, error) {
	mode, ok := o.Get("mode")
	if !ok {
		return PolicyBody{}, &wireError{"Unable to extract tag using discriminator 'mode'"}
	}
	s, _ := mode.(string)
	switch s {
	case "unrestricted":
		return wireUnrestrictedPolicyBody(), nil
	case "deny-all":
		return wireDenyAllPolicyBody(), nil
	case "allow":
		return wireAllowPolicyBody(o)
	case "deny":
		return wireDenyPolicyBody(o)
	}
	return PolicyBody{}, &wireError{fmt.Sprintf("Input tag %v found using 'mode' does not match any of the expected tags", mode)}
}

// Body leaving every connector and tool enabled, only its mode carried
func wireUnrestrictedPolicyBody() PolicyBody { return PolicyBody{Mode: "unrestricted"} }

// Body disabling every connector, only its mode carried
func wireDenyAllPolicyBody() PolicyBody { return PolicyBody{Mode: "deny-all"} }

// Body enabling only the listed connectors with their per connector tool rules and optional tags
func wireAllowPolicyBody(o *pyjson.Object) (PolicyBody, error) {
	b := PolicyBody{Mode: "allow"}
	var err error
	if b.Connectors, err = wireReqStrList(o, "connectors", "connectors"); err != nil {
		return b, err
	}
	return wirePolicyRules(o, b)
}

// Body disabling the listed connectors with their per connector tool rules and optional tags
func wireDenyPolicyBody(o *pyjson.Object) (PolicyBody, error) {
	b := PolicyBody{Mode: "deny"}
	var err error
	if b.DisabledConnectors, err = wireReqStrList(o, "disabledConnectors", "disabled_connectors"); err != nil {
		return b, err
	}
	return wirePolicyRules(o, b)
}

// Tool rules and tags an allow or deny body carries, the tool rules empty and the tags none when absent
func wirePolicyRules(o *pyjson.Object, b PolicyBody) (PolicyBody, error) {
	var err error
	if b.ToolKeys, b.Tools, err = wireToolRules(o); err != nil {
		return b, err
	}
	if raw, has := o.Get("tags"); has && raw != nil {
		to, err := wireObj(raw, "tags")
		if err != nil {
			return b, err
		}
		var tags PolicyTags
		if tags.Enable, err = wireOptStrList(to, "enable"); err != nil {
			return b, err
		}
		if tags.Disable, err = wireOptStrList(to, "disable"); err != nil {
			return b, err
		}
		b.Tags = &tags
	}
	return b, nil
}

func wireEffective(raw any) (ConnectorPolicyEffective, error) {
	var e ConnectorPolicyEffective
	o, err := wireObj(raw, "effective")
	if err != nil {
		return e, err
	}
	if e.PolicyBody, err = wirePolicyBody(o); err != nil {
		return e, err
	}
	if e.Version, err = wireInt(o, "version", "version"); err != nil {
		return e, err
	}
	if e.Version != 1 {
		return e, &wireError{"version: Input should be 1"}
	}
	if e.Revision, err = wireStr(o, "revision", "revision"); err != nil {
		return e, err
	}
	e.IssuedAtMs, err = wireInt(o, "issuedAtMs", "issued_at_ms")
	return e, err
}

func wirePolicyResponse(o *pyjson.Object) (any, error) {
	raw, ok := o.Get("layers")
	if !ok {
		return nil, &wireError{"layers: Field required"}
	}
	items, isList := raw.([]any)
	if !isList {
		return nil, &wireError{"layers: Input should be a valid list"}
	}
	out := ConnectorPolicyResponse{Layers: make([]ConnectorPolicyLayer, 0, len(items))}
	for i, item := range items {
		lo, err := wireObj(item, fmt.Sprintf("layers.%d", i))
		if err != nil {
			return nil, err
		}
		var layer ConnectorPolicyLayer
		if layer.Kind, err = wireStr(lo, "kind", "kind"); err != nil {
			return nil, err
		}
		if !slices.Contains([]string{"org", "role", "member"}, layer.Kind) {
			return nil, &wireError{"kind: Input should be 'org', 'role' or 'member'"}
		}
		if id, has := lo.Get("id"); has && id != nil {
			s, isStr := id.(string)
			if !isStr {
				return nil, &wireError{"id: Input should be a valid string"}
			}
			layer.ID = &s
		}
		body, has := lo.Get("body")
		if !has {
			return nil, &wireError{"body: Field required"}
		}
		bo, err := wireObj(body, "body")
		if err != nil {
			return nil, err
		}
		if layer.Body, err = wirePolicyBody(bo); err != nil {
			return nil, err
		}
		if layer.Revision, err = wireStr(lo, "revision", "revision"); err != nil {
			return nil, err
		}
		out.Layers = append(out.Layers, layer)
	}
	effective, ok := o.Get("effective")
	if !ok {
		return nil, &wireError{"effective: Field required"}
	}
	var err error
	out.Effective, err = wireEffective(effective)
	return out, err
}

func wirePolicyWrite(o *pyjson.Object) (any, error) {
	var out ConnectorPolicyWriteResponse
	var err error
	if out.Revision, err = wireStr(o, "revision", "revision"); err != nil {
		return nil, err
	}
	effective, ok := o.Get("effective")
	if !ok {
		return nil, &wireError{"effective: Field required"}
	}
	out.Effective, err = wireEffective(effective)
	return out, err
}

// Validates a 2xx body loaded the json.loads way through model, raising kind's INVALID_RESPONSE error
func (c *PortalConnectorClient) parseModel(resp *http.Response, status int, route, kind, message string, model func(*pyjson.Object) (any, error)) (any, error) {
	requestID := responseRequestID(resp)
	parsed, err := portalModel(resp, model)
	if err != nil {
		logConnectorOutcome(c.env, route, status, true, "INVALID_RESPONSE", requestID, nil)
		return nil, &ToolGatewayError{Message: message, Code: "INVALID_RESPONSE", Status: status, RequestID: requestID, Kind: kind}
	}
	logConnectorOutcome(c.env, route, status, true, "", requestID, nil)
	return parsed, nil
}

func portalModel(resp *http.Response, model func(*pyjson.Object) (any, error)) (any, error) {
	if resp == nil || resp.Body == nil {
		return nil, &wireError{"no body"}
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	payload, err := pyjson.LoadsBytes(data)
	if err != nil {
		return nil, err
	}
	o, ok := payload.(*pyjson.Object)
	if !ok {
		return nil, &wireError{"response must be an object"}
	}
	return model(o)
}

// Lists one connector's tools, a 304 answering PortalNotModified and every unavailable answer PortalToolsUnavailable
func (c *PortalConnectorClient) Tools(slug, ifNoneMatch string) (any, error) {
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}
	var extra map[string]string
	if ifNoneMatch != "" {
		extra = map[string]string{"If-None-Match": ifNoneMatch}
	}
	resp, status, route, err := c.request("GET", "/api/v1/connectors/"+slug+"/tools", nil, extra, false)
	if err != nil {
		gerr, ok := err.(*ToolGatewayError)
		if !ok {
			return nil, err
		}
		missingRoute := gerr.Kind == gatewayKindUnavailable && gerr.Code != "connector_not_found"
		if gerr.Kind != "" && !missingRoute {
			return nil, err
		}
		return nil, &ToolGatewayError{Message: "portal tools unavailable", Code: gerr.Code, Status: gerr.Status, RequestID: gerr.RequestID,
			Retryable: gerr.Retryable, Kind: PortalKindToolsUnavailable}
	}
	if status == 304 {
		return PortalNotModified{}, nil
	}
	parsed, err := c.parseModel(resp, status, route, PortalKindToolsUnavailable, "portal tools unavailable", wireToolsListing)
	if err != nil {
		return nil, err
	}
	listing := parsed.(ConnectorToolsListing)
	if etag := resp.Header.Get("Etag"); etag != "" {
		listing.Etag = etag
	}
	return listing, nil
}

// Reads the connector catalog of the authorized caller
func (c *PortalConnectorClient) Catalog() (ConnectorCatalogResponse, error) {
	return c.catalogAt("/api/v1/connectors/catalog", false)
}

// Reads the catalog a new guest would get, sent without a bearer so every caller shares one cached answer
func (c *PortalConnectorClient) PublicCatalog() (ConnectorCatalogResponse, error) {
	return c.catalogAt("/api/v1/connectors/catalog/public", true)
}

func (c *PortalConnectorClient) catalogAt(path string, public bool) (ConnectorCatalogResponse, error) {
	resp, status, route, err := c.request("GET", path, nil, nil, public)
	if err != nil {
		return ConnectorCatalogResponse{}, err
	}
	parsed, err := c.parseModel(resp, status, route, PortalKindConnectorUnavailable, "portal catalog unavailable", wireCatalog)
	if err != nil {
		return ConnectorCatalogResponse{}, err
	}
	return parsed.(ConnectorCatalogResponse), nil
}

// Reads the connector policy layers and the effective policy
func (c *PortalConnectorClient) Policy() (ConnectorPolicyResponse, error) {
	resp, status, route, err := c.request("GET", "/api/v1/connectors/policy", nil, nil, false)
	if err != nil {
		return ConnectorPolicyResponse{}, err
	}
	parsed, err := c.parseModel(resp, status, route, PortalKindConnectorUnavailable, "portal policy unavailable", wirePolicyResponse)
	if err != nil {
		return ConnectorPolicyResponse{}, err
	}
	return parsed.(ConnectorPolicyResponse), nil
}

// Writes the connector policy body, answering the new revision and effective policy
func (c *PortalConnectorClient) SetPolicy(body map[string]any) (ConnectorPolicyWriteResponse, error) {
	resp, status, route, err := c.request("PUT", "/api/v1/connectors/policy", body, nil, false)
	if err != nil {
		return ConnectorPolicyWriteResponse{}, err
	}
	parsed, err := c.parseModel(resp, status, route, PortalKindConnectorUnavailable, "portal policy unavailable", wirePolicyWrite)
	if err != nil {
		return ConnectorPolicyWriteResponse{}, err
	}
	return parsed.(ConnectorPolicyWriteResponse), nil
}
