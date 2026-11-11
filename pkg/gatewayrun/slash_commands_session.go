package gatewayrun

import (
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Whether the source is an explicitly configured admin, never true while slash gating is disabled
func (r *GatewayRunner) resumeCallerIsAdmin(source *gateway.SessionSource) (admin bool) {
	defer func() {
		if recover() != nil {
			admin = false
		}
	}()
	policy := gateway.PolicyForRunnerSource(r, source)
	if source == nil || source.UserID == nil || *source.UserID == "" {
		return false
	}
	return policy.Enabled && policy.IsAdmin(*source.UserID)
}
