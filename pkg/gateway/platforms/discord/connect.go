package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Health reasons that confirm the transport is dead, escalated on the first strike
var terminalHealthReasons = map[string]bool{"socket_closed": true, "client_closed": true}

// Diff based slash command reconciliation, replaced by tests that patch _safe_sync_slash_commands
var safeSyncSlashCommandsFn = (*DiscordAdapter).safeSyncSlashCommands

// Discord adapter for one platform config with the instance state of a fresh connection
func NewDiscordAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*DiscordAdapter, error) {
	a := &DiscordAdapter{readyEvent: gateway.NewInterruptEvent(), allowedUserIDs: strSet{}, usernameResolvedIDs: strSet{}, allowedRoleIDs: strSet{},
		botTagDebounceUntil: map[string]float64{}, lastSelfMessageID: map[string]string{}, lastOverflowPreview: map[[2]string]string{},
		typingTasks: map[string]*typingTask{}, semanticThreadRenames: newSemanticThreadRenames(), voiceState: newVoiceState()}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformDiscord, a)
	a.handleMessage = a.BasePlatformAdapter.HandleMessage
	a.textBatchDelaySeconds = config.EnvFloat("ICEBATH_DISCORD_TEXT_BATCH_DELAY_SECONDS", 0.6)
	a.textBatchSplitDelaySeconds = config.EnvFloat("ICEBATH_DISCORD_TEXT_BATCH_SPLIT_DELAY_SECONDS", 2.0)
	a.SetTextBatchDelays(a.textBatchDelaySeconds, a.textBatchSplitDelaySeconds)
	a.voiceTimeoutSeconds = loadVoiceTimeout(ctx)
	a.playbackTimeoutSeconds = loadPlaybackTimeout(ctx)
	a.voiceFXCfg = loadVoiceFXConfig(ctx)
	a.threads = gateway.NewThreadParticipationTracker(ctx, "discord", 500)
	a.livenessIntervalSeconds = a.finitePositiveConfigFloat(ctx, "websocket_liveness_interval_seconds", 15.0, "ICEBATH_DISCORD_LIVENESS_INTERVAL_SECONDS")
	a.livenessFailureThreshold = a.livenessConfigInt(ctx, "websocket_liveness_failure_threshold", 2, "ICEBATH_DISCORD_LIVENESS_FAILURE_THRESHOLD")
	a.heartbeatAckMaxAgeSeconds = a.finitePositiveConfigFloat(ctx, "websocket_heartbeat_ack_max_age_seconds", 60.0, "")
	a.maxLatencySeconds = a.finitePositiveConfigFloat(ctx, "websocket_max_latency_seconds", 30.0, "")
	a.eventMaxSilenceSeconds = a.finitePositiveConfigFloat(ctx, "websocket_event_max_silence_seconds", 14400.0, "")
	a.recoveryStore = NewDiscordRecoveryStore(ctx, home.Dir(ctx))
	a.dedup = gateway.NewMessageDeduplicator(2000, 300)
	a.replyToMode = "first"
	if mode, ok := cfg.ReplyToMode.(string); ok && mode != "" {
		a.replyToMode = mode
	}
	a.slashCommands = true
	if v, ok := cfg.Extra["slash_commands"]; ok {
		a.slashCommands = pyrepr.Truthy(v)
	}
	a.nonconversationalMessages = newNonConversationalTracker(ctx, nonConversationalMaxTracked)
	return a, nil
}

// Connects to the Discord gateway and starts receiving events, false with a fatal error set on failure
func (a *DiscordAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if !opus.isLoaded() {
		loadOpusCodec()
	}
	if !pyrepr.Truthy(a.Config.Token) {
		slog.Error(fmt.Sprintf("[%s] No bot token configured", a.Name()))
		a.SetFatalError(ctx, "missing_credentials", "No bot token configured", false)
		return false, nil
	}
	ok, err := a.connect(ctx, pyrepr.Str(a.Config.Token))
	if err == nil {
		return ok, nil
	}
	if errors.Is(err, errDiscordReadyTimeout) {
		slog.Error(fmt.Sprintf("[%s] Timeout waiting for connection to Discord", a.Name()))
		a.cancelBotTask()
		a.ReleasePlatformLock()
		a.SetFatalError(ctx, "discord_connect_timeout", "Timed out waiting for the Discord gateway to become ready", true)
		return false, nil
	}
	slog.Error(fmt.Sprintf("[%s] Failed to connect to Discord: %s", a.Name(), err))
	a.cancelBotTask()
	a.ReleasePlatformLock()
	code, message, retryable := a.classifyConnectException(err)
	a.SetFatalError(ctx, code, message, retryable)
	return false, nil
}

// Body of connect whose errors the caller classifies into fatal codes
func (a *DiscordAdapter) connect(ctx context.Context, token string) (bool, error) {
	acquired, err := a.AcquirePlatformLock(ctx, "discord-bot-token", token, "Discord bot token")
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	a.snapshotGateEnv(ctx)
	allowedUsers := a.getAllowedUsers(ctx)
	allowedRoles, err := a.getAllowedRoles(ctx)
	if err != nil {
		return false, err
	}
	a.mu.Lock()
	a.allowedUserIDs, a.allowedRoleIDs = allowedUsers, allowedRoles
	a.mu.Unlock()
	intents := discordgo.IntentGuilds | discordgo.IntentGuildModeration | discordgo.IntentGuildEmojis | discordgo.IntentGuildIntegrations |
		discordgo.IntentGuildWebhooks | discordgo.IntentGuildInvites | discordgo.IntentGuildVoiceStates | discordgo.IntentGuildMessages |
		discordgo.IntentGuildMessageReactions | discordgo.IntentGuildMessageTyping | discordgo.IntentDirectMessages |
		discordgo.IntentDirectMessageReactions | discordgo.IntentDirectMessageTyping | discordgo.IntentGuildScheduledEvents |
		discordgo.IntentAutoModerationConfiguration | discordgo.IntentAutoModerationExecution | discordgo.IntentGuildMessagePolls |
		discordgo.IntentDirectMessagePolls | discordgo.IntentMessageContent
	if needsServerMembersIntent(keysOf(allowedUsers), keysOf(allowedRoles)) {
		intents |= discordgo.IntentGuildMembers
	}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "DISCORD_PROXY", nil, "")
	if err != nil {
		return false, err
	}
	if proxyURL != "" {
		slog.Info(fmt.Sprintf("[%s] Using proxy for Discord: %s", a.Name(), proxyURL))
	}
	a.mu.Lock()
	previous := a.link
	a.mu.Unlock()
	if previous != nil {
		if !previous.isClosed() {
			if err := previous.close(); err != nil {
				slog.Debug(fmt.Sprintf("[%s] Failed to close previous Discord client", a.Name()))
			}
		}
		a.mu.Lock()
		a.link, a.client = nil, nil
		a.mu.Unlock()
		a.readyEvent.Clear()
	}
	mentions, err := buildAllowedMentions(ctx, a.Config.Extra)
	if err != nil {
		return false, err
	}
	proxy, err := gateway.ProxyKwargsForBot(proxyURL)
	if err != nil {
		return false, err
	}
	link, err := commandsBot(token, intents, mentions, proxy)
	if err != nil {
		return false, err
	}
	runCtx := context.WithoutCancel(ctx)
	a.mu.Lock()
	a.link, a.client, a.allowedMentions, a.runCtx = link, link.Session, mentions, runCtx
	a.lastDispatchedEventMonotonic = nil
	a.mu.Unlock()
	a.registerGatewayHandlers(runCtx, link)
	if a.slashCommands {
		a.registerSlashCommands(runCtx)
	}
	a.mu.Lock()
	a.disconnecting = false
	a.mu.Unlock()
	taskCtx, cancel := context.WithCancel(runCtx)
	task := newBotTask(cancel)
	a.mu.Lock()
	a.botTask = task
	a.mu.Unlock()
	go func() {
		task.finish(link.start(taskCtx, token))
		a.handleBotTaskDone(runCtx, task)
	}()
	timeout := time.Duration(-1)
	if readyTimeout := discordReadyTimeoutSeconds(); readyTimeout > 0 {
		timeout = time.Duration(readyTimeout * float64(time.Second))
	}
	if err := waitForReadyOrBotExit(ctx, a.readyEvent.Done(), task, timeout); err != nil {
		return false, err
	}
	a.MarkConnected(ctx, "")
	a.startLivenessProbe(runCtx)
	a.WirePluginHandlers(ctx, link.Session)
	return true, nil
}

// Wires the discord.py event set onto the session handlers
func (a *DiscordAdapter) registerGatewayHandlers(ctx context.Context, link *gatewayLink) {
	s := link.Session
	pending := map[string]bool{}
	var readyTimer *time.Timer
	fire := func() {
		a.onReady(ctx, link)
	}
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		a.mu.Lock()
		clear(pending)
		for _, g := range r.Guilds {
			if g.Unavailable {
				pending[g.ID] = true
			}
		}
		if readyTimer != nil {
			readyTimer.Stop()
		}
		if len(pending) == 0 {
			a.mu.Unlock()
			fire()
			return
		}
		readyTimer = time.AfterFunc(2*time.Second, fire)
		a.mu.Unlock()
	})
	s.AddHandler(func(_ *discordgo.Session, g *discordgo.GuildCreate) {
		a.mu.Lock()
		if !pending[g.ID] {
			a.mu.Unlock()
			return
		}
		delete(pending, g.ID)
		done := len(pending) == 0
		if readyTimer != nil {
			if done {
				readyTimer.Stop()
			} else {
				readyTimer.Reset(2 * time.Second)
			}
		}
		a.mu.Unlock()
		if done {
			fire()
		}
	})
	s.AddHandler(func(_ *discordgo.Session, e *discordgo.Event) {
		now := monotonic()
		a.mu.Lock()
		a.lastDispatchedEventMonotonic = &now
		a.mu.Unlock()
		if e.Type == "THREAD_UPDATE" {
			var data map[string]any
			if json.Unmarshal(e.RawData, &data) == nil {
				id, _ := data["id"].(string)
				a.onPlatformRawThreadUpdate(id, data)
			}
		}
	})
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		a.dispatchDiscordMessage(ctx, a.inbound(m.Message))
	})
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageUpdate) {
		if m.BeforeUpdate != nil {
			a.onPlatformMessageEdit(ctx, m.BeforeUpdate, m.Message)
		}
	})
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageDelete) {
		if m.BeforeDelete != nil {
			a.onPlatformMessageDelete(ctx, m.BeforeDelete)
		}
	})
	s.AddHandler(func(_ *discordgo.Session, th *discordgo.ThreadCreate) {
		if th.NewlyCreated {
			a.onPlatformThreadCreate(ctx, th.Channel)
		}
	})
	s.AddHandler(func(_ *discordgo.Session, th *discordgo.ThreadUpdate) {
		if th.BeforeUpdate != nil {
			a.onPlatformThreadUpdate(ctx, th.BeforeUpdate, th.Channel)
		}
	})
	s.AddHandler(func(_ *discordgo.Session, v *discordgo.VoiceStateUpdate) {
		a.onVoiceStateUpdate(ctx, v.BeforeUpdate, v.VoiceState)
	})
	s.AddHandler(func(_ *discordgo.Session, i *discordgo.InteractionCreate) {
		a.onInteraction(ctx, i.Interaction)
	})
}

// Resolves username allowlist entries, marks ready and starts post connect work and recovery
func (a *DiscordAdapter) onReady(ctx context.Context, link *gatewayLink) {
	if user := link.State.User; user != nil {
		slog.Info(fmt.Sprintf("[%s] Connected as %s", a.Name(), user.String()))
	}
	a.resolveAllowedUsernames(ctx)
	a.readyEvent.Set()
	a.mu.Lock()
	if a.postConnectTask != nil && !a.postConnectTask.isDone() {
		a.postConnectTask.cancel()
	}
	a.postConnectTask = spawnTask(ctx, a.runPostConnectInitialization)
	a.mu.Unlock()
	if a.missedMessageBackfillEnabled(ctx) {
		a.ensureMissedMessageBackfillTask(ctx)
	}
}

// Starts the gateway websocket health probe unless a knob disables it or it already runs
func (a *DiscordAdapter) startLivenessProbe(ctx context.Context) {
	if a.livenessIntervalSeconds <= 0 || a.livenessFailureThreshold <= 0 || a.heartbeatAckMaxAgeSeconds <= 0 || a.maxLatencySeconds <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.livenessTask != nil && !a.livenessTask.isDone() {
		return
	}
	a.livenessTask = spawnTask(ctx, a.livenessLoop)
}

// Gateway health from ready, close, socket, heartbeat ack age, latency and dispatch silence, no REST call
func (a *DiscordAdapter) readWebsocketHealth(link *gatewayLink) (bool, string) {
	if !a.readyEvent.IsSet() {
		return false, "not_ready"
	}
	if link.isClosed() {
		return false, "client_closed"
	}
	sock := link.socket()
	if sock == nil || !sock.open.Load() {
		return false, "socket_closed"
	}
	link.RLock()
	lastAck, lastSent := link.LastHeartbeatAck, link.LastHeartbeatSent
	link.RUnlock()
	if lastAck.IsZero() {
		return false, "ack_unavailable"
	}
	ackAge := time.Since(lastAck).Seconds()
	if math.IsNaN(ackAge) || math.IsInf(ackAge, 0) || ackAge > a.heartbeatAckMaxAgeSeconds {
		return false, "ack_stale"
	}
	if lastSent.IsZero() {
		return false, "latency_non_finite"
	}
	if latency := lastAck.Sub(lastSent).Seconds(); latency > a.maxLatencySeconds {
		return false, "latency_exceeded"
	}
	if a.eventMaxSilenceSeconds > 0 {
		a.mu.Lock()
		last := a.lastDispatchedEventMonotonic
		a.mu.Unlock()
		if last != nil && monotonic()-*last > a.eventMaxSilenceSeconds {
			return false, "event_silence"
		}
	}
	return true, "healthy"
}

// Forces a reconnect after repeated unhealthy samples, at once when the transport is confirmed dead
func (a *DiscordAdapter) livenessLoop(ctx context.Context) {
	interval := time.Duration(a.livenessIntervalSeconds * float64(time.Second))
	threshold := a.livenessFailureThreshold
	failures := 0
	for a.IsConnected() {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}
		a.mu.Lock()
		link, disconnecting := a.link, a.disconnecting
		a.mu.Unlock()
		if !a.IsConnected() || link == nil || disconnecting {
			slog.Info(fmt.Sprintf("[%s] Discord liveness probe exiting (running=%s, client=%s, disconnecting=%s)", a.Name(),
				pyrepr.Repr(a.IsConnected()), pyrepr.Repr(link != nil), pyrepr.Repr(disconnecting)))
			return
		}
		healthy, reason := a.readWebsocketHealth(link)
		if healthy {
			if failures > 0 {
				slog.Info(fmt.Sprintf("[%s] Discord Gateway WebSocket healthy again after %d unhealthy sample(s)", a.Name(), failures))
			}
			failures = 0
			continue
		}
		failures++
		slog.Warn(fmt.Sprintf("[%s] Discord Gateway WebSocket unhealthy (%s, %d/%d)", a.Name(), reason, failures, threshold))
		terminal := terminalHealthReasons[reason]
		if failures < threshold && !terminal {
			continue
		}
		a.mu.Lock()
		a.disconnecting = true
		a.mu.Unlock()
		state := "remained unhealthy"
		if terminal {
			state = "transport closed"
		}
		slog.Error(fmt.Sprintf("[%s] Discord Gateway WebSocket %s (%s, %d/%d); forcing reconnect", a.Name(), state, reason, failures, threshold))
		a.SetFatalError(ctx, "discord_websocket_health_stale", "Discord Gateway WebSocket health check failed: "+reason, true)
		a.mu.Lock()
		a.livenessNotificationTask = spawnTask(context.WithoutCancel(ctx), func(c context.Context) { a.notifyLivenessFatalError(c, link) })
		a.mu.Unlock()
		return
	}
}

// Closes the failed client within a second, aborting its socket on timeout, then notifies the runner
func (a *DiscordAdapter) notifyLivenessFatalError(ctx context.Context, link *gatewayLink) {
	failed := link.socket()
	closed := make(chan error, 1)
	go func() { closed <- link.close() }()
	select {
	case err := <-closed:
		if err != nil {
			slog.Debug(fmt.Sprintf("[%s] Error closing unhealthy Discord client: %s", a.Name(), err))
		}
	case <-time.After(time.Second):
		slog.Warn(fmt.Sprintf("[%s] Timed out closing unhealthy Discord client", a.Name()))
		if failed != nil && abortDiscordWebsocketTransport(&wsTransport{conn: failed}) {
			slog.Warn(fmt.Sprintf("[%s] Aborted unresponsive Discord WebSocket transport", a.Name()))
		}
	}
	a.mu.Lock()
	a.livenessNotificationTask = nil
	a.mu.Unlock()
	if err := a.NotifyFatalError(ctx); err != nil {
		slog.Debug(fmt.Sprintf("[%s] Fatal-error handler raised: %s", a.Name(), err))
	}
}

// Cancels and awaits the liveness tasks, skipping the one the caller runs on
func (a *DiscordAdapter) cancelLivenessTask(current *bgTask) {
	for _, slot := range []**bgTask{&a.livenessTask, &a.livenessNotificationTask} {
		a.mu.Lock()
		task := *slot
		a.mu.Unlock()
		if task == nil || task == current {
			continue
		}
		task.stop()
		a.mu.Lock()
		if *slot == task {
			*slot = nil
		}
		a.mu.Unlock()
	}
}

// Flushes pending text batches within the shutdown deadline before the base cancels background tasks
func (a *DiscordAdapter) CancelBackgroundTasks(ctx context.Context) {
	if n := a.PendingTextBatchTaskCount(); n > 0 {
		slog.Info(fmt.Sprintf("[%s] Flushing %d pending text-batch task(s) before shutdown", a.Name(), n))
	}
	if a.AwaitPendingTextBatches(ctx, time.Duration(textBatchFlushDeadlineSeconds()*float64(time.Second))) {
		slog.Warn(fmt.Sprintf("[%s] Text-batch flush timed out; cancelling remaining tasks", a.Name()))
	}
	a.BasePlatformAdapter.CancelBackgroundTasks(ctx)
}

// Disconnects from Discord, leaving voice before the gateway session ends
func (a *DiscordAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.disconnecting = true
	a.mu.Unlock()
	a.cancelLivenessTask(nil)
	for _, guildID := range a.voiceGuildIDs() {
		if err := a.leaveVoiceChannel(ctx, guildID); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Error leaving voice channel %d: %s", a.Name(), guildID, err))
		}
	}
	a.cancelBotTask()
	a.mu.Lock()
	link := a.link
	a.mu.Unlock()
	if link != nil {
		if err := link.close(); err != nil {
			slog.Warn(fmt.Sprintf("[%s] Error during disconnect: %s", a.Name(), err))
		}
	}
	a.mu.Lock()
	tasks := []*bgTask{a.postConnectTask, a.missedMessageBackfillTask}
	a.mu.Unlock()
	for _, task := range tasks {
		if task != nil && !task.isDone() {
			task.stop()
		}
	}
	a.SetRunning(false)
	a.mu.Lock()
	a.link, a.client = nil, nil
	a.postConnectTask, a.livenessTask, a.missedMessageBackfillTask = nil, nil, nil
	a.mu.Unlock()
	a.readyEvent.Clear()
	a.ReleasePlatformLock()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Slash command sync after connect by policy, bulk overwrite or a fingerprinted safe reconcile
func (a *DiscordAdapter) runPostConnectInitialization(ctx context.Context) {
	a.mu.Lock()
	link := a.link
	a.mu.Unlock()
	if link == nil {
		return
	}
	err := a.postConnectSync(ctx, link)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		slog.Warn(fmt.Sprintf("[%s] Slash command sync timed out — Discord rate-limit bucket may be saturated; will retry on next reconnect", a.Name()))
	case errors.Is(err, context.Canceled):
	default:
		slog.Warn(fmt.Sprintf("[%s] Slash command sync failed: %s", a.Name(), err))
	}
}

// Command sync steps whose timeouts and failures the caller logs
func (a *DiscordAdapter) postConnectSync(ctx context.Context, link *gatewayLink) error {
	policy := a.getDiscordCommandSyncPolicy(ctx)
	if policy == "off" {
		slog.Info(fmt.Sprintf("[%s] Skipping Discord slash command sync (policy=off)", a.Name()))
		return nil
	}
	if policy == "bulk" {
		bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		appID := applicationID(link.Session)
		endpoint := discordgo.EndpointApplicationGlobalCommands(appID)
		body, err := link.RequestWithBucketID(http.MethodPut, endpoint, a.desiredCommandPayloads(), endpoint, discordgo.WithContext(bctx))
		if err != nil {
			return dpyError(err)
		}
		var synced []json.RawMessage
		if err := json.Unmarshal(body, &synced); err != nil {
			return err
		}
		slog.Info(fmt.Sprintf("[%s] Synced %d slash command(s) via bulk tree sync", a.Name(), len(synced)))
		return nil
	}
	appID := applicationID(link.Session)
	fingerprint := a.desiredCommandSyncFingerprint(ctx)
	if reason := a.commandSyncSkipReason(ctx, appID, fingerprint); reason != "" {
		slog.Info(fmt.Sprintf("[%s] Skipping Discord slash command sync: %s", a.Name(), reason))
		return nil
	}
	a.recordCommandSyncAttempt(ctx, appID, fingerprint)
	previous := link.rateCap.swap(commandSyncMaxRateLimitSleepSeconds)
	sctx, cancel := context.WithTimeout(ctx, 600*time.Second)
	summary, err := safeSyncSlashCommandsFn(a, sctx)
	cancel()
	link.rateCap.swap(previous)
	if err != nil {
		if !isDiscordRateLimit(err) {
			return err
		}
		retryAfter := commandSyncMaxRateLimitSleepSeconds
		if v := extractDiscordRetryAfter(err); v != nil {
			retryAfter = *v
		}
		a.recordCommandSyncRateLimit(ctx, appID, fingerprint, retryAfter)
		slog.Warn(fmt.Sprintf("[%s] Discord rate-limited slash command sync; retrying after %.0fs", a.Name(), retryAfter))
		return nil
	}
	a.recordCommandSyncSuccess(ctx, appID, fingerprint, summary)
	slog.Info(fmt.Sprintf("[%s] Safely reconciled %d slash command(s): unchanged=%d updated=%d recreated=%d created=%d deleted=%d",
		a.Name(), summary["total"], summary["unchanged"], summary["updated"], summary["recreated"], summary["created"], summary["deleted"]))
	return nil
}

// Desired command payloads as one JSON array for the bulk overwrite
func (a *DiscordAdapter) desiredCommandPayloads() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.desiredCommands...)
}
