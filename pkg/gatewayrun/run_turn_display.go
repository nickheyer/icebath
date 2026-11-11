package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Per turn display, progress, status and streaming surface settings of one agent run
type runAgentDisplay struct {
	userConfig                      map[string]any
	platformKey                     string
	enabledToolsets                 []string
	disabledToolsets                []string
	resolveDisplaySetting           func(ctx context.Context, userConfig map[string]any, platformKey, setting string, fallback any) any
	progressMode                    string
	progressGrouping                string
	displaySurfaceMode              func(setting string, def bool, platformOnly []gateway.Platform, allowGeneric bool) string
	toolProgressEnabled             bool
	liveStatusMode                  string
	liveStatusAdapter               gateway.PlatformAdapter
	logModeEnabled                  bool
	logQueue                        *turnQueue
	interimAssistantMessagesEnabled bool
	thinkingEnabled                 bool
	nativeSlackTaskCards            bool
	needsProgressQueue              bool
	genericStatusPhrase             func(kind string) string
}

// Adapter that renders Slack native task cards when its opt in is set
type nativeTaskCardsAdapter interface {
	NativeTaskCardsEnabled() (bool, error)
}

// Adapter with Discord voice channels bound to text channels
type voiceTextChannelAdapter interface {
	VoiceTextChannels() map[int]string
	VoiceMixerActive(guildID int) bool
}

// Adapter carrying the Buzz reply to mode
type replyToModeAdapter interface {
	ReplyToMode() string
}

// Adapter resolving its own reply in thread mode
type replyInThreadAdapter interface {
	EffectiveReplyInThread() bool
}

// Text of a display value, empty for none
func displayText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Display, progress and status surface settings of a turn on the source's platform
func (r *GatewayRunner) runAgentDisplaySettings(ctx context.Context, source *gateway.SessionSource) (*runAgentDisplay, error) {
	policy, err := gateway.LocalPolicyForSource(ctx, r, source)
	if err != nil {
		return nil, err
	}
	var userConfig map[string]any
	var platformKey string
	var authority *gateway.SessionAuthority
	if policy != nil {
		if authority, err = gateway.ActiveAuthority(ctx, r); err != nil {
			return nil, err
		}
		if userConfig, _, err = policy.EffectiveFor(ctx, authority, false); err != nil {
			return nil, err
		}
		platformKey = policy.Platform
	} else {
		userConfig = gateway.LoadGatewayConfigMap(ctx, "")
		platformKey = platformConfigKey(source.Platform)
	}
	enabledToolsets, disabledToolsets := r.resolveTurnToolsets(ctx, userConfig, source, platformKey)
	if policy != nil {
		if enabledToolsets, err = gateway.PolicyTurnToolsets(ctx, authority, policy, userConfig, source.ChatID); err != nil {
			return nil, err
		}
	}
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	suppressPanic(func() {
		v := config.ResolveDisplaySetting(ctx, userConfig, platformKey, "tool_preview_length", 0)
		n := 0
		if pyrepr.Truthy(v) {
			parsed, err := pyint.Of(v)
			if err != nil {
				return
			}
			n = int(parsed.Int64())
		}
		tools.SetToolPreviewMaxLen(n)
	})
	suppressPanic(func() {
		tools.SetFriendlyToolLabels(pyrepr.Truthy(config.ResolveDisplaySetting(ctx, userConfig, platformKey, "friendly_tool_labels", true)))
	})
	envMode, _, err := provider.GetSecret(ctx, "ICEBATH_TOOL_PROGRESS_MODE")
	if err != nil {
		return nil, err
	}
	rawMode, explicit := config.ResolveToolProgress(ctx, userConfig, platformKey, envMode)
	progressMode := displayText(rawMode)
	progressGrouping := displayText(config.ResolveDisplaySetting(ctx, userConfig, platformKey, "tool_progress_grouping", nil))
	if progressGrouping == "" {
		progressGrouping = "accumulate"
	}
	recent := []string{}
	catalog := resolveStatusPhraseCatalog(home.Dir(ctx), userConfig, platformKey)
	displaySurfaceMode := func(setting string, def bool, platformOnly []gateway.Platform, allowGeneric bool) string {
		if len(platformOnly) > 0 {
			current := gatewayPlatformValue(source.Platform)
			for _, item := range platformOnly {
				if gatewayPlatformValue(item) == current && !hasPlatformDisplayOverride(userConfig, platformKey, setting) {
					return "off"
				}
			}
		}
		value := config.ResolveDisplaySetting(ctx, userConfig, platformKey, setting, def)
		if s, ok := value.(string); ok && strings.ToLower(strings.TrimSpace(s)) == "generic" {
			if allowGeneric {
				return "generic"
			}
			return "off"
		}
		if pyrepr.Truthy(value) {
			return "raw"
		}
		return "off"
	}
	genericStatusPhrase := func(kind string) (phrase string) {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("generic status phrase selection failed: %v", rec))
				switch kind {
				case "heartbeat", "waiting", "long_running", "status":
					phrase = i18n.T(ctx, "gateway.progress.status_fallback_long", "", nil)
				default:
					phrase = i18n.T(ctx, "gateway.progress.status_fallback_short", "", nil)
				}
			}
		}()
		return chooseStatusPhrase(kind, &recent, nil, catalog)
	}
	isWebhook := source.Platform == gateway.PlatformWebhook
	toolProgressEnabled := progressMode != "off" && progressMode != "log" && !isWebhook
	liveStatusMode := displayText(config.ResolveDisplaySetting(ctx, userConfig, platformKey, "live_status", "full"))
	var liveStatusAdapter gateway.PlatformAdapter
	if adapter != nil && adapter.Base() != nil && adapter.Base().Traits.SupportsStatusText && liveStatusMode != "off" {
		liveStatusAdapter = adapter
	}
	logModeEnabled := progressMode == "log" && !isWebhook
	mattermostOnly := []gateway.Platform{gateway.PlatformMattermost}
	interimMode := displaySurfaceMode("interim_assistant_messages", true, mattermostOnly, false)
	thinkingEnabled := displaySurfaceMode("thinking_progress", false, mattermostOnly, false) != "off"
	nativeCards := false
	if cards, ok := adapter.(nativeTaskCardsAdapter); ok && adapter != nil && source.Platform == gateway.PlatformSlack && !(explicit && progressMode == "off") {
		enabled, err := cards.NativeTaskCardsEnabled()
		if err != nil {
			runLog().Debug("Slack native task-card config check failed", "exc_info", err)
		} else {
			nativeCards = enabled
		}
	}
	disp := &runAgentDisplay{
		userConfig: userConfig, platformKey: platformKey, enabledToolsets: enabledToolsets, disabledToolsets: disabledToolsets,
		resolveDisplaySetting: config.ResolveDisplaySetting, progressMode: progressMode, progressGrouping: progressGrouping,
		displaySurfaceMode: displaySurfaceMode, toolProgressEnabled: toolProgressEnabled, liveStatusMode: liveStatusMode,
		liveStatusAdapter: liveStatusAdapter, logModeEnabled: logModeEnabled,
		interimAssistantMessagesEnabled: !isWebhook && interimMode != "off", thinkingEnabled: thinkingEnabled,
		nativeSlackTaskCards: nativeCards, needsProgressQueue: toolProgressEnabled || thinkingEnabled || nativeCards,
		genericStatusPhrase: genericStatusPhrase,
	}
	if logModeEnabled {
		disp.logQueue = newTurnQueue()
	}
	return disp, nil
}

// Guild whose voice call is bound to the source's text channel on the bot that answers it
func (r *GatewayRunner) voiceAckGuildFor(ctx context.Context, source *gateway.SessionSource) *int {
	if source.Platform != gateway.PlatformDiscord {
		return nil
	}
	adapter, _ := r.deliveryAdapterFor(ctx, source)
	voice, ok := adapter.(voiceTextChannelAdapter)
	if !ok || adapter == nil {
		return nil
	}
	bound := voice.VoiceTextChannels()
	if bound == nil {
		return nil
	}
	for _, gid := range sortedIntKeys(bound) {
		if bound[gid] == source.ChatID && voice.VoiceMixerActive(gid) {
			return &gid
		}
	}
	return nil
}

// Keys of an int keyed map in ascending order
func sortedIntKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Arguments of one agent run beyond the message and source, stored on the turn context verbatim
type turnContextParams struct {
	contextPrompt              string
	history                    []map[string]any
	sessionID                  string
	interruptDepth             int
	eventMessageID             *string
	inboundMessageID           *string
	channelPrompt              *string
	moaConfig                  map[string]any
	persistUserMessage         any
	persistUserTimestamp       *float64
	persistUserDisplayKind     *string
	persistUserDisplayMetadata map[string]any
	replyExpected              *bool
	scheduledHeartbeat         bool
	titleUserMessage           *string
	sessionEnv                 *tools.Env
	voiceTurn                  bool
}

// Turn context and turn runner of one agent run with the adapter that cleans up progress bubbles
func (r *GatewayRunner) runAgentBuildTurnContext(ctx context.Context, disp *runAgentDisplay, message string, source *gateway.SessionSource,
	sessionKey string, runGeneration *int, params turnContextParams) (*TurnContext, *TurnRunner, gateway.PlatformAdapter, error) {
	voiceAckGuild := r.voiceAckGuildFor(ctx, source)
	cleanupProgress := pyrepr.Truthy(disp.resolveDisplaySetting(ctx, disp.userConfig, disp.platformKey, "cleanup_progress", nil))
	var cleanupAdapter gateway.PlatformAdapter
	if cleanupProgress {
		cleanupAdapter, _ = r.deliveryAdapterFor(ctx, source)
	}
	if cleanupAdapter != nil && !gateway.AdapterOverridesBaseMethod(cleanupAdapter, "DeleteMessage") {
		cleanupProgress = false
		cleanupAdapter = nil
	}
	tc := newTurnContext()
	tc.source, tc.message, tc.sessionKey, tc.runGeneration = source, message, sessionKey, runGeneration
	tc.cleanupProgress = cleanupProgress
	tc.runStillCurrent = r.runStillCurrentFn(sessionKey, runGeneration)
	if disp.needsProgressQueue {
		tc.progressQueue = newTurnQueue()
	}
	tc.voiceAckGuild, tc.voiceAckLoop = voiceAckGuild, r.gatewayLoop
	tc.liveStatusAdapter, tc.liveStatusMode, tc.thinkingEnabled = disp.liveStatusAdapter, disp.liveStatusMode, disp.thinkingEnabled
	tc.progressMode, tc.progressGrouping, tc.toolProgressEnabled = disp.progressMode, disp.progressGrouping, disp.toolProgressEnabled
	tc.logQueue, tc.resolveDisplaySetting, tc.userConfig = disp.logQueue, disp.resolveDisplaySetting, disp.userConfig
	tc.enabledToolsets, tc.disabledToolsets, tc.logModeEnabled = disp.enabledToolsets, disp.disabledToolsets, disp.logModeEnabled
	tc.interimAssistantMessagesEnabled, tc.needsProgressQueue = disp.interimAssistantMessagesEnabled, disp.needsProgressQueue
	tc.nativeSlackTaskCards = disp.nativeSlackTaskCards
	tc.contextPrompt, tc.history, tc.sessionID, tc.interruptDepth = params.contextPrompt, params.history, params.sessionID, params.interruptDepth
	tc.eventMessageID, tc.inboundMessageID, tc.channelPrompt, tc.moaConfig = params.eventMessageID, params.inboundMessageID, params.channelPrompt, params.moaConfig
	tc.persistUserMessage, tc.persistUserTimestamp = params.persistUserMessage, params.persistUserTimestamp
	tc.persistUserDisplayKind, tc.persistUserDisplayMetadata = params.persistUserDisplayKind, params.persistUserDisplayMetadata
	tc.replyExpected, tc.scheduledHeartbeat, tc.titleUserMessage = params.replyExpected, params.scheduledHeartbeat, params.titleUserMessage
	tc.sessionEnv, tc.voiceTurn = params.sessionEnv, params.voiceTurn
	turnRunner, err := newTurnRunner(ctx, r, tc)
	if err != nil {
		return nil, nil, nil, err
	}
	tc.muteNotificationReply = config.DiagnosticTurnMuted(ctx, tc.persistUserDisplayMetadata, string(source.Platform), tc.userConfig)
	tc.progressCallback = turnRunner.progressCallback
	tc.voiceAckCallback = turnRunner.voiceAckCallback
	tc.nativeToolStartCallback = turnRunner.combinedToolStartCallback
	tc.nativeToolCompleteCallback = turnRunner.nativeToolCompleteCallback
	return tc, turnRunner, cleanupAdapter, nil
}

// Thread metadata of a progress lane send, the relay auto thread lane falling back to the reply anchor
func (r *GatewayRunner) threadMetadataForProgress(source *gateway.SessionSource, eventMessageID, progressThreadID, relayProspectiveThreadID *string) map[string]any {
	var metadata map[string]any
	switch {
	case progressThreadID == nil || *progressThreadID == "":
	case source.ThreadID != nil && *progressThreadID == *source.ThreadID:
		metadata = r.threadMetadataForSource(source, eventMessageID)
	default:
		metadata = r.threadMetadataForTarget(source.Platform, source.ChatID, progressThreadID, source.ChatType, eventMessageID, nil)
	}
	if metadata == nil && relayProspectiveThreadID != nil && *relayProspectiveThreadID != "" {
		var anchor any
		if eventMessageID != nil {
			anchor = *eventMessageID
		}
		metadata = map[string]any{"reply_to_message_id": anchor}
	}
	return metadata
}

// Thread or root id progress and status bubbles target, none without a thread
func resolveProgressThreadID(platform gateway.Platform, sourceThreadID, eventMessageID *string, replyInThread bool) *string {
	platformKey := strings.ToLower(string(platform))
	thread, event := optText(sourceThreadID), optText(eventMessageID)
	if !replyInThread {
		if thread != "" && event != "" && thread == event {
			return nil
		}
		if thread != "" {
			return &thread
		}
		return nil
	}
	if thread != "" {
		return &thread
	}
	if (platformKey == "slack" || platformKey == "mattermost" || platformKey == "buzz") && event != "" {
		return &event
	}
	return nil
}

// Where progress bubbles thread, their reply anchor and the thread metadata of status sends
func (r *GatewayRunner) runAgentProgressThreading(ctx context.Context, source *gateway.SessionSource, eventMessageID *string, nativeSlackTaskCards bool) (
	map[string]any, *string, map[string]any) {
	isBuzz := strings.ToLower(string(source.Platform)) == "buzz"
	replyInThread := true
	if source.Platform == gateway.PlatformSlack || isBuzz {
		if adapter, _ := r.deliveryAdapterFor(ctx, source); adapter != nil {
			suppressPanic(func() {
				switch {
				case isBuzz:
					mode := "first"
					if a, ok := adapter.(replyToModeAdapter); ok {
						mode = a.ReplyToMode()
					}
					replyInThread = mode != "off"
				default:
					if a, ok := adapter.(replyInThreadAdapter); ok {
						replyInThread = a.EffectiveReplyInThread()
						return
					}
					v, has := adapter.Base().Config.Extra["reply_in_thread"]
					if !has {
						v = true
					}
					replyInThread = pyrepr.Truthy(v)
				}
			})
		}
	}
	progressThreadID := resolveProgressThreadID(source.Platform, source.ThreadID, eventMessageID, replyInThread)
	var relayProspective *string
	if source.Platform == gateway.PlatformDiscord && source.DeliveredViaUpstreamRelay && source.ProspectiveThreadID != nil &&
		*source.ProspectiveThreadID != "" && optText(source.ThreadID) == "" {
		id := *source.ProspectiveThreadID
		relayProspective = &id
	}
	progressMetadata := nonConversationalMetadata(r.threadMetadataForProgress(source, eventMessageID, progressThreadID, relayProspective), source.Platform)
	if nativeSlackTaskCards {
		merged := map[string]any{}
		for k, v := range progressMetadata {
			merged[k] = v
		}
		if source.ScopeID != nil && *source.ScopeID != "" {
			setDefault(merged, "recipient_team_id", *source.ScopeID)
			setDefault(merged, "slack_team_id", *source.ScopeID)
		}
		if source.UserID != nil && *source.UserID != "" {
			setDefault(merged, "recipient_user_id", *source.UserID)
		}
		progressMetadata = merged
	}
	threaded := optText(source.ThreadID) != ""
	event := optText(eventMessageID)
	var progressReplyTo *string
	if ((source.Platform == gateway.PlatformFeishu || source.Platform == gateway.PlatformMattermost) && threaded && event != "") ||
		(isBuzz && event != "" && replyInThread) || relayProspective != nil {
		progressReplyTo = eventMessageID
	}
	var statusThreadMetadata map[string]any
	if source.Platform == gateway.PlatformFeishu && threaded && event != "" {
		var thread any
		if progressThreadID != nil {
			thread = *progressThreadID
		}
		statusThreadMetadata = map[string]any{"thread_id": thread, "reply_to_message_id": event}
	} else {
		statusThreadMetadata = r.threadMetadataForProgress(source, eventMessageID, progressThreadID, relayProspective)
	}
	return progressMetadata, progressReplyTo, statusThreadMetadata
}

// Sets key only when the map does not hold it yet
func setDefault(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}
