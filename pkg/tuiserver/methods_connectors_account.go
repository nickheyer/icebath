package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Connectors page handler body over the validated request, nil when the method takes no contract
type accountBody func(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error)

// Gate, validation, identity and failure mapping of one account owned Connectors page method
type accountMethodSpec struct {
	model              *pydantic.Model
	createsGuest       bool
	invalid            string
	invalidReason      string
	unavailable        string
	unavailableMessage string
}

var connectorsAccountRegistry = &handlerRegistry{}

func init() {
	connectorsAccountRegistry.scopedMethod("connectors.tools", accountMethod(accountMethodSpec{model: tuigateway.ConnectorToolsParams,
		createsGuest: true, invalid: "slug and refresh are required parameters", unavailable: "tools_unavailable",
		unavailableMessage: "Connector tools are unavailable."}, connectorsTools))
	connectorsAccountRegistry.scopedMethod("connectors.catalog", accountMethod(accountMethodSpec{unavailable: "catalog_unavailable",
		unavailableMessage: "Connector catalog is unavailable."}, connectorsCatalog))
	connectorsAccountRegistry.scopedMethod("connectors.accounts", accountMethod(accountMethodSpec{model: tuigateway.ConnectorAccountsParams,
		invalid: "connector must be a slug.", unavailable: "accounts_unavailable",
		unavailableMessage: "Connector accounts are unavailable."}, connectorsAccounts))
	connectorsAccountRegistry.scopedMethod("connectors.accounts.rename", accountMethod(accountMethodSpec{model: tuigateway.ConnectorAccountsRenameParams,
		createsGuest: true, invalid: "connection_id and a valid alias are required.", unavailable: "accounts_unavailable",
		unavailableMessage: "Connector accounts are unavailable."}, connectorsAccountsRename))
	connectorsAccountRegistry.scopedMethod("connectors.accounts.remove", accountMethod(accountMethodSpec{model: tuigateway.ConnectorAccountsRemoveParams,
		createsGuest: true, invalid: "connection_id is required.", unavailable: "accounts_unavailable",
		unavailableMessage: "Connector accounts are unavailable."}, connectorsAccountsRemove))
	connectorsAccountRegistry.scopedMethod("connectors.policy.get", accountMethod(accountMethodSpec{unavailable: "policy_unavailable",
		unavailableMessage: "Connector policy is unavailable."}, connectorsPolicyGet))
	connectorsAccountRegistry.scopedMethod("connectors.policy.set", accountMethod(accountMethodSpec{model: tuigateway.ConnectorPolicySetParams,
		createsGuest: true, invalid: "Connector parameters are invalid.", unavailable: "policy_unavailable",
		unavailableMessage: "Connector policy is unavailable."}, connectorsPolicySet))
	connectorsAccountRegistry.install()
	addLongHandlers(connectorsAccountRegistry.names()...)
}

// Gateway error of err with its kind, nil for any other error
func gatewayErrorOf(err error) *tools.ToolGatewayError {
	var gatewayErr *tools.ToolGatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr
	}
	return nil
}

// Failure of an account method body as its refusal, an auth failure by code and org_required asking for an organization
func accountFailure(rid any, spec accountMethodSpec, failure any) map[string]any {
	err, _ := failure.(error)
	if gatewayErr := gatewayErrorOf(err); gatewayErr != nil {
		if gatewayErr.Kind == "auth" {
			return connectorAuthError(rid, gatewayErr)
		}
		if gatewayErr.Code == "org_required" {
			return connectorRPCError(rid, 4090, "org_required", "Select an organization to manage connector rules.")
		}
	}
	return connectorRPCError(rid, 5034, spec.unavailable, spec.unavailableMessage)
}

// Handler binding the Connectors page variables, gating availability and mapping the body's failures to refusals
func accountMethod(spec accountMethodSpec, body accountBody) Handler {
	if spec.invalidReason == "" {
		spec.invalidReason = "invalid_params"
	}
	return func(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
		desktopPageVars(ctx, func(scoped context.Context, env *tools.Env) {
			if gateError := accountGateClosed(env, rid); gateError != nil {
				response = gateError
				return
			}
			var request *pydantic.Instance
			if spec.model != nil {
				validated, invalid := spec.model.Validate(params)
				if invalid != nil {
					response = connectorRPCError(rid, 4000, spec.invalidReason, spec.invalid)
					return
				}
				request = validated
			}
			if spec.createsGuest {
				if identityError := accountIdentityError(env, rid); identityError != nil {
					response = identityError
					return
				}
			}
			response = runAccountBody(scoped, env, rid, request, spec, body)
		})
		return response
	}
}

// Runs one account body, a raised failure other than a profile or config outage answered as its refusal
func runAccountBody(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance, spec accountMethodSpec, body accountBody) (response map[string]any) {
	defer func() {
		failure := recover()
		if failure == nil {
			return
		}
		if connectorOutage(failure) {
			panic(failure)
		}
		response = accountFailure(rid, spec, failure)
	}()
	response, err := body(ctx, env, rid, request)
	if err != nil {
		if connectorOutage(err) {
			panic(err)
		}
		return accountFailure(rid, spec, err)
	}
	return response
}

// Plain dump of a contract model validated from fields, the validation error raised as a failure
func contractDump(model *pydantic.Model, fields map[string]any) (map[string]any, error) {
	instance, invalid := model.Validate(fields)
	if invalid != nil {
		return nil, invalid
	}
	return pyjson.Plain(instance.Dump(pydantic.DumpOptions{})).(map[string]any), nil
}

func anyStrings(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// Tool list of one connector from the member's cache or the Portal, an unknown connector answered as not found
func connectorsTools(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	client := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{})
	if err := client.RequireAuthentication(); err != nil {
		return nil, err
	}
	listing, err := tools.ReadTools(ctx, env, connectorText(request, "slug"), client, truthy(request.Get("refresh")), nil)
	if err != nil {
		var invalidSlug *tools.InvalidConnectorSlug
		if gatewayErr := gatewayErrorOf(err); errors.As(err, &invalidSlug) || gatewayErr != nil && gatewayErr.Kind == "unavailable" {
			return connectorRPCError(rid, 4041, "connector_not_found", "Connector not found."), nil
		}
		return nil, err
	}
	rows := make([]any, 0, len(listing.Tools))
	for _, tool := range listing.Tools {
		rows = append(rows, map[string]any{"slug": tool.Slug, "name": tool.Name, "description": tool.Description, "facet": tool.Facet,
			"hints": anyStrings(tool.Hints), "categories": anyStrings(tool.Categories), "no_auth": tool.NoAuth, "deprecated": tool.Deprecated})
	}
	result, err := contractDump(tuigateway.ConnectorToolsResult, map[string]any{"connector": listing.Connector,
		"toolkit_version": listing.ToolkitVersion, "etag": listing.Etag, "fetched_at": listing.FetchedAt, "source": listing.Source,
		"stale": listing.Stale, "tools": rows})
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Signed in catalog with a bearer, else the public catalog a new guest would get
func connectorsCatalog(ctx context.Context, env *tools.Env, rid any, _ *pydantic.Instance) (map[string]any, error) {
	client := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{})
	var catalog tools.ConnectorCatalogResponse
	var err error
	if tools.ReadNousAccessToken(env) != "" {
		catalog, err = client.Catalog()
	} else {
		catalog, err = client.PublicCatalog()
	}
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(catalog.Connectors))
	for _, row := range catalog.Connectors {
		var logo any
		if row.LogoURL != nil {
			logo = *row.LogoURL
		}
		rows = append(rows, map[string]any{"slug": row.Slug, "name": row.Name, "description": row.Description, "category": row.Category, "logo_url": logo})
	}
	result, err := contractDump(tuigateway.ConnectorsCatalogResult, map[string]any{"connectors": rows})
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Contract row of one Portal account dump
func accountRow(account map[string]any) (map[string]any, error) {
	fields := map[string]any{"status_reason": account["statusReason"], "alias": account["alias"], "disabled": truthy(account["disabled"])}
	for wire, name := range map[string]string{"connectionId": "connection_id", "connector": "connector", "status": "status",
		"label": "label", "active": "active", "createdAt": "created_at", "updatedAt": "updated_at"} {
		value, present := account[wire]
		if !present {
			return nil, fmt.Errorf("KeyError: '%s'", wire)
		}
		fields[name] = value
	}
	return contractDump(tuigateway.ConnectorAccountRow, fields)
}

// The member's Portal accounts, filtered to one connector when asked, none without a bearer
func connectorsAccounts(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	accounts := []map[string]any{}
	if tools.ReadNousAccessToken(env) != "" {
		listed, err := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{}).ListAccounts()
		if err != nil {
			return nil, err
		}
		accounts = listed
	}
	rows := []any{}
	for _, account := range accounts {
		if connector := request.Get("connector"); connector != nil && account["connector"] != connector {
			continue
		}
		row, err := accountRow(account)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	result, err := contractDump(tuigateway.ConnectorAccountsResult, map[string]any{"accounts": rows})
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Refusal of an account write the Portal failed, nil when the failure belongs to the outer mapping
func accountWriteRefusal(rid any, err error, invalidCode, invalidMessage string, status400 bool) map[string]any {
	gatewayErr := gatewayErrorOf(err)
	if gatewayErr == nil {
		return nil
	}
	switch gatewayErr.Kind {
	case "unavailable":
		if gatewayErr.Code == "connection_not_found" {
			return connectorRPCError(rid, 4041, "connection_not_found", "Connector account not found.")
		}
		return connectorRPCError(rid, 5034, "accounts_unavailable", "Connector accounts are unavailable.")
	case "auth":
		return nil
	}
	if gatewayErr.Code == "org_required" {
		return nil
	}
	if (status400 && gatewayErr.Status == 400) || (!status400 && gatewayErr.Code == invalidCode) {
		return connectorRPCError(rid, 4000, "invalid_params", invalidMessage)
	}
	return connectorRPCError(rid, 5034, "accounts_unavailable", "Connector accounts are unavailable.")
}

// Names one Portal account, a name another account holds answered as alias taken
func connectorsAccountsRename(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	account, err := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{}).RenameAccount(connectorText(request, "connection_id"), connectorText(request, "alias"))
	if err != nil {
		if gatewayErr := gatewayErrorOf(err); gatewayErr != nil && gatewayErr.Kind == "idempotency" {
			return connectorRPCError(rid, 4090, "alias_taken", "That name is already used."), nil
		}
		if refusal := accountWriteRefusal(rid, err, "", "That name is invalid.", true); refusal != nil {
			return refusal, nil
		}
		return nil, err
	}
	row, err := accountRow(account)
	if err != nil {
		return nil, err
	}
	return ok(rid, row), nil
}

// Removes one Portal account and answers the removed row
func connectorsAccountsRemove(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	removed, err := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{}).DeleteAccount(connectorText(request, "connection_id"))
	if err != nil {
		if refusal := accountWriteRefusal(rid, err, "invalid_connection_id", "Connection id is invalid.", false); refusal != nil {
			return refusal, nil
		}
		return nil, err
	}
	fields := map[string]any{}
	for wire, name := range map[string]string{"connectionId": "connection_id", "connector": "connector", "status": "status"} {
		value, present := removed[wire]
		if !present {
			return nil, fmt.Errorf("KeyError: '%s'", wire)
		}
		fields[name] = value
	}
	result, err := contractDump(tuigateway.ConnectorAccountsRemoveResult, fields)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Refusal for an auth failure of a policy read or write
func policyAuthError(rid any, gatewayErr *tools.ToolGatewayError) map[string]any {
	switch {
	case gatewayErr.Code == "no_access" || gatewayErr.Code == "ORG_ACCESS_DENIED":
		return connectorRPCError(rid, 4030, "org_access_denied", "This account cannot manage connectors for this organization.")
	case gatewayErr.Code == "forbidden":
		return connectorRPCError(rid, 4030, "forbidden_scope", "Connector policy cannot be changed for this member.")
	case gatewayErr.Status == 401 || gatewayErr.Code == "invalid_token" || gatewayErr.Code == "INVALID_TOKEN" || gatewayErr.Code == "NO_TOKEN":
		return connectorRPCError(rid, 4032, "needs_nous_auth", "Sign in to use connectors.")
	}
	return connectorRPCError(rid, 5034, "policy_unavailable", "Connector policy is unavailable.")
}

// Refusal for a gateway failure of a policy read or write by its code and status
func policyError(rid any, gatewayErr *tools.ToolGatewayError) map[string]any {
	switch gatewayErr.Code {
	case "policy_changed":
		return connectorRPCError(rid, 4090, "policy_conflict", "Connector policy changed. Refresh and try again.")
	case "org_required":
		return connectorRPCError(rid, 4090, "org_required", "Select an organization to manage connector rules.")
	case "forbidden":
		return connectorRPCError(rid, 4030, "forbidden_scope", "Connector policy cannot be changed for this member.")
	case "no_access":
		return connectorRPCError(rid, 4030, "org_access_denied", "This account cannot manage connectors for this organization.")
	case "invalid_connector_policy":
		return connectorRPCError(rid, 4000, "invalid_policy", "Connector policy change is invalid.")
	}
	if gatewayErr.Code == fmt.Sprintf("HTTP_%d", gatewayErr.Status) && gatewayErr.Status == 400 {
		return connectorRPCError(rid, 4000, "invalid_policy", "Connector policy change is invalid.")
	}
	return connectorRPCError(rid, 5034, "policy_unavailable", "Connector policy is unavailable.")
}

// Refusal of a failed policy call, nil for a failure that is no gateway error
func policyFailure(rid any, err error) map[string]any {
	gatewayErr := gatewayErrorOf(err)
	if gatewayErr == nil {
		return nil
	}
	if gatewayErr.Kind == "auth" {
		return policyAuthError(rid, gatewayErr)
	}
	return policyError(rid, gatewayErr)
}

// Contract rule fields of an allow or deny policy body, none for unrestricted and deny-all
func policyRuleFields(body tools.PolicyBody) map[string]any {
	if body.Mode == "unrestricted" || body.Mode == "deny-all" {
		return map[string]any{}
	}
	var tags any
	if body.Tags != nil {
		var enable, disable any
		if body.Tags.Enable != nil {
			enable = anyStrings(body.Tags.Enable)
		}
		if body.Tags.Disable != nil {
			disable = anyStrings(body.Tags.Disable)
		}
		tags = map[string]any{"enable": enable, "disable": disable}
	}
	rules := pyjson.NewObject()
	for _, connector := range body.ToolKeys {
		rules.Set(connector, anyStrings(body.Tools[connector].Disable))
	}
	fields := map[string]any{"tools": rules, "tags": tags}
	if body.Mode == "allow" {
		fields["connectors"] = anyStrings(body.Connectors)
	} else {
		fields["disabled_connectors"] = anyStrings(body.DisabledConnectors)
	}
	return fields
}

// Contract fields of a policy body by its mode
func contractPolicyBody(body tools.PolicyBody) map[string]any {
	fields := policyRuleFields(body)
	fields["mode"] = body.Mode
	return fields
}

// Contract fields of the effective policy by its mode with its version, revision and issue time
func contractPolicyEffective(effective tools.ConnectorPolicyEffective) map[string]any {
	fields := policyRuleFields(effective.PolicyBody)
	fields["mode"], fields["version"], fields["revision"], fields["issued_at_ms"] = effective.Mode, effective.Version, effective.Revision, effective.IssuedAtMs
	return fields
}

// Authenticated Portal policy client and its current policy, the failure refused as a policy error
func readConnectorPolicy(ctx context.Context, env *tools.Env) (*tools.PortalConnectorClient, tools.ConnectorPolicyResponse, error) {
	client := tools.NewPortalConnectorClient(ctx, env, tools.PortalOptions{})
	if err := client.RequireAuthentication(); err != nil {
		return nil, tools.ConnectorPolicyResponse{}, err
	}
	policy, err := client.Policy()
	return client, policy, err
}

// Policy layers from organization to member and the effective policy
func connectorsPolicyGet(ctx context.Context, env *tools.Env, rid any, _ *pydantic.Instance) (map[string]any, error) {
	_, policy, err := readConnectorPolicy(ctx, env)
	if err != nil {
		if refusal := policyFailure(rid, err); refusal != nil {
			return refusal, nil
		}
		return nil, err
	}
	layers := make([]any, 0, len(policy.Layers))
	for _, layer := range policy.Layers {
		layers = append(layers, map[string]any{"kind": layer.Kind, "revision": layer.Revision, "body": contractPolicyBody(layer.Body)})
	}
	result, err := contractDump(tuigateway.ConnectorPolicyGetResult, map[string]any{"layers": layers, "effective": contractPolicyEffective(policy.Effective)})
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Applies one member connector or tool list change over the current member layer at the expected revision
func connectorsPolicySet(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	write, err := func() (tools.ConnectorPolicyWriteResponse, error) {
		client, policy, err := readConnectorPolicy(ctx, env)
		if err != nil {
			return tools.ConnectorPolicyWriteResponse{}, err
		}
		var member *tools.PolicyBody
		for i := range policy.Layers {
			if policy.Layers[i].Kind == "member" {
				member = &policy.Layers[i].Body
				break
			}
		}
		change, _ := request.Get("change").(*pydantic.Instance)
		var body map[string]any
		if change.Get("type") == "tools" {
			var disabled []string
			for _, item := range pyjson.Plain(change.Get("disabled_tools")).([]any) {
				disabled = append(disabled, item.(string))
			}
			body, err = tools.ComposeToolsWrite(member, connectorText(change, "connector"), disabled)
		} else {
			body, err = tools.ComposeConnectorWrite(member, connectorText(change, "connector"), change.Get("enabled") == true)
		}
		if err != nil {
			return tools.ConnectorPolicyWriteResponse{}, err
		}
		body["expectedRevision"] = request.Get("expected_revision")
		return client.SetPolicy(body)
	}()
	if err != nil {
		var invalidPolicy *tools.InvalidMemberPolicy
		if gatewayErr := gatewayErrorOf(err); gatewayErr != nil && gatewayErr.Kind == "auth" {
			return policyAuthError(rid, gatewayErr), nil
		}
		if errors.As(err, &invalidPolicy) {
			return connectorRPCError(rid, 4000, "invalid_policy", "Connector policy change is invalid."), nil
		}
		if refusal := policyFailure(rid, err); refusal != nil {
			return refusal, nil
		}
		return nil, err
	}
	result, err := contractDump(tuigateway.ConnectorPolicySetResult, map[string]any{"revision": write.Revision, "effective": contractPolicyEffective(write.Effective)})
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}
