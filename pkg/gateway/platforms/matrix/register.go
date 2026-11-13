package matrix

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// YAML key, env var and encoding kind of every matrix block key bridged into the environment
var yamlBridge = []gateway.YAMLBridgeRow{
	{Key: "require_mention", Env: "MATRIX_REQUIRE_MENTION", Kind: "lower"}, {Key: "process_notices", Env: "MATRIX_PROCESS_NOTICES", Kind: "lower"},
	{Key: "session_scope", Env: "MATRIX_SESSION_SCOPE", Kind: "lower"}, {Key: "auto_thread", Env: "MATRIX_AUTO_THREAD", Kind: "lower"},
	{Key: "dm_mention_threads", Env: "MATRIX_DM_MENTION_THREADS", Kind: "lower"},
	{Key: "allowed_users", Env: "MATRIX_ALLOWED_USERS", Kind: "csv"}, {Key: "free_response_rooms", Env: "MATRIX_FREE_RESPONSE_ROOMS", Kind: "csv"},
	{Key: "allowed_rooms", Env: "MATRIX_ALLOWED_ROOMS", Kind: "csv"}, {Key: "ignore_user_patterns", Env: "MATRIX_IGNORE_USER_PATTERNS", Kind: "csv"},
	{Key: "max_message_length", Env: "MATRIX_MAX_MESSAGE_LENGTH", Kind: "str"},
}

// Bridges matrix YAML keys into MATRIX env vars, explicit env winning, and returns the extra they seed
func applyYAMLConfig(ctx context.Context, _ map[string]any, matrixCfg map[string]any) (map[string]any, error) {
	return gateway.ApplyYAMLBridge(ctx, matrixCfg, yamlBridge)
}

// Env value through the gateway setup reader tests replace
var getEnvValue = func(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// Connected means a homeserver plus an access token or password, config extras honored
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	var extra map[string]any
	token := ""
	if cfg != nil {
		extra = cfg.Extra
		token = tokenOf(cfg.Token)
	}
	homeserver := ""
	if v := extra["homeserver"]; pyTruthy(v) {
		homeserver = pyrepr.Str(v)
	} else {
		v, err := getEnvValue(ctx, "MATRIX_HOMESERVER")
		if err != nil {
			return false, err
		}
		homeserver = v
	}
	for _, name := range []string{"MATRIX_ACCESS_TOKEN", "MATRIX_PASSWORD"} {
		if token != "" {
			break
		}
		v, err := getEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		token = v
	}
	return pystr.Strip(homeserver) != "" && pystr.Strip(token) != "", nil
}

// Room and thread of a room colon dollar event id target, nil for bare rooms and users
func parseSendTarget(ref string) any {
	trimmed := pystr.Strip(ref)
	splitIdx := strings.LastIndex(trimmed, ":$")
	if splitIdx <= 0 {
		return nil
	}
	chatID, threadID := trimmed[:splitIdx], trimmed[splitIdx+1:]
	return &gateway.TargetRefParse{ChatID: &chatID, ThreadID: &threadID}
}

// Matrix platform plugin compiled into the binary under the bundled platforms/matrix key
type matrixPlatformPlugin struct{}

func (matrixPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "matrix-platform", Version: "1.0.0", Kind: "platform", Source: "bundled", Key: "platforms/matrix",
		Author: "NousResearch",
		Description: "Matrix gateway adapter for Icebath Agent. Connects to a Matrix homeserver via mautrix (with optional E2EE) and relays " +
			"messages between Matrix rooms/DMs and the Icebath agent. Supports threads, HTML/markdown rendering, native media uploads, " +
			"mention gating, free-response rooms, and per-room allowlists.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "MATRIX_HOMESERVER", Description: "Matrix homeserver URL (e.g. https://matrix.org)"},
			{Name: "MATRIX_ACCESS_TOKEN", Description: "Matrix access token (or use MATRIX_PASSWORD for password login)", Secret: true}}}
}

// Registers the Matrix platform entry with its factory, setup, YAML bridge, standalone sender and card
func (matrixPlatformPlugin) Register(ctx *plugins.Context) error {
	var card gateway.PlatformEntry
	applyDashboardCard(&card)
	extra := map[string]any{
		"target_ref_syntax_fn":  parseSendTarget,
		"dashboard_description": card.DashboardDescription, "docs_url": card.DocsURL,
		"dashboard_env_vars": card.DashboardEnvVars, "dashboard_required_env": card.DashboardRequiredEnv,
		"ensure_deps_fn":         ensureMatrixDeps,
		"is_connected":           isConnected,
		"setup_fn":               InteractiveSetup,
		"apply_yaml_config_fn":   applyYAMLConfig,
		"cron_deliver_env_var":   "MATRIX_HOME_ROOM",
		"standalone_sender_fn":   standaloneSend,
		"max_message_length":     defaultMaxMessageLength,
		"emoji":                  "🔐",
		"allow_update_command":   true,
		"platform_hint":          platformHint,
		"display_tier":           "medium",
		"env_overrides":          envOverrides,
		"env_enable_credentials": envEnableCredentials,
	}
	for k, v := range authz {
		extra[k] = v
	}
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "matrix", Label: "Matrix",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			adapter, err := NewMatrixAdapter(fctx, cfg)
			if err != nil {
				return nil, err
			}
			return adapter, nil
		}),
		Check:       matrixDepsPresent,
		RequiredEnv: []string{"MATRIX_HOMESERVER", "MATRIX_ACCESS_TOKEN"},
		Extra:       extra,
	})
	return err
}

var (
	_ gateway.PlatformAdapter    = (*MatrixAdapter)(nil)
	_ gateway.ModelPickerSender  = (*MatrixAdapter)(nil)
	_ gateway.ChoicePickerSender = (*MatrixAdapter)(nil)
)

func init() { plugins.RegisterBundled(matrixPlatformPlugin{}) }
