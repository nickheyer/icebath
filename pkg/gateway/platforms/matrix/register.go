package matrix

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Env value through the gateway setup reader tests replace
var getEnvValue = func(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// Connected means a homeserver plus an access token or password
func isConnected(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	token := ""
	if cfg != nil {
		token = tokenOf(cfg.Token)
	}
	homeserver := settings.MatrixHomeserver.Get(gateway.SettingsContext(ctx))
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
		Author: "Icebath",
		Description: "Matrix gateway adapter for Icebath Agent. Connects to a Matrix homeserver via mautrix (with optional E2EE) and relays " +
			"messages between Matrix rooms/DMs and the Icebath agent. Supports threads, HTML/markdown rendering, native media uploads, " +
			"mention gating, free-response rooms, and per-room allowlists.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "MATRIX_ACCESS_TOKEN", Description: "Matrix access token (or use MATRIX_PASSWORD for password login)", Secret: true}}}
}

// Registers the Matrix platform entry with its factory, setup and standalone sender
func (matrixPlatformPlugin) Register(ctx *plugins.Context) error {
	extra := map[string]any{
		"target_ref_syntax_fn":   parseSendTarget,
		"ensure_deps_fn":         ensureMatrixDeps,
		"is_connected":           isConnected,
		"setup_fn":               InteractiveSetup,
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
		RequiredEnv: []string{"MATRIX_ACCESS_TOKEN"},
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
