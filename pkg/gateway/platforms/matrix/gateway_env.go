package matrix

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Writes encryption from the e2ee mode or the encryption flag, then the mode and device id when set
func matrixE2EE(ctx context.Context, cfg *gateway.GatewayConfig, pc *gateway.PlatformConfig) error {
	mode := pystr.Lower(pystr.Strip(gateway.EnvFirst(ctx, []string{"MATRIX_E2EE_MODE"})))
	switch mode {
	case "required", "require", "optional", "prefer", "preferred":
		pc.Extra["encryption"] = true
	default:
		pc.Extra["encryption"] = gateway.TruthyValue(gateway.EnvFirst(ctx, []string{"MATRIX_ENCRYPTION"}))
	}
	if mode != "" {
		pc.Extra["e2ee_mode"] = mode
	}
	gateway.EnvExtras(ctx, pc.Extra, []gateway.EnvSpec{{Key: "device_id", Env: "MATRIX_DEVICE_ID"}}, false)
	return nil
}

var envEnableCredentials = []string{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"}

var envOverrides = []gateway.EnvStep{
	&gateway.EnvCred{Platform: gateway.PlatformMatrix, Creds: [][]string{{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"}}, Token: "MATRIX_ACCESS_TOKEN",
		WarnMissingEnv: "MATRIX_HOMESERVER", WarnMissingMsg: "MATRIX_ACCESS_TOKEN/MATRIX_PASSWORD set but MATRIX_HOMESERVER is missing",
		Fixed:    []gateway.EnvSpec{{Key: "homeserver", Env: "MATRIX_HOMESERVER"}},
		Optional: []gateway.EnvSpec{{Key: "user_id", Env: "MATRIX_USER_ID"}, {Key: "password", Env: "MATRIX_PASSWORD"}},
		Then:     matrixE2EE},
	gateway.HomeStep{Platform: gateway.PlatformMatrix, EnvBase: "MATRIX_HOME_ROOM"},
}

var authz = map[string]any{"allowed_users_env": "MATRIX_ALLOWED_USERS", "allow_all_env": "MATRIX_ALLOW_ALL_USERS", "allowlist_suppresses_pairing": true}

// Registers the env declarations of the bundled matrix directory with the gateway config bridge
func init() {
	gateway.RegisterBundledEnv("matrix", "", gateway.BundledEnvRows{Overrides: envOverrides, EnableCredentials: envEnableCredentials, Authz: authz})
}
