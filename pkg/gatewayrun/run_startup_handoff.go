package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Resolved destination of one handoff row
type handoffDestination struct {
	platform          gateway.Platform
	platformName      string
	transport         *gateway.DeliveryTransport
	home              *gateway.HomeChannel
	homeChatID        string
	effectiveThreadID *string
	source            *gateway.SessionSource
	handoffConfig     *gateway.GatewayConfig
}

// Adapter answering whether a chat lives in a workspace scope, the Slack adapter's scope_id_for_chat
type scopeIDForChatAdapter interface {
	ScopeIDForChat(chatID string) *string
}

// Config and adapters of the profile that queued the handoff, failing closed for a secondary it cannot load
func (r *GatewayRunner) handoffResolveScope(ctx context.Context, profileName *string) (*gateway.GatewayConfig, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter], error) {
	if profileName == nil || *profileName == "" || *profileName == "default" {
		return r.config, r.adapters, nil
	}
	var secondary *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	if r.profileAdapters != nil {
		secondary, _ = r.profileAdapters.Get(*profileName)
	}
	if secondary == nil || secondary.Len() == 0 {
		return nil, nil, fmt.Errorf("profile '%s' has no live adapters in this gateway", *profileName)
	}
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		runLog().Error(fmt.Sprintf("Handoff: could not load config for profile %s; failing the handoff instead of delivering via the primary's config", *profileName), "exc_info", err)
		return nil, nil, fmt.Errorf("could not load config for profile '%s': %s", *profileName, err)
	}
	return cfg, secondary, nil
}

// Platform, transport, home channel, thread and destination source of a handoff row
func (r *GatewayRunner) handoffResolveDestination(ctx context.Context, row map[string]any, profileName *string) (*handoffDestination, error) {
	cliSessionID := pyrepr.Str(row["id"])
	platformName := ""
	if value := row["handoff_platform"]; pyrepr.Truthy(value) {
		platformName = pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	}
	if platformName == "" {
		return nil, fmt.Errorf("handoff_platform is empty")
	}
	platform, err := gateway.ParsePlatform(ctx, platformName)
	if err != nil {
		return nil, fmt.Errorf("unknown platform '%s'", platformName)
	}
	handoffConfig, handoffAdapters, err := r.handoffResolveScope(ctx, profileName)
	if err != nil {
		return nil, err
	}
	transport := gateway.ResolveDeliveryTransport(ctx, platform, handoffConfig, handoffAdapters)
	if transport == nil {
		return nil, fmt.Errorf("platform '%s' is not active in this gateway", platformName)
	}
	homeChannel := handoffConfig.GetHomeChannel(platform)
	if homeChannel == nil || homeChannel.ChatID == "" {
		return nil, fmt.Errorf("no home channel configured for %s; run /sethome on the desired chat first", platformName)
	}
	homeChatID := homeChannel.ChatID
	cliTitle := pyrepr.Str(row["title"])
	if !pyrepr.Truthy(row["title"]) {
		cliTitle = string([]rune(cliSessionID)[:min(8, len([]rune(cliSessionID)))])
	}
	adapter, _ := transport.Adapter.(gateway.PlatformAdapter)
	var newThreadID *string
	if adapter != nil {
		created, ok, err := adapter.CreateHandoffThread(ctx, homeChatID, i18n.T(ctx, "gateway.startup.handoff_thread_title", "", map[string]any{"title": cliTitle}))
		if err != nil {
			runLog().Debug(fmt.Sprintf("Handoff: create_handoff_thread raised on %s: %s", platformName, err), "exc_info", err)
		} else if ok && created != "" {
			newThreadID = &created
		}
	}
	effectiveThreadID := newThreadID
	if effectiveThreadID == nil && homeChannel.ThreadID != nil && *homeChannel.ThreadID != "" {
		thread := *homeChannel.ThreadID
		effectiveThreadID = &thread
	}
	telegramPrivate := platform == gateway.PlatformTelegram && gateway.LooksLikeTelegramPrivateChatID(homeChatID)
	isThread := newThreadID != nil && !telegramPrivate
	chatType := "dm"
	if isThread {
		chatType = "thread"
	}
	var scopeID *string
	if platform == gateway.PlatformTelegram && !telegramPrivate {
		chatType = "group"
	}
	if platform == gateway.PlatformSlack {
		chatType = "group"
		if strings.HasPrefix(homeChatID, "D") {
			chatType = "dm"
		}
		scopeID = homeChannel.ScopeID
		if scopeID == nil || *scopeID == "" {
			scopeID = nil
			if scoper, ok := transport.Adapter.(scopeIDForChatAdapter); ok {
				scopeID = scoper.ScopeIDForChat(homeChatID)
			}
		}
	}
	if platform == gateway.PlatformMatrix {
		chatType = "group"
		if adapter != nil && handoffHomeIsDM(ctx, adapter, homeChatID) {
			chatType = "dm"
		}
	}
	chatID := homeChatID
	if isThread && platform == gateway.PlatformDiscord && effectiveThreadID != nil && *effectiveThreadID != "" {
		chatID = *effectiveThreadID
	}
	userID, userName := "system:handoff", "Handoff"
	if telegramPrivate {
		userID = homeChatID
	}
	var chatName *string
	if homeChannel.Name != nil {
		name := pyrepr.Str(homeChannel.Name)
		chatName = &name
	}
	source := &gateway.SessionSource{Platform: platform, ChatID: chatID, ChatName: chatName, ChatType: chatType,
		UserID: &userID, UserName: &userName, ThreadID: effectiveThreadID, Profile: profileName, ScopeID: scopeID}
	transportProfile := ""
	if profileName != nil {
		transportProfile = *profileName
	}
	r.canonicalize(ctx, source, transportProfile, "")
	return &handoffDestination{platform: platform, platformName: platformName, transport: transport, home: homeChannel,
		homeChatID: homeChatID, effectiveThreadID: effectiveThreadID, source: source, handoffConfig: handoffConfig}, nil
}

// Whether the adapter reports the home chat as a DM, a failed lookup counting as not one
func handoffHomeIsDM(ctx context.Context, adapter gateway.PlatformAdapter, homeChatID string) bool {
	info, err := adapter.GetChatInfo(ctx, homeChatID)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Handoff: get_chat_info failed for %s", homeChatID), "exc_info", err)
		return false
	}
	return info != nil && info["type"] == "dm"
}

// Destination session key by the adapters' own rules, namespaced to the queuing profile
func (r *GatewayRunner) handoffSessionKey(ctx context.Context, dest *handoffDestination, profileName *string) (string, error) {
	extra := map[string]any{}
	if pc := dest.handoffConfig.Platforms[dest.platform]; pc != nil && pc.Extra != nil {
		extra = pc.Extra
	}
	handoffProfile := ""
	if profileName != nil && *profileName != "" && *profileName != "default" {
		handoffProfile = *profileName
	}
	if handoffProfile == "" && r.sessionStore != nil {
		resolved, err := r.sessionStore.ResolveProfileForKey(dest.source)
		if err != nil {
			runLog().Debug("Handoff: could not resolve profile namespace", "exc_info", err)
		} else if text, ok := resolved.(string); ok && pystr.Strip(text) != "" {
			handoffProfile = text
		}
	}
	flag := func(key string, def bool) bool {
		if value, present := extra[key]; present {
			return pyrepr.Truthy(value)
		}
		return def
	}
	return gateway.BuildSessionKey(ctx, dest.source, flag("group_sessions_per_user", true), flag("thread_sessions_per_user", false), handoffProfile)
}

// Points the destination route at the handed off conversation's head and moves a viewer's mount with it
func (r *GatewayRunner) handoffRepoint(ctx context.Context, dest *handoffDestination, sessionKey, cliSessionID string) error {
	authority, localOwner, target, err := gateway.ConversationHead(ctx, r, cliSessionID)
	if err != nil {
		return err
	}
	if _, err := r.sessionStore.GetOrCreateSession(ctx, dest.source, false, true); err != nil {
		return err
	}
	switched, err := r.sessionStore.SwitchSession(ctx, sessionKey, target, nil, false)
	if err != nil {
		return err
	}
	if switched == nil {
		return fmt.Errorf("could not switch session key %s → %s", sessionKey, target)
	}
	if authority != nil {
		owner, err := authority.LogicalOwner(ctx, target)
		if err != nil {
			return err
		}
		return gateway.FollowHandOff(ctx, authority, dest.source, sessionKey, owner, localOwner)
	}
	return nil
}
