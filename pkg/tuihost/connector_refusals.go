package tuihost

import (
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One connector refusal as both connector handlers send it, a wire code, a ConnectorErrorReason and a message
type ConnectorRefusalSpec struct {
	Code    int
	Reason  tuigateway.ConnectorErrorReason
	Message string
}

// Connector refusals of the legacy handlers and the session authority's connection verbs
var (
	ConnectorInvalidParams         = ConnectorRefusalSpec{4000, "invalid_params", "Connector parameters are invalid."}
	ConnectorInvalidAnswer         = ConnectorRefusalSpec{4002, "invalid_answer", "Connection answer is invalid."}
	ConnectorUnknownOperation      = ConnectorRefusalSpec{4004, "unknown_operation", "No open operation with that op_id."}
	ConnectorUnknownTargetRefusal  = ConnectorRefusalSpec{4004, "unknown_target", "No such target on the open operation."}
	ConnectorConnectorsUnavailable = ConnectorRefusalSpec{4031, "connectors_unavailable", "Connectors are not available in this session."}
	ConnectorRequestFailed         = ConnectorRefusalSpec{5034, "connector_request_failed", "Connector request failed. Try again explicitly."}
	ConnectorReissueRefused        = ConnectorRefusalSpec{4002, "reissue_refused", "The target cannot be run again."}
	ConnectorReissueRefusals       = map[string]ConnectorRefusalSpec{
		tools.ConnUnknownTarget:  ConnectorUnknownTargetRefusal,
		tools.ConnMixedKinds:     {4000, "invalid_params", "One target kind per request."},
		tools.ConnLinkStillValid: {4002, "link_still_valid", "Reopen the stored link."},
		tools.ConnSettled:        {4002, "reissue_refused", "The operation has settled."},
		tools.ConnNotAllowed:     {4002, "reissue_refused", "This target cannot be run again."},
		tools.ConnRefused:        ConnectorReissueRefused,
	}
)
