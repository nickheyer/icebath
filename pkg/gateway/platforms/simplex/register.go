package simplex

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Model facing description of SimpleX, kept byte stable for the cached prompt
const platformHint = "You are chatting via SimpleX Chat, a private decentralised " +
	"messenger. Contacts are identified by opaque internal IDs, " +
	"not phone numbers or usernames. SimpleX supports standard " +
	"markdown formatting. There is no typing indicator and no " +
	"hard message length limit, but keep responses conversational. " +
	"You can attach native images, voice notes, and arbitrary " +
	"files; the adapter handles MEDIA:<path> tags by sending them " +
	"as inline voice notes (audio extensions) or documents."

// Home and allowlist names already warned about, kept for the process since reconnects rebuild adapters
var nameWarnings = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Warns once per profile about SIMPLEX_ALLOWED_USERS entries that are display names authorization ignores
func warnNameAllowlistEntries(ctx context.Context) {
	var entries []string
	switch raw := gateway.DecodeJSONListLiteral(gateway.PlatformGateEnv(ctx, "SIMPLEX_ALLOWED_USERS", "")).(type) {
	case []any:
		for _, e := range raw {
			entries = append(entries, strings.TrimSpace(fmt.Sprint(e)))
		}
	case string:
		entries = strings.Split(raw, ",")
	}
	var names []string
	for _, e := range entries {
		if e = strings.TrimSpace(e); e != "" && e != "*" && !isDigits(e) {
			names = append(names, e)
		}
	}
	if len(names) == 0 {
		return
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	key := home.Key(home.Dir(ctx)) + "\x00" + strings.Join(sorted, "\x00")
	nameWarnings.Lock()
	seen := nameWarnings.seen[key]
	nameWarnings.seen[key] = true
	nameWarnings.Unlock()
	if !seen {
		slog.Warn(fmt.Sprintf("[simplex] SIMPLEX_ALLOWED_USERS entries %q are not numeric contactIds and are ignored "+
			"(display names are not trusted, see /contacts for IDs)", names))
	}
}

// Whether the profile scoped env names the daemon WebSocket URL
func checkRequirements(ctx context.Context) bool {
	v, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	return err == nil && strings.TrimSpace(v) != ""
}

// Whether the daemon URL comes from the scoped env or the config extra
func validateConfig(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) {
	if checkRequirements(ctx) {
		return true, nil
	}
	return cfg != nil && strings.TrimSpace(str(cfg.Extra, "ws_url")) != "", nil
}

// Extra seeded from the profile env before the adapter is built, nil without SIMPLEX_WS_URL
func envEnablement(ctx context.Context) map[string]any {
	wsURL, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
	if err != nil || strings.TrimSpace(wsURL) == "" {
		return nil
	}
	seed, err := gateway.SeedExtraFromEnv(ctx, []gateway.EnvSeedRow{
		{Env: "SIMPLEX_AUTO_ACCEPT", Key: "auto_accept", Conv: func(raw string) (any, error) { return acceptFlag(raw), nil }},
		{Env: "SIMPLEX_GROUP_ALLOWED", Key: "group_allowed"},
	}, "SIMPLEX_HOME_CHANNEL", "")
	if err != nil {
		slog.Debug(fmt.Sprintf("[simplex] env enablement could not read the profile env: %v", err))
		return nil
	}
	seed["ws_url"] = strings.TrimSpace(wsURL)
	return seed
}

// Daemon URL a standalone send dials, the scoped env winning over the config extra
func standaloneURL(ctx context.Context, pconfig *gateway.PlatformConfig) string {
	if v, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if pconfig != nil {
		if v := strings.TrimSpace(str(pconfig.Extra, "ws_url")); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return defaultWSURL
}

// Text and media delivery for cron and icebath send, over the in process connection or a short lived one
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID, _ := kwargs["chat_id"].(string)
	message, _ := kwargs["message"].(string)
	mediaFiles, _ := kwargs["media_files"].([]gateway.MediaRef)
	forceDocument, _ := kwargs["force_document"].(bool)
	wsURL := standaloneURL(ctx, pconfig)
	live.Lock()
	adapter := live.byURL[wsURL]
	live.Unlock()
	var c *wsClient
	if adapter != nil {
		c = adapter.liveClient()
	}
	if c == nil {
		dialed, err := dialClient(ctx, wsURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return gateway.SendError(fmt.Sprintf("SimpleX send failed: %v", err)), nil
		}
		go func() { _ = dialed.readLoop(nil) }()
		defer dialed.close()
		c = dialed
	}
	s := sender{c}
	var last gateway.SendResult
	sent := false
	if strings.TrimSpace(message) != "" {
		if last = s.text(ctx, chatID, message); !last.Success {
			return gateway.SendError(fmt.Sprintf("SimpleX send failed: %v", last.Error)), nil
		}
		sent = true
	}
	for _, m := range mediaFiles {
		if last = s.media(ctx, chatID, m, forceDocument); !last.Success {
			return gateway.SendError(fmt.Sprintf("SimpleX media send failed: %v", last.Error)), nil
		}
		sent = true
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !sent {
		return gateway.SendError("SimpleX send failed: no text or media to deliver"), nil
	}
	result := map[string]any{"success": true, "platform": "simplex", "chat_id": chatID, "message_id": last.MessageID}
	if len(mediaFiles) > 0 {
		result["media_delivered"] = true
	}
	return result, nil
}

// SimpleX platform plugin compiled into the binary under the bundled platforms/simplex key
type simplexPlatformPlugin struct{}

func (simplexPlatformPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "simplex-platform", Version: "1.1.0", Kind: "platform", Source: "bundled", Key: "platforms/simplex",
		Author: "Mibayy, jooray",
		Description: "SimpleX Chat gateway adapter for Icebath. Connects to a local simplex-chat daemon via WebSocket and relays " +
			"messages between SimpleX contacts/groups and the Icebath agent. SimpleX is decentralised and assigns no persistent " +
			"user IDs, every contact is an opaque internal ID generated at connection time.\n",
		RequiresEnv: []plugins.EnvSpec{
			{Name: "SIMPLEX_WS_URL", Description: "WebSocket URL of the simplex-chat daemon (e.g. ws://127.0.0.1:5225)"}}}
}

// Registers the SimpleX platform entry with its factory, gates, setup wizard, standalone sender and hint
func (simplexPlatformPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterPlatform(plugins.PlatformSpec{
		Name: "simplex", Label: "SimpleX Chat",
		AdapterFactory: gateway.AdapterFactory(func(fctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return NewAdapter(fctx, cfg)
		}),
		Check:       func() bool { return checkRequirements(context.Background()) },
		RequiredEnv: []string{"SIMPLEX_WS_URL"},
		InstallHint: "Start the simplex-chat daemon (simplex-chat -p 5225) and set SIMPLEX_WS_URL",
		Extra: map[string]any{
			"standalone_send":        &gateway.StandaloneSendPolicy{Label: "SimpleX", Media: true, ForwardForceDocument: true},
			"dashboard_description":  "Talk to Icebath over SimpleX Chat via a local simplex-chat daemon.",
			"docs_url":               "https://simplex.chat/docs/cli.html",
			"scoped_check_fn":        checkRequirements,
			"scoped_validate_config": validateConfig,
			"is_connected":           validateConfig,
			"setup_fn":               InteractiveSetup,
			"env_enablement_fn":      envEnablement,
			"cron_deliver_env_var":   "SIMPLEX_HOME_CHANNEL",
			"standalone_sender_fn":   standaloneSend,
			"allowed_users_env":      "SIMPLEX_ALLOWED_USERS",
			"allow_all_env":          "SIMPLEX_ALLOW_ALL_USERS",
			"max_message_length":     maxMessageLength,
			"emoji":                  "🔒",
			"pii_safe":               true,
			"allow_update_command":   true,
			"platform_hint":          platformHint,
		},
	})
	return err
}

var (
	_ gateway.PlatformAdapter       = (*Adapter)(nil)
	_ gateway.ChannelListingAdapter = (*Adapter)(nil)
)

func init() { plugins.RegisterBundled(simplexPlatformPlugin{}) }
