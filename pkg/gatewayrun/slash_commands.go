package gatewayrun

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Canonical reply of a shared slash executor, its text and structured data
type SlashCommandReply struct {
	Text   string
	Data   map[string]any
	Format string
}

// Shared slash executor of pkg/commands run on a surface with its args and options, assigned in cmd/icebath wiring
var ExecuteSlashCommand func(ctx context.Context, name, surface, args string, options map[string]any) (SlashCommandReply, error)

// Runs a command through the shared slash executor on the gateway surface
func execute(ctx context.Context, command, args string, options map[string]any) (SlashCommandReply, error) {
	if options == nil {
		options = map[string]any{}
	}
	return ExecuteSlashCommand(ctx, command, "gateway", args, options)
}

// Sets one config leaf through a strict raw round trip so read errors and expanded values never get saved
func writeRawConfigLeaf(ctx context.Context, configPath string, keys []string, value any) error {
	raw, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	parents, leaf := keys[:len(keys)-1], keys[len(keys)-1]
	nestedDict(raw, parents...)[leaf] = value
	return config.AtomicConfigWrite(ctx, home.Dir(ctx), configPath, raw, "")
}

// First limit characters of text with an ellipsis when it was longer
func preview(text string, limit int) string {
	out := pystr.Slice(text, 0, limit)
	if len(pystr.Runes(text)) > limit {
		out += "..."
	}
	return out
}

// Requester routing the restarted gateway uses to notify them once back online
func restartNotifyPayload(event *gateway.MessageEvent) map[string]any {
	source := event.Source
	var platform any
	if source.Platform != "" {
		platform = string(source.Platform)
	}
	data := map[string]any{"platform": platform, "chat_id": source.ChatID, "chat_type": source.ChatType}
	if source.DeliveredViaUpstreamRelay {
		data["delivered_via_upstream_relay"] = true
		if source.UserID != nil && *source.UserID != "" {
			data["user_id"] = *source.UserID
		}
		if source.ScopeID != nil && *source.ScopeID != "" {
			data["scope_id"] = *source.ScopeID
		}
	}
	if source.ThreadID != nil && *source.ThreadID != "" {
		data["thread_id"] = *source.ThreadID
	}
	if event.MessageID != nil && *event.MessageID != "" {
		data["message_id"] = *event.MessageID
	}
	if source.Profile != nil && *source.Profile != "" {
		data["profile"] = *source.Profile
	}
	return data
}

// Thread id sethome persists on the home target, nil for none or a synthetic Slack top level message thread
func homeThreadFromSource(source *gateway.SessionSource) *string {
	if source == nil || source.ThreadID == nil || *source.ThreadID == "" {
		return nil
	}
	thread := *source.ThreadID
	if source.Platform == gateway.PlatformSlack && source.MessageID != nil && *source.MessageID != "" && thread == *source.MessageID {
		return nil
	}
	return &thread
}

// Profile serving the source and its home, the routed profile under multiplex and the process profile otherwise
func (r *GatewayRunner) handleProfileCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	profileName, display := "", ""
	if r.multiplexing() {
		if source != nil && source.Profile != nil {
			profileName = pystr.Strip(*source.Profile)
		}
		display = home.Display(home.Dir(ctx))
		if source != nil {
			if h, err := r.resolveProfileHomeForSource(ctx, source); err == nil {
				display = home.Display(home.Dir(gateway.ProfileRuntimeScope(ctx, h, false)))
			}
		}
	}
	reply, err := execute(ctx, "profile", "", map[string]any{"profile_name": profileName, "home_display": display})
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		i18n.T(ctx, "gateway.profile.header", "", map[string]any{"profile": reply.Data["profile"]}),
		i18n.T(ctx, "gateway.profile.home", "", map[string]any{"home": reply.Data["home"]}),
	}, "\n"), nil
}

// Platform, dm or group scope, access tier and runnable commands of the caller
func (r *GatewayRunner) handleWhoamiCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	policy := gateway.PolicyForRunnerSource(r, source)
	platform, chatType, userID := "?", "", "?"
	if source != nil {
		if source.Platform != "" {
			platform = string(source.Platform)
		}
		chatType = source.ChatType
		if source.UserID != nil && *source.UserID != "" {
			userID = *source.UserID
		}
	}
	if chatType == "" {
		chatType = "dm"
	}
	scopeKey := "gateway.whoami.scope_group"
	switch pystr.Lower(chatType) {
	case "dm", "direct", "private", "":
		scopeKey = "gateway.whoami.scope_dm"
	}
	head := i18n.T(ctx, "gateway.whoami.header", "", map[string]any{"platform": platform,
		"scope": i18n.T(ctx, scopeKey, "", nil), "user_id": userID})
	if !policy.Enabled {
		return head + i18n.T(ctx, "gateway.whoami.tier_unrestricted", "", nil), nil
	}
	if policy.IsAdmin(userID) {
		return head + i18n.T(ctx, "gateway.whoami.tier_admin", "", nil), nil
	}
	extra := make([]string, 0, len(policy.UserAllowedCommands))
	for c := range policy.UserAllowedCommands {
		extra = append(extra, c)
	}
	sort.Strings(extra)
	seen := map[string]bool{}
	var runnable []string
	for _, c := range append([]string{"help", "whoami"}, extra...) {
		if !seen[c] {
			seen[c] = true
			runnable = append(runnable, "/"+c)
		}
	}
	runnableStr := i18n.T(ctx, "gateway.shared.none_marker", "", nil)
	if len(runnable) > 0 {
		runnableStr = strings.Join(runnable, ", ")
	}
	return head + i18n.T(ctx, "gateway.whoami.tier_user", "", map[string]any{"commands": runnableStr}), nil
}

// Running icebath agent version
func (r *GatewayRunner) handleVersionCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	reply, err := execute(ctx, "version", "", nil)
	if err != nil {
		return "", err
	}
	return reply.Text, nil
}

// Installed skill bundles, each with its skills, mirroring the CLI listing
func (r *GatewayRunner) handleBundlesCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	reply, err := execute(ctx, "bundles", "", nil)
	if err != nil {
		return "", err
	}
	if msg, ok := reply.Data["error"]; ok {
		runLog().Warn(fmt.Sprintf("Bundles command unavailable: %s", pyrepr.Str(msg)))
		return reply.Text, nil
	}
	bundles := bundleMaps(reply.Data["bundles"])
	if len(bundles) == 0 {
		return i18n.T(ctx, "gateway.bundles.none", "", map[string]any{"dir": reply.Data["dir"]}), nil
	}
	lines := []string{i18n.T(ctx, "gateway.bundles.header", "", map[string]any{"count": len(bundles)}), ""}
	for _, info := range bundles {
		skills := stringItems(info["skills"])
		desc := info["description"]
		if !pyrepr.Truthy(desc) {
			desc = i18n.T(ctx, "gateway.bundles.default_desc", "", map[string]any{"count": len(skills)})
		}
		lines = append(lines, i18n.T(ctx, "gateway.bundles.item", "", map[string]any{"slug": info["slug"], "desc": desc, "count": len(skills)}))
		for _, s := range skills {
			lines = append(lines, "    · "+s)
		}
	}
	lines = append(lines, "", i18n.T(ctx, "gateway.bundles.invoke_hint", "", nil))
	return strings.Join(lines, "\n"), nil
}

// Bundle mappings of a reply data value
func bundleMaps(v any) []map[string]any {
	switch b := v.(type) {
	case []map[string]any:
		return b
	case []any:
		out := make([]map[string]any, 0, len(b))
		for _, item := range b {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Text of each item of a list value
func stringItems(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			out = append(out, pyrepr.Str(item))
		}
		return out
	}
	return nil
}
