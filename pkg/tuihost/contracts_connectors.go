package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConnectorSlug = pydantic.ConStr(pydantic.Pattern(`^[a-z0-9][a-z0-9_-]*$`))

var ConnectorErrorReason = pydantic.Enum("ConnectorErrorReason", "invalid_params", "INVALID_PARAMS", "not_owner", "NOT_OWNER", "connector_request_failed", "CONNECTOR_REQUEST_FAILED", "unknown_target", "UNKNOWN_TARGET", "link_still_valid", "LINK_STILL_VALID", "reissue_refused", "REISSUE_REFUSED", "unknown_operation", "UNKNOWN_OPERATION", "invalid_answer", "INVALID_ANSWER", "connectors_unavailable", "CONNECTORS_UNAVAILABLE")

var ConnectorsConnectParams = pydantic.Extend(ProfileParams, "ConnectorsConnectParams", ParamsConfig,
	pydantic.Req("owner", SessionOwner),
	pydantic.Req("connectors", pydantic.List(ConnectorSlug, pydantic.MinLen(1))),
	pydantic.Def("reconnect", pydantic.Bool, false),
)

var ConnectorsConnectResult = pydantic.Extend(ConnectionOperationStatus, "ConnectorsConnectResult", ClosedConfig,
	pydantic.Def("status", pydantic.Nullable(pydantic.Literal("initiated", "settled")), nil),
	pydantic.Def("note", pydantic.Nullable(pydantic.Str), nil),
).Described("``methods_connectors._reissue``: the operation the connect opened; ``status``/``note`` ride along from the tool result.")

var _ = Method("connectors.connect", ConnectorsConnectParams, ConnectorsConnectResult, "Run the named failed rows of the session's open connection operation again, the card's Try again.")
