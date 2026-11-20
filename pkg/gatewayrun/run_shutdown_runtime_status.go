package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Platform runtime status fields of one write, nil strings as None and an unset retrying since left out
type platformStatus struct {
	state          *string
	errorCode      *string
	errorMessage   *string
	needsAttention *bool
	retryingSince  *any
}

// Pointer to a string value, the Python str of an optional argument
func optStr(s string) *string { return &s }

// Pointer to a bool value, the Python bool of an optional argument
func optBool(b bool) *bool { return &b }

// Pointer to a retrying since value, nil meaning the field is written as None
func optSince(v any) *any { return &v }

// Optional string as the Python value of a status field
func statusField(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Publishes one platform's runtime status quietly, the attention flag and retry stamp only when given
func (r *GatewayRunner) updatePlatformRuntimeStatus(ctx context.Context, platform string, status platformStatus) {
	fields := map[string]any{"platform": platform, "platform_state": statusField(status.state),
		"error_code": statusField(status.errorCode), "error_message": statusField(status.errorMessage)}
	if status.needsAttention != nil {
		fields["needs_attention"] = *status.needsAttention
	}
	if status.retryingSince != nil {
		fields["retrying_since"] = *status.retryingSince
	}
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: fields})
}
