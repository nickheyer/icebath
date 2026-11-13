package gatewayrun

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Copy of the source with only its dataclass fields carried and the edit applied, normalized again
func replacedSource(source *gateway.SessionSource, edit func(*gateway.SessionSource)) *gateway.SessionSource {
	copied := &gateway.SessionSource{Platform: source.Platform, ChatID: source.ChatID, ChatName: source.ChatName,
		ChatType: source.ChatType, UserID: source.UserID, UserName: source.UserName, ThreadID: source.ThreadID,
		ChatTopic: source.ChatTopic, UserIDAlt: source.UserIDAlt, ChatIDAlt: source.ChatIDAlt, IsBot: source.IsBot,
		ScopeID: source.ScopeID, GuildID: source.GuildID, ParentChatID: source.ParentChatID, MessageID: source.MessageID,
		RoleAuthorized: source.RoleAuthorized, Profile: source.Profile, ProfileRouteRejected: source.ProfileRouteRejected,
		AutoThreadCreated: source.AutoThreadCreated, AutoThreadInitialName: source.AutoThreadInitialName,
		ProspectiveThreadID: source.ProspectiveThreadID, DeliveredViaUpstreamRelay: source.DeliveredViaUpstreamRelay,
		DetachedRoute: source.DetachedRoute}
	if edit != nil {
		edit(copied)
	}
	return copied.Normalize()
}

// Interrupts the chat's running turn and force cleans its session lock, keeping the session itself
func (r *GatewayRunner) handleStopCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	source := event.Source
	entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return nil, err
	}
	sessionKey := entry.SessionKey
	if err := r.hmStopOwnerTurn(ctx, sessionKey); err != nil {
		return nil, err
	}
	stop := func(key, invalidationReason string) error {
		return r.interruptAndClearSession(ctx, key, source, interruptReasonStop, invalidationReason, true)
	}
	runKey, err := gateway.ChatRunningKey(ctx, r, sessionKey, entry)
	if err != nil {
		return nil, err
	}
	running, _ := r.runningAgents().Get(runKey)
	if running == agentPendingSentinel {
		if err := stop(runKey, "stop_command_pending"); err != nil {
			return nil, err
		}
		runLog().Info(fmt.Sprintf("STOP (pending) for session %s — sentinel cleared", runKey))
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.stopped_pending", "", nil), nil), nil
	}
	if running != nil {
		if err := stop(runKey, "stop_command_handler"); err != nil {
			return nil, err
		}
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.stopped", "", nil), nil), nil
	}
	runs, err := r.sameChatRuns(ctx, source, sessionKey)
	if err != nil {
		return nil, err
	}
	siblingKeys := siblingThreadRunKeys(source, runs)
	fallbackKeys := chatScopedRunKeys(source, runs)
	reason := "stop_command_chat_scope"
	if slices.Equal(fallbackKeys, siblingKeys) {
		reason = "stop_command_thread_sibling"
	}
	if len(fallbackKeys) > 0 {
		authorized, err := r.isUserAuthorizedForSource(ctx, source, true)
		if err != nil {
			return nil, err
		}
		if authorized {
			for _, key := range fallbackKeys {
				if err := stop(key, reason); err != nil {
					return nil, err
				}
			}
			runLog().Info(fmt.Sprintf("STOP (%s) by %s — interrupted %d run(s): %s", reason, sessionKey, len(fallbackKeys),
				strings.Join(fallbackKeys, ", ")))
			return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.stopped", "", nil), nil), nil
		}
	}
	if tools.InterruptAsyncDelegationsForSession(sessionKey, "", entry.SessionID, "stop_command") > 0 {
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.stopped", "", nil), nil), nil
	}
	if r.adapters != nil {
		if adapter, ok := r.adapters.Get(source.Platform); ok && adapter != nil {
			if err := gateway.StopTypingWithMetadata(ctx, adapter, source.ChatID, r.replyMetadata(event)); err != nil {
				runLog().Debug("Failed to clear typing on /stop with no active agent", "error", err)
			}
		}
	}
	return i18n.T(ctx, "gateway.stop.no_active", "", nil), nil
}

// Lists failed and paused adapters or pauses and resumes the reconnect of one by name
func (r *GatewayRunner) handlePlatformCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	parts := pystr.SplitWhitespace(pystr.Strip(event.Text), 2)
	if len(parts) > 0 && strings.HasPrefix(pystr.LstripChars(pystr.Lower(parts[0]), "/"), "platform") {
		parts = parts[1:]
	}
	action := "list"
	if len(parts) > 0 {
		action = pystr.Lower(parts[0])
	}
	target := ""
	if len(parts) > 1 {
		target = pystr.Lower(parts[1])
	}
	failed := map[gateway.Platform]map[string]any{}
	var failedOrder []gateway.Platform
	if r.failedPlatforms != nil {
		for _, p := range r.failedPlatforms.Keys() {
			info, _ := r.failedPlatforms.Get(p)
			failed[p] = info
			failedOrder = append(failedOrder, p)
		}
	}
	if action == "list" {
		var names []string
		if r.adapters != nil {
			for p := range r.adapters.All() {
				names = append(names, string(p))
			}
		}
		sort.Strings(names)
		connected := strings.Join(names, ", ")
		if connected == "" {
			connected = i18n.T(ctx, "gateway.shared.none_marker", "", nil)
		}
		lines := []string{i18n.T(ctx, "gateway.platform.header", "", nil),
			i18n.T(ctx, "gateway.platform.connected", "", map[string]any{"platforms": connected})}
		for _, p := range failedOrder {
			info := failed[p]
			if pyrepr.Truthy(info["paused"]) {
				reason := info["pause_reason"]
				if !pyrepr.Truthy(reason) {
					reason = i18n.T(ctx, "gateway.platform.reason_default", "", nil)
				}
				lines = append(lines, i18n.T(ctx, "gateway.platform.item_paused", "", map[string]any{"name": string(p), "reason": reason}))
			} else {
				attempts, ok := info["attempts"]
				if !ok {
					attempts = 0
				}
				lines = append(lines, i18n.T(ctx, "gateway.platform.item_retrying", "", map[string]any{"name": string(p), "attempts": attempts}))
			}
		}
		if len(failed) == 0 {
			lines = append(lines, i18n.T(ctx, "gateway.platform.none_failed", "", nil))
		}
		return strings.Join(lines, "\n"), nil
	}
	if action != "pause" && action != "resume" {
		return i18n.T(ctx, "gateway.platform.usage", "", nil), nil
	}
	if target == "" {
		return i18n.T(ctx, "gateway.platform.usage_action", "", map[string]any{"action": action}), nil
	}
	var platform gateway.Platform
	for _, p := range gateway.PlatformMembers() {
		if pystr.Lower(string(p)) == target {
			platform = p
			break
		}
	}
	if platform == "" {
		return i18n.T(ctx, "gateway.platform.unknown", "", map[string]any{"name": target}), nil
	}
	name := string(platform)
	info, queued := failed[platform]
	paused := queued && pyrepr.Truthy(info["paused"])
	if action == "pause" {
		if !queued {
			return i18n.T(ctx, "gateway.platform.not_queued_pause", "", map[string]any{"name": name}), nil
		}
		if paused {
			return i18n.T(ctx, "gateway.platform.already_paused", "", map[string]any{"name": name}), nil
		}
		r.pauseFailedPlatform(ctx, platform, i18n.T(ctx, "gateway.platform.reason_manual", "", nil))
		return i18n.T(ctx, "gateway.platform.paused", "", map[string]any{"name": name}), nil
	}
	if !queued {
		return i18n.T(ctx, "gateway.platform.not_queued_resume", "", map[string]any{"name": name}), nil
	}
	if !paused {
		return i18n.T(ctx, "gateway.platform.already_retrying", "", map[string]any{"name": name}), nil
	}
	r.resumePausedPlatform(ctx, platform)
	return i18n.T(ctx, "gateway.platform.resumed", "", map[string]any{"name": name}), nil
}

// Records the restart requester and dedup marker, then drains active work and restarts the gateway
func (r *GatewayRunner) handleRestartCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	if r.isStaleRestartRedelivery(event) {
		platform := "?"
		if event.Source != nil && event.Source.Platform != "" {
			platform = string(event.Source.Platform)
		}
		updateID := "None"
		if event.PlatformUpdateID != nil {
			updateID = fmt.Sprint(*event.PlatformUpdateID)
		}
		runLog().Info(fmt.Sprintf("Ignoring redelivered /restart (platform=%s, update_id=%s) — "+
			"already processed by a previous gateway instance.", platform, updateID))
		return "", nil
	}
	if r.restartRequested || r.draining.Load() {
		if count := r.runningAgentCount(); count > 0 {
			return i18n.T(ctx, "gateway.draining", "", map[string]any{"count": count}), nil
		}
		return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.restart.in_progress", "", nil), nil), nil
	}
	hermesHome := home.Dir(ctx)
	writeMarker := func(name string, build func() map[string]any, label string) {
		if err := config.AtomicJSONWriteWith(ctx, filepath.Join(hermesHome, name), build()); err != nil {
			runLog().Debug(fmt.Sprintf("Failed to write restart %s: %s", label, pyrepr.ExceptionMessage(err)))
		}
	}
	notifyPayload := func() map[string]any {
		data := restartNotifyPayload(event)
		mid := event.Source.MessageID
		if event.MessageID != nil {
			text := *event.MessageID
			mid = &text
		}
		r.restartCommandSource = replacedSource(event.Source, func(s *gateway.SessionSource) { s.MessageID = mid })
		return data
	}
	dedupPayload := func() map[string]any {
		var platform any
		if event.Source.Platform != "" {
			platform = string(event.Source.Platform)
		}
		data := map[string]any{"platform": platform, "requested_at": float64(time.Now().UnixNano()) / 1e9}
		if event.PlatformUpdateID != nil {
			data["update_id"] = *event.PlatformUpdateID
		}
		return data
	}
	writeMarker(".restart_notify.json", notifyPayload, "notify file")
	writeMarker(".restart_last_processed.json", dedupPayload, "dedup marker")
	activeAgents := r.runningAgentCount()
	viaService := gateway.IsGatewaySupervisorProcess(nil) || gateway.IsContainerRestartContext()
	r.RequestRestart(!viaService, viaService)
	if activeAgents > 0 {
		return i18n.T(ctx, "gateway.draining", "", map[string]any{"count": activeAgents}), nil
	}
	return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.restart.restarting", "", nil), nil), nil
}

// Low disk auto prune decision text, clearing a local session's sticky notice once it is answered
func (r *GatewayRunner) handleAutopruneCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	reply, err := execute(ctx, "autoprune", event.GetCommandArgs(), nil)
	if err != nil {
		return "", err
	}
	if pystr.Strip(event.GetCommandArgs()) != "" && state.AutoPruneAnswered(ctx, "") {
		if err := clearLocalAutoPruneNotice(ctx, r, event.Source); err != nil {
			return "", err
		}
	}
	return reply.Text, nil
}

// Allowed commands of a gated non admin caller for the catalog, empty for admins and ungated callers
func (r *GatewayRunner) catalogOptions(event *gateway.MessageEvent) map[string]any {
	source := event.Source
	policy := gateway.PolicyForRunnerSource(r, source)
	userID := ""
	if source != nil && source.UserID != nil {
		userID = *source.UserID
	}
	if policy.Enabled && !policy.IsAdmin(userID) {
		allowed := map[string]bool{"help": true, "whoami": true}
		for c := range policy.UserAllowedCommands {
			allowed[c] = true
		}
		return map[string]any{"allowed_commands": allowed}
	}
	return map[string]any{}
}

// Available commands, limited to the caller's runnable set when gated
func (r *GatewayRunner) handleHelpCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	reply, err := execute(ctx, "help", "", r.catalogOptions(event))
	if err != nil {
		return "", err
	}
	return telegramizedCommandReply(event, reply.Text), nil
}

// Paged command catalog, shorter pages on Telegram
func (r *GatewayRunner) handleCommandsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	pageSize := 20
	if event.Source.Platform == gateway.PlatformTelegram {
		pageSize = 15
	}
	options := map[string]any{"page_size": pageSize}
	for k, v := range r.catalogOptions(event) {
		options[k] = v
	}
	reply, err := execute(ctx, "commands", event.GetCommandArgs(), options)
	if err != nil {
		return "", err
	}
	return telegramizedCommandReply(event, reply.Text), nil
}

// Makes the current chat the platform home channel in config, legacy env and the running config
func (r *GatewayRunner) handleSetHomeCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	platformName := "unknown"
	if source.Platform != "" {
		platformName = string(source.Platform)
	}
	chatID := source.ChatID
	chatName := chatID
	if source.ChatName != nil && *source.ChatName != "" {
		chatName = *source.ChatName
	}
	saveFailed := func(errText string) string {
		return i18n.T(ctx, "gateway.set_home.save_failed", "", map[string]any{"error": errText})
	}
	if source.Platform == "" {
		return saveFailed(i18n.T(ctx, "gateway.set_home.err_missing_platform", "", nil)), nil
	}
	viaRelay := source.DeliveredViaUpstreamRelay
	if viaRelay {
		relayAdapter, err := r.intakeAdapterFor(ctx, source)
		if err != nil {
			return "", err
		}
		fronter, canFront := relayAdapter.(interface{ FrontsPlatform(p gateway.Platform) bool })
		if source.Platform == gateway.PlatformLocal || source.Platform == gateway.PlatformRelay ||
			source.UserID == nil || *source.UserID == "" || relayAdapter == nil || !canFront || !fronter.FrontsPlatform(source.Platform) {
			return saveFailed(i18n.T(ctx, "gateway.set_home.err_relay_unauthenticated", "", nil)), nil
		}
	}
	threadID := homeThreadFromSource(source)
	optional := func(v *string) *string {
		if v == nil || *v == "" {
			return nil
		}
		text := *v
		return &text
	}
	homeChannel := &gateway.HomeChannel{Platform: source.Platform, ChatID: chatID, Name: chatName, ThreadID: threadID,
		UserID: optional(source.UserID), ScopeID: optional(source.ScopeID)}
	homeChannel.PostInit()
	if err := gateway.PersistHomeChannel(ctx, homeChannel, !viaRelay); err != nil {
		return saveFailed(pyrepr.ExceptionMessage(err)), nil
	}
	threadText := ""
	if threadID != nil {
		threadText = *threadID
	}
	if err := func() error {
		if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), homeTargetEnvVar(ctx, platformName), chatID); err != nil {
			return err
		}
		return config.SaveEnvValueContext(ctx, home.Dir(ctx), homeThreadEnvVar(ctx, platformName), threadText)
	}(); err != nil {
		runLog().Warn(fmt.Sprintf("Home config saved but legacy env persistence failed: %s", pyrepr.ExceptionMessage(err)))
	}
	platformConfig := r.config.Platforms[source.Platform]
	if platformConfig == nil {
		platformConfig = gateway.NewPlatformConfig()
		platformConfig.Enabled = !viaRelay
		r.config.SetPlatform(source.Platform, platformConfig)
	}
	platformConfig.HomeChannel = homeChannel
	return i18n.T(ctx, "gateway.set_home.success", "", map[string]any{"name": chatName, "chat_id": chatID}), nil
}
