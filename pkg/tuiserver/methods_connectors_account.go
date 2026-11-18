package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Account handler body over the validated request
type accountBody func(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error)

// Validation, identity and failure mapping of one account owned connectors page method
type accountMethodSpec struct {
	model              *pydantic.Model
	createsGuest       bool
	invalid            string
	unavailable        string
	unavailableMessage string
}

var connectorsAccountRegistry = &handlerRegistry{}

func init() {
	accounts := accountMethodSpec{model: tuigateway.ConnectorAccountsParams, invalid: "connector must be a slug.",
		unavailable: "accounts_unavailable", unavailableMessage: "Connector accounts are unavailable."}
	connectorsAccountRegistry.scopedMethod("connectors.accounts", accountMethod(accounts, connectorsAccounts))
	rename := accounts
	rename.model, rename.createsGuest, rename.invalid = tuigateway.ConnectorAccountsRenameParams, true, "connection_id and a valid alias are required."
	connectorsAccountRegistry.scopedMethod("connectors.accounts.rename", accountMethod(rename, connectorsAccountsRename))
	remove := accounts
	remove.model, remove.createsGuest, remove.invalid = tuigateway.ConnectorAccountsRemoveParams, true, "connection_id is required."
	connectorsAccountRegistry.scopedMethod("connectors.accounts.remove", accountMethod(remove, connectorsAccountsRemove))
	connectorsAccountRegistry.install()
	addLongHandlers(connectorsAccountRegistry.names()...)
}

// Gateway error of err, nil for any other error
func gatewayErrorOf(err error) *tools.ToolGatewayError {
	var gatewayErr *tools.ToolGatewayError
	if errors.As(err, &gatewayErr) {
		return gatewayErr
	}
	return nil
}

// Whether a raised failure is a config backend outage, the one failure an account method lets through
func accountOutage(failure any) bool {
	err, isError := failure.(error)
	if !isError {
		return false
	}
	var backendErr *config.ConfigBackendUnavailable
	var sourceErr *provider.ConfigBackendUnavailableError
	return errors.As(err, &backendErr) || errors.As(err, &sourceErr)
}

// Refusal of a failed account body, an auth failure by its code and org_required asking for an organization
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

// Handler binding the page variables, gating availability, validating, creating the guest and mapping failures
func accountMethod(spec accountMethodSpec, body accountBody) Handler {
	return func(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
		accountPageVars(ctx, func(scoped context.Context, env *tools.Env) {
			if response = accountGateClosed(env, rid); response != nil {
				return
			}
			request, invalid := spec.model.Validate(params)
			if invalid != nil {
				response = connectorRPCError(rid, 4000, "invalid_params", spec.invalid)
				return
			}
			if spec.createsGuest {
				if response = accountIdentityError(env, rid); response != nil {
					return
				}
			}
			response = runAccountBody(scoped, env, rid, request, spec, body)
		})
		return response
	}
}

// Runs one account body, any raised failure other than a config outage answered as its refusal
func runAccountBody(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance, spec accountMethodSpec, body accountBody) (response map[string]any) {
	defer func() {
		failure := recover()
		if failure == nil {
			return
		}
		if accountOutage(failure) {
			panic(failure)
		}
		response = accountFailure(rid, spec, failure)
	}()
	response, err := body(ctx, env, rid, request)
	if err != nil {
		if accountOutage(err) {
			panic(err)
		}
		return accountFailure(rid, spec, err)
	}
	return response
}

// Plain dump of a contract model validated from fields, a validation error returned as the failure
func contractDump(model *pydantic.Model, fields map[string]any) (map[string]any, error) {
	instance, invalid := model.Validate(fields)
	if invalid != nil {
		return nil, invalid
	}
	return pyjson.Plain(instance.Dump(pydantic.DumpOptions{})).(map[string]any), nil
}

// Contract fields copied from the Portal wire keys of a dump, a missing key raised as a KeyError
func wireFields(dump map[string]any, names map[string]string) (map[string]any, error) {
	fields := map[string]any{}
	for wire, name := range names {
		value, present := dump[wire]
		if !present {
			return nil, fmt.Errorf("KeyError: '%s'", wire)
		}
		fields[name] = value
	}
	return fields, nil
}

// Contract row of one Portal account dump
func accountRow(account map[string]any) (map[string]any, error) {
	fields, err := wireFields(account, map[string]string{"connectionId": "connection_id", "connector": "connector", "status": "status",
		"label": "label", "active": "active", "createdAt": "created_at", "updatedAt": "updated_at"})
	if err != nil {
		return nil, err
	}
	fields["status_reason"], fields["alias"], fields["disabled"] = account["statusReason"], account["alias"], truthy(account["disabled"])
	return contractDump(tuigateway.ConnectorAccountRow, fields)
}

// The member's Portal accounts, filtered to one connector when asked, none without a bearer
func connectorsAccounts(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	accounts := []map[string]any{}
	if tools.ReadNousAccessToken(ctx, env) != "" {
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
func accountWriteRefusal(rid any, err error, invalid func(*tools.ToolGatewayError) bool, invalidMessage string) map[string]any {
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
	if invalid(gatewayErr) {
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
		badRequest := func(gatewayErr *tools.ToolGatewayError) bool { return gatewayErr.Status == 400 }
		if refusal := accountWriteRefusal(rid, err, badRequest, "That name is invalid."); refusal != nil {
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
		invalidID := func(gatewayErr *tools.ToolGatewayError) bool { return gatewayErr.Code == "invalid_connection_id" }
		if refusal := accountWriteRefusal(rid, err, invalidID, "Connection id is invalid."); refusal != nil {
			return refusal, nil
		}
		return nil, err
	}
	fields, err := wireFields(removed, map[string]string{"connectionId": "connection_id", "connector": "connector", "status": "status"})
	if err != nil {
		return nil, err
	}
	result, err := contractDump(tuigateway.ConnectorAccountsRemoveResult, fields)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}
