package matrix

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Writes encryption from the e2ee mode or the encryption flag, then the mode when set
func matrixE2EE(ctx context.Context, cfg *gateway.GatewayConfig, pc *gateway.PlatformConfig) error {
	mode := strings.ToLower(strings.TrimSpace(gateway.EnvFirst(ctx, []string{"MATRIX_E2EE_MODE"})))
	switch mode {
	case "required", "require", "optional", "prefer", "preferred":
		pc.Extra["encryption"] = true
	default:
		pc.Extra["encryption"] = gateway.TruthyValue(gateway.EnvFirst(ctx, []string{"MATRIX_ENCRYPTION"}))
	}
	if mode != "" {
		pc.Extra["e2ee_mode"] = mode
	}
	return nil
}

var envEnableCredentials = []string{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformMatrix, Creds: [][]string{{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"}}, Token: "MATRIX_ACCESS_TOKEN",
		Optional: []gateway.EnvSpec{{Key: "password", Env: "MATRIX_PASSWORD"}}, Then: matrixE2EE},
	gateway.HomeStep{Platform: gateway.PlatformMatrix, EnvBase: "MATRIX_HOME_ROOM"},
}

var authz = map[string]any{"allowed_users_env": "MATRIX_ALLOWED_USERS", "allow_all_env": "MATRIX_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled matrix directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("matrix", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
