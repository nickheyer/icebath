package tools

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Shared config of the gateway wire models, unknown keys ignored and every field accepted by name or alias
var wireModelConfig = pydantic.Config{Extra: pydantic.ExtraIgnore, PopulateByName: true}

// Surfaces a connect or execute call may ask the vendor done page to return to
var connectorReturnTarget = pydantic.Literal("hermes-desktop", "hermes-desktop-dev", "portal")

// Who asked for a connect so the gateway can attribute each minted link
var connectorInitiator = pydantic.Literal("agent_tool", "desktop_page", "card_retry")

// Operation id a caller sends, one to 128 characters
var connectorOp = pydantic.Nullable(pydantic.ConStr(pydantic.MinLen(1), pydantic.MaxLen(128)))

// One use case query of a connector search
var ConnectorSearchQuery = pydantic.NewModel("ConnectorSearchQuery", wireModelConfig,
	pydantic.Req("use_case", pydantic.Str).Alias("useCase"),
	pydantic.Def("known_fields", pydantic.Nullable(pydantic.Str), nil).Alias("knownFields"),
)

// Body of the connector search route
var ConnectorSearchRequest = pydantic.NewModel("ConnectorSearchRequest", wireModelConfig,
	pydantic.Req("queries", pydantic.List(ConnectorSearchQuery)),
)

// Input schema of one vendor tool slug
var ConnectorToolSchema = pydantic.NewModel("ConnectorToolSchema", wireModelConfig,
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("tool", pydantic.Str),
	pydantic.Def("description", pydantic.Str, ""),
	pydantic.Fac("input_schema", pydantic.Dict(pydantic.Any), pydantic.NewDict).Alias("inputSchema"),
)

// One query's answer of a connector search, correlated by array position
var ConnectorSearchResult = pydantic.NewModel("ConnectorSearchResult", wireModelConfig,
	pydantic.Req("index", pydantic.Int),
	pydantic.Def("use_case", pydantic.Str, "").Alias("useCase"),
	pydantic.Fac("tools", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Fac("related_tools", pydantic.List(pydantic.Str), pydantic.NewList).Alias("relatedTools"),
	pydantic.Fac("connectors", pydantic.List(pydantic.Str), pydantic.NewList),
	pydantic.Def("guidance", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plan_steps", pydantic.Nullable(pydantic.List(pydantic.Str)), nil).Alias("planSteps"),
	pydantic.Def("pitfalls", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

// Whether one connector a search names is connected
var ConnectorConnectionStatus = pydantic.NewModel("ConnectorConnectionStatus", wireModelConfig,
	pydantic.Req("connector", pydantic.Str),
	pydantic.Def("connected", pydantic.Bool, false),
	pydantic.Def("description", pydantic.Str, ""),
)

// Answer of the connector search route
var ConnectorSearchResponse = pydantic.NewModel("ConnectorSearchResponse", wireModelConfig,
	pydantic.Fac("results", pydantic.List(ConnectorSearchResult), pydantic.NewList),
	pydantic.Fac("schemas", pydantic.Dict(ConnectorToolSchema), pydantic.NewDict),
	pydantic.Fac("connections", pydantic.List(ConnectorConnectionStatus), pydantic.NewList),
	pydantic.Fac("next_steps", pydantic.List(pydantic.Str), pydantic.NewList).Alias("nextSteps"),
)

// Body of the connector schemas route
var ConnectorSchemasRequest = pydantic.NewModel("ConnectorSchemasRequest", wireModelConfig,
	pydantic.Req("tools", pydantic.List(pydantic.Str)),
)

// Answer of the connector schemas route
var ConnectorSchemasResponse = pydantic.NewModel("ConnectorSchemasResponse", wireModelConfig,
	pydantic.Fac("schemas", pydantic.Dict(ConnectorToolSchema), pydantic.NewDict),
	pydantic.Fac("not_found", pydantic.List(pydantic.Str), pydantic.NewList).Alias("notFound"),
	pydantic.Fac("suggestions", pydantic.Dict(pydantic.List(pydantic.Str)), pydantic.NewDict),
)

// Body of the presigned file upload route
var ConnectorFileUploadRequest = pydantic.NewModel("ConnectorFileUploadRequest", wireModelConfig,
	pydantic.Req("toolkit", pydantic.Str),
	pydantic.Req("tool", pydantic.Str),
	pydantic.Req("filename", pydantic.Str),
	pydantic.Req("mimetype", pydantic.Str),
	pydantic.Req("size", pydantic.Int),
)

// Value a file uploadable tool input takes on the wire
var ConnectorFileDescriptor = pydantic.NewModel("ConnectorFileDescriptor", wireModelConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("mimetype", pydantic.Str),
	pydantic.Req("s3key", pydantic.Str),
)

// Answer of the presigned file upload route
var ConnectorFileUploadResponse = pydantic.NewModel("ConnectorFileUploadResponse", wireModelConfig,
	pydantic.Req("upload_url", pydantic.Str).Alias("uploadUrl"),
	pydantic.Fac("headers", pydantic.Dict(pydantic.Str), pydantic.NewDict),
	pydantic.Req("descriptor", ConnectorFileDescriptor),
	pydantic.Req("expires_at", pydantic.Str).Alias("expiresAt"),
)

// Error of one connector call, any code string accepted so newer gateway codes still render
var ConnectorToolError = pydantic.NewModel("ConnectorToolError", wireModelConfig,
	pydantic.Req("code", pydantic.Str),
	pydantic.Req("message", pydantic.Str),
	pydantic.Def("connector", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connect_url", pydantic.Nullable(pydantic.Str), nil).Alias("connectUrl"),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil).Alias("connectionId"),
	pydantic.Def("hint", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("family", pydantic.Nullable(pydantic.Str), nil),
)

// Body of a non 2xx single call execute answer
var ConnectorExecuteFailure = pydantic.Extend(ConnectorToolError, "ConnectorExecuteFailure", wireModelConfig,
	pydantic.Def("request_id", pydantic.Nullable(pydantic.Str), nil).Alias("requestId"),
)

// One call of an execute request, the account left for the gateway to resolve from connector_alias
var ConnectorExecuteCall = pydantic.NewModel("ConnectorExecuteCall", wireModelConfig,
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("tool", pydantic.Str),
	pydantic.Fac("arguments", pydantic.Dict(pydantic.Any), pydantic.NewDict),
	pydantic.Def("account", pydantic.Nullable(pydantic.Str), nil),
)

// Array form execute body an older gateway accepts
var ConnectorExecuteRequest = pydantic.NewModel("ConnectorExecuteRequest", wireModelConfig,
	pydantic.Req("tools", pydantic.List(ConnectorExecuteCall)),
	pydantic.Def("return_to", pydantic.Nullable(connectorReturnTarget), nil).Alias("returnTo"),
	pydantic.Def("op", connectorOp, nil),
)

// Single call execute body
var ConnectorExecuteSingleRequest = pydantic.NewModel("ConnectorExecuteSingleRequest", wireModelConfig,
	pydantic.Req("call", ConnectorExecuteCall),
	pydantic.Def("return_to", pydantic.Nullable(connectorReturnTarget), nil).Alias("returnTo"),
	pydantic.Def("op", connectorOp, nil),
)

// Guarantees a model receiving a result must give, no training or zero retention
var ConnectorDataUse = pydantic.NewModel("ConnectorDataUse", wireModelConfig,
	pydantic.Req("requires", pydantic.List(pydantic.Literal("no_training", "zero_retention"))),
)

// Result of a single call execute
var ConnectorExecuteSingleResult = pydantic.NewModel("ConnectorExecuteSingleResult", wireModelConfig,
	pydantic.Def("connector", pydantic.Str, ""),
	pydantic.Def("tool", pydantic.Str, ""),
	pydantic.Def("data", pydantic.Any, nil),
	pydantic.Def("account", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data_use", pydantic.Nullable(ConnectorDataUse), nil).Alias("dataUse"),
)

// Answer of a single call execute
var ConnectorExecuteSingleResponse = pydantic.NewModel("ConnectorExecuteSingleResponse", wireModelConfig,
	pydantic.Req("result", ConnectorExecuteSingleResult),
)

// One entry of the array form execute answer, never correlated by its wire index
var ConnectorExecuteResult = pydantic.NewModel("ConnectorExecuteResult", wireModelConfig,
	pydantic.Def("index", pydantic.Int, 0),
	pydantic.Def("connector", pydantic.Str, ""),
	pydantic.Def("tool", pydantic.Str, ""),
	pydantic.Def("data", pydantic.Any, nil),
	pydantic.Def("error", pydantic.Nullable(ConnectorToolError), nil),
	pydantic.Def("account", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("data_use", pydantic.Nullable(ConnectorDataUse), nil).Alias("dataUse"),
)

// Array form execute answer with its counts
var ConnectorExecuteResponse = pydantic.NewModel("ConnectorExecuteResponse", wireModelConfig,
	pydantic.Fac("results", pydantic.List(ConnectorExecuteResult), pydantic.NewList),
	pydantic.Def("success_count", pydantic.Int, 0).Alias("successCount"),
	pydantic.Def("error_count", pydantic.Int, 0).Alias("errorCount"),
	pydantic.Def("total_count", pydantic.Int, 0).Alias("totalCount"),
)

// Body of the connections route that starts or repairs authorizations
var ConnectorConnectionsRequest = pydantic.NewModel("ConnectorConnectionsRequest", wireModelConfig,
	pydantic.Req("connectors", pydantic.List(pydantic.Str)),
	pydantic.Def("reinitiate", pydantic.Bool, false),
	pydantic.Def("alias", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil).Alias("connectionId"),
	pydantic.Def("return_to", pydantic.Nullable(connectorReturnTarget), nil).Alias("returnTo"),
	pydantic.Def("op", connectorOp, nil),
	pydantic.Def("initiator", pydantic.Nullable(connectorInitiator), nil),
)

// One connector's authorization outcome
var ConnectorConnectionResult = pydantic.NewModel("ConnectorConnectionResult", wireModelConfig,
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("status", pydantic.Literal("active", "initiated", "failed")),
	pydantic.Def("connect_url", pydantic.Nullable(pydantic.Str), nil).Alias("connectUrl"),
	pydantic.Def("connection_id", pydantic.Nullable(pydantic.Str), nil).Alias("connectionId"),
	pydantic.Def("alias", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("instruction", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("status_reason", pydantic.Nullable(pydantic.Str), nil).Alias("statusReason"),
	pydantic.Def("reinitiated", pydantic.Bool, false),
)

// One page of the session's toolkit list
var ConnectorListResponse = pydantic.NewModel("ConnectorListResponse", wireModelConfig,
	pydantic.Req("items", pydantic.List(ConnectorListItem)),
	pydantic.Def("next_cursor", pydantic.Nullable(pydantic.Str), nil).Alias("nextCursor"),
)

// One vendor account of a connector with its six state status
var ConnectorAccount = pydantic.NewModel("ConnectorAccount", wireModelConfig,
	pydantic.Req("connection_id", pydantic.Str).Alias("connectionId"),
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("status", pydantic.Literal("pending", "active", "failed", "expired", "revoked", "inactive")),
	pydantic.Def("status_reason", pydantic.Nullable(pydantic.Str), nil).Alias("statusReason"),
	pydantic.Req("label", pydantic.ConStr(pydantic.MinLen(1))),
	pydantic.Def("alias", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("active", pydantic.Bool),
	pydantic.Def("disabled", pydantic.Bool, false),
	pydantic.Req("created_at", pydantic.Str).Alias("createdAt"),
	pydantic.Req("updated_at", pydantic.Str).Alias("updatedAt"),
)

// Every account of the caller's principal
var ConnectorAccountsResponse = pydantic.NewModel("ConnectorAccountsResponse", wireModelConfig,
	pydantic.Req("accounts", pydantic.List(ConnectorAccount)),
)

// Row the portal answers for a deleted account
var RemovedConnectorAccount = pydantic.NewModel("RemovedConnectorAccount", wireModelConfig,
	pydantic.Req("connection_id", pydantic.Str).Alias("connectionId"),
	pydantic.Req("connector", pydantic.Str),
	pydantic.Req("status", pydantic.Literal("removed")),
)

// Counts of a connections answer by outcome
var ConnectorConnectionsSummary = pydantic.NewModel("ConnectorConnectionsSummary", wireModelConfig,
	pydantic.Def("total", pydantic.Int, 0),
	pydantic.Def("active", pydantic.Int, 0),
	pydantic.Def("initiated", pydantic.Int, 0),
	pydantic.Def("failed", pydantic.Int, 0),
)

// Answer of the connections route, an all zero summary when the gateway sends none
var ConnectorConnectionsResponse = pydantic.NewModel("ConnectorConnectionsResponse", wireModelConfig,
	pydantic.Fac("results", pydantic.List(ConnectorConnectionResult), pydantic.NewList),
	pydantic.Fac("summary", ConnectorConnectionsSummary, func() any {
		summary, _ := ConnectorConnectionsSummary.Validate(pyjson.NewObject())
		return summary
	}),
)

// Python value of a validated model dump, every object a plain map and every number a float64
func wireDump(inst *pydantic.Instance, byAlias bool) map[string]any {
	m, _ := pyjson.WTF8Shape(inst.Dump(pydantic.DumpOptions{ByAlias: byAlias})).(map[string]any)
	return m
}

// Request body a model validates from the given field values, dumped by alias with nulls left out
func wireBody(model *pydantic.Model, fields *pyjson.Object) (*pyjson.Object, error) {
	inst, verr := model.Validate(fields)
	if verr != nil {
		return nil, verr
	}
	body, _ := inst.Dump(pydantic.DumpOptions{ByAlias: true, ExcludeNone: true}).(*pyjson.Object)
	return body, nil
}

// Null for an empty Go string, the string otherwise, the Optional[str] a wire field takes
func wireOptional(s string) any {
	if s == "" {
		return nil
	}
	return s
}
