package browser

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live connect endpoint of the env's profile, empty for an unbound context on a multiplexed process
func liveCDPOverride(env *tools.Env) string {
	if (env == nil || env.Home == "") && provider.IsMultiplexActive(context.Background()) {
		return ""
	}
	return LiveCDPOverride(cfgOf(env).homeKey())
}

// Starts the task's CDP supervisor on the override or the session's own endpoint, a failed attach only logged
func ensureCDPSupervisor(env *tools.Env, taskID string) {
	s := cfgOf(env)
	cdpURL := s.cdpOverride(context.Background())
	if cdpURL == "" {
		sessionsMu.Lock()
		info, _ := activeSessions.Get(taskID)
		sessionCDP := ""
		if pyrepr.Truthy(info["cdp_url"]) {
			sessionCDP = pyrepr.Str(info["cdp_url"])
		}
		sessionsMu.Unlock()
		if sessionCDP != "" {
			cdpURL = ResolveCDPOverride(context.Background(), sessionCDP)
		}
	}
	if cdpURL == "" {
		return
	}
	policy, timeoutS := s.dialogPolicyConfig()
	if err := catchRaised(func() error { return supervisorGetOrStart(env, taskID, cdpURL, policy, timeoutS) }); err != nil {
		s.logger().Debug(fmt.Sprintf("CDP supervisor attach for task=%s failed (non-fatal): %s", taskID, err))
	}
}

// Stops the task's CDP supervisor, a teardown error only logged
func stopCDPSupervisor(env *tools.Env, taskID string) {
	if err := catchRaised(func() error { supervisorStop(taskID); return nil }); err != nil {
		cfgOf(env).logger().Debug(fmt.Sprintf("CDP supervisor stop for task=%s failed (non-fatal): %s", taskID, err))
	}
}
