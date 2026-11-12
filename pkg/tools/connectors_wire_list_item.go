package tools

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

// One toolkit row of the gateway connector list, validated by field name or wire alias
var ConnectorListItem = pydantic.NewModel("ConnectorListItem", pydantic.Config{Extra: pydantic.ExtraIgnore, PopulateByName: true},
	pydantic.Req("connector", pydantic.Str),
	pydantic.Def("enabled", pydantic.Bool, true),
	pydantic.Def("connected", pydantic.Bool, false),
	pydantic.Def("connection_status", pydantic.Nullable(pydantic.Literal("pending", "active", "failed", "expired", "revoked", "inactive")), nil).Alias("connectionStatus"),
	pydantic.Def("status_reason", pydantic.Nullable(pydantic.Str), nil).Alias("statusReason"),
	pydantic.Fac("gateway_disabled_tools", pydantic.List(pydantic.Str), pydantic.NewList).Alias("disabledTools"),
)

// AccountOperationMismatch is the ValueError of connectors that do not share one open account operation
type AccountOperationMismatch struct{ Msg string }

func (e *AccountOperationMismatch) Error() string { return e.Msg }
