package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
)

// True when an adapter send result explicitly reports failure
func sendFailed(result gateway.SendResult) bool { return !result.Success }

// Error text of a failed send result, the attribute value even when the adapter left it unset
func sendError(result gateway.SendResult) string { return anyutil.String(result.Error) }
