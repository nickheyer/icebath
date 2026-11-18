package tuigateway

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConnectorSlug = pydantic.ConStr(pydantic.Pattern(`^[a-z0-9][a-z0-9_-]*$`))

var ConnectorAlias = pydantic.ConStr(pydantic.Pattern(`^[a-z0-9][a-z0-9-]{0,31}$`))

var ConnectorErrorReason = pydantic.Enum("ConnectorErrorReason", "invalid_params", "INVALID_PARAMS", "not_owner", "NOT_OWNER", "unsupported_runtime", "UNSUPPORTED_RUNTIME", "connector_request_failed", "CONNECTOR_REQUEST_FAILED", "invalid_connector_response", "INVALID_CONNECTOR_RESPONSE", "unknown_target", "UNKNOWN_TARGET", "link_still_valid", "LINK_STILL_VALID", "reissue_refused", "REISSUE_REFUSED", "unknown_operation", "UNKNOWN_OPERATION", "invalid_answer", "INVALID_ANSWER", "needs_nous_auth", "NEEDS_NOUS_AUTH", "connector_not_found", "CONNECTOR_NOT_FOUND", "tools_unavailable", "TOOLS_UNAVAILABLE", "connectors_unavailable", "CONNECTORS_UNAVAILABLE", "catalog_unavailable", "CATALOG_UNAVAILABLE", "accounts_unavailable", "ACCOUNTS_UNAVAILABLE", "connection_not_found", "CONNECTION_NOT_FOUND", "policy_unavailable", "POLICY_UNAVAILABLE", "policy_conflict", "POLICY_CONFLICT", "forbidden_scope", "FORBIDDEN_SCOPE", "org_required", "ORG_REQUIRED", "org_access_denied", "ORG_ACCESS_DENIED", "invalid_policy", "INVALID_POLICY", "alias_taken", "ALIAS_TAKEN")

var ConnectorsListParams = pydantic.Extend(ProfileParams, "ConnectorsListParams", ParamsConfig,
	pydantic.Req("owner", ConnectorOwner),
)

var ConnectorRow = pydantic.NewModel("ConnectorRow", ClosedConfig,
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Req("connected", pydantic.Bool),
	pydantic.Req("connection_status", pydantic.Nullable(pydantic.Literal("pending", "active", "failed", "expired", "revoked", "inactive"))),
	pydantic.Req("status_reason", pydantic.Nullable(pydantic.Str)),
	pydantic.Req("gateway_disabled_tools", pydantic.List(pydantic.Str)),
)

var ConnectorsListResult = pydantic.NewModel("ConnectorsListResult", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Req("connectors", pydantic.List(ConnectorRow)),
)

var _ = Method("connectors.list", ConnectorsListParams, ConnectorsListResult, "Connector catalog + connection state for one session or profile account owner.")

var ConnectorsConnectParams = pydantic.Extend(ProfileParams, "ConnectorsConnectParams", ParamsConfig,
	pydantic.Req("owner", ConnectorOwner),
	pydantic.Req("connectors", pydantic.List(ConnectorSlug, pydantic.MinLen(1))),
	pydantic.Def("reconnect", pydantic.Bool, false),
	pydantic.Def("alias", pydantic.Nullable(ConnectorAlias), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.ConStr(pydantic.MinLen(1))), nil),
).WithAfter(oneConnectorPerAlias)

var ConnectorsConnectResult = pydantic.Extend(ConnectionOperationStatus, "ConnectorsConnectResult", ClosedConfig,
	pydantic.Def("status", pydantic.Nullable(pydantic.Literal("initiated", "settled")), nil),
	pydantic.Def("note", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_connectors._reissue``: the operation the connect opened; ``status``/``note`` ride along from the tool result.")

var _ = Method("connectors.connect", ConnectorsConnectParams, ConnectorsConnectResult, "Start or re-initiate authorization for named connectors on a session or account operation.")

// Refuses an alias over several connectors and a connection_id outside a single connector reconnect
func oneConnectorPerAlias(r *pydantic.Instance) error {
	alias, _ := r.Get("alias").(string)
	connectors := len(r.Get("connectors").([]any))
	if r.Get("alias") != nil && connectors != 1 {
		return errors.New("alias names one account, so it takes exactly one connector")
	}
	if r.Get("connection_id") != nil && (r.Get("reconnect") != true || connectors != 1 || alias != "") {
		return errors.New("connection_id repairs one account: it needs reconnect, one connector and no alias")
	}
	return nil
}

var ConnectorAccountStatus = pydantic.Enum("ConnectorAccountStatus", "pending", "pending", "active", "active", "failed", "failed", "expired", "expired", "revoked", "revoked", "inactive", "inactive")

var ConnectorAccountsParams = pydantic.Extend(ProfileParams, "ConnectorAccountsParams", ParamsConfig,
	pydantic.Def("connector", pydantic.Nullable(ConnectorSlug), nil),
)

var ConnectorAccountRow = pydantic.NewModel("ConnectorAccountRow", ClosedConfig,
	pydantic.Req("connection_id", pydantic.Str),
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("status", ConnectorAccountStatus),
	pydantic.Def("status_reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("label", pydantic.Str),
	pydantic.Def("alias", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("active", pydantic.Bool),
	pydantic.Def("disabled", pydantic.Bool, false),
	pydantic.Req("created_at", pydantic.Str),
	pydantic.Req("updated_at", pydantic.Str),
)

var ConnectorAccountsResult = pydantic.NewModel("ConnectorAccountsResult", ClosedConfig,
	pydantic.Req("accounts", pydantic.List(ConnectorAccountRow)),
)

var _ = Method("connectors.accounts", ConnectorAccountsParams, ConnectorAccountsResult, "The scoped member's hosted connector accounts, optionally filtered by connector slug.")

var ConnectorAccountsRemoveParams = pydantic.Extend(ProfileParams, "ConnectorAccountsRemoveParams", ParamsConfig,
	pydantic.Req("connection_id", pydantic.ConStr(pydantic.MinLen(1))),
)

var ConnectorAccountsRemoveResult = pydantic.NewModel("ConnectorAccountsRemoveResult", ClosedConfig,
	pydantic.Req("connection_id", pydantic.Str),
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("status", pydantic.Literal("removed")),
)

var _ = Method("connectors.accounts.remove", ConnectorAccountsRemoveParams, ConnectorAccountsRemoveResult, "Remove one hosted connector account owned by the scoped member.")

var ConnectorAccountsRenameParams = pydantic.Extend(ProfileParams, "ConnectorAccountsRenameParams", ParamsConfig,
	pydantic.Req("connection_id", pydantic.ConStr(pydantic.MinLen(1))),
	pydantic.Req("alias", ConnectorAlias),
)

var _ = Method("connectors.accounts.rename", ConnectorAccountsRenameParams, ConnectorAccountRow, "Name or rename one hosted connector account owned by the scoped member.")
