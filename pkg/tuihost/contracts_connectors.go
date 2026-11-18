package tuihost

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var ConnectorErrorReason = pydantic.Enum("ConnectorErrorReason", "invalid_params", "INVALID_PARAMS", "not_owner", "NOT_OWNER", "connector_request_failed", "CONNECTOR_REQUEST_FAILED", "unknown_target", "UNKNOWN_TARGET", "link_still_valid", "LINK_STILL_VALID", "reissue_refused", "REISSUE_REFUSED", "unknown_operation", "UNKNOWN_OPERATION", "invalid_answer", "INVALID_ANSWER", "connectors_unavailable", "CONNECTORS_UNAVAILABLE")

