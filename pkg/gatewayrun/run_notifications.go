package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	notifyVideoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
	notifyImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
	// Routing fields copied verbatim from a process watcher onto its synthetic completion event
	watcherRouteFields = []string{"session_key", "platform", "chat_type", "chat_id", "thread_id", "user_id", "user_name"}
	// Storage causes that clear on their own, every other cause gets the operator restart tail
	selfClearingStorageCauses = map[string]bool{"compression": true, "compression_closed": true, "turn_lease": true}
	// Coalescing keys of process completions and of async delegation groups
	completionBatchKeyFields = []string{"session_key", "platform", "chat_type", "chat_id", "thread_id", "user_id"}
	asyncGroupKeyFields      = []string{"session_key", "parent_session_id", "task_failure_notice", "platform", "chat_type", "chat_id", "thread_id", "user_id"}
)

const (
	maxCompletionInjectionAttempts   = 8
	completionInjectionRetryMaxDelay = 60
	watchInjectionRetryBaseDelay     = 4.0
)

// One durable async delegation claim transition and the log line of its failure
type durableClaimOp struct {
	fn      func(env *tools.Env, delegationID, claimID string) (bool, error)
	failMsg string
}

// Durable claim transitions by kind
var durableClaimOps = map[string]durableClaimOp{
	"drop":     {tools.DropCompletionDelivery, "Could not drop durable completion claim"},
	"release":  {tools.ReleaseCompletionDelivery, "Could not release durable completion claim"},
	"defer":    {tools.DeferCompletionDelivery, "Could not defer unadmitted completion claim"},
	"complete": {tools.CompleteCompletionDelivery, "Could not acknowledge durable completion claim"},
}

// Dedupe key of one notice destination, thread platforms routing a shared chat apart
type noticeTargetKey struct {
	platform string
	chatID   string
	threadID string
	threaded bool
}

// Key of a notice destination, the thread part absent for a falsy thread
func noticeTargetKeyOf(platformValue string, chatID, threadID any) noticeTargetKey {
	key := noticeTargetKey{platform: platformValue, chatID: pyrepr.Str(chatID)}
	if pyrepr.Truthy(threadID) {
		key.threadID, key.threaded = pyrepr.Str(threadID), true
	}
	return key
}

// List form of a key as the planned restart marker persists it
func (k noticeTargetKey) list() []any {
	if k.threaded {
		return []any{k.platform, k.chatID, k.threadID}
	}
	return []any{k.platform, k.chatID, nil}
}

// Key of one delivered chat, Telegram private chats kept apart per served profile
func deliveryTargetKey(platformValue string, chatID, threadID any, profile *string) noticeTargetKey {
	if profile != nil && *profile != "" && *profile != "default" && platformValue == "telegram" && gateway.LooksLikeTelegramPrivateChatID(pyrepr.Str(chatID)) {
		platformValue = *profile + ":" + platformValue
	}
	return noticeTargetKeyOf(platformValue, chatID, threadID)
}

// Notice key of a served profile's home channel, the launch profile keeping the bare platform
func servedNoticeTargetKey(profile *string, platformValue string, chatID, threadID any) noticeTargetKey {
	if profile == nil {
		return noticeTargetKeyOf(platformValue, chatID, threadID)
	}
	return noticeTargetKeyOf(*profile+":"+platformValue, chatID, threadID)
}

// Config list or comma separated scalar as a set of stripped non empty strings
func csvOrListToSet(raw any) map[string]bool {
	out := map[string]bool{}
	if raw == nil {
		return out
	}
	if items, ok := raw.([]any); ok {
		for _, part := range items {
			if text := pystr.Strip(pyrepr.Str(part)); text != "" {
				out[text] = true
			}
		}
		return out
	}
	for _, part := range strings.Split(pyrepr.Str(raw), ",") {
		if text := pystr.Strip(part); text != "" {
			out[text] = true
		}
	}
	return out
}

// Slack channels the gateway never dispatches, the routed adapter's list authoritative when given
func slackIgnoredChannelsFromGatewayConfig(ctx context.Context, cfg *gateway.GatewayConfig, adapter gateway.PlatformAdapter) map[string]bool {
	var raw any
	if adapter != nil {
		if base := adapter.Base(); base != nil && base.Config != nil {
			raw = base.Config.Extra["ignored_channels"]
		}
	}
	if raw == nil && adapter == nil && cfg != nil {
		if platformCfg := cfg.Platforms[gateway.PlatformSlack]; platformCfg != nil {
			raw = platformCfg.Extra["ignored_channels"]
		}
	}
	if raw == nil {
		if env := gateway.PlatformGateEnv(ctx, "SLACK_IGNORED_CHANNELS", ""); env != "" {
			raw = env
		}
	}
	return csvOrListToSet(raw)
}

// Parent Slack channel of a possibly thread scoped chat id
func slackParentChannelID(chatID string) string {
	if chatID == "" {
		return ""
	}
	return strings.SplitN(chatID, ":", 2)[0]
}

// Whether the Slack blacklist names the chat's channel
func isSlackIgnoredChannel(ctx context.Context, cfg *gateway.GatewayConfig, chatID string, adapter gateway.PlatformAdapter) bool {
	channel := slackParentChannelID(chatID)
	ignored := slackIgnoredChannelsFromGatewayConfig(ctx, cfg, adapter)
	return channel != "" && (ignored["*"] || ignored[channel])
}

// Monotonic time a refused queued notice is next offered, exponential from the drain base and capped
func refusalRetryAt(step int) float64 {
	delay := provider.JitteredBackoff(step, provider.BackoffOptions{BaseDelay: time.Duration(watchInjectionRetryBaseDelay * float64(time.Second)),
		MaxDelay: completionInjectionRetryMaxDelay * time.Second, JitterRatio: 0})
	return pythread.Monotonic() + delay.Seconds()
}

// Why a completion has no transport, naming the bot or saved route that is missing
func heldReason(source *gateway.SessionSource, platform string, multiplexed bool) string {
	identity := gateway.IdentityOf(source)
	runtime := "default"
	if source.Profile != nil && *source.Profile != "" {
		runtime = *source.Profile
	}
	if identity != nil && identity.Multiplexed && !identity.TransportInferred {
		return fmt.Sprintf("the %s bot of profile %s that received this chat (runtime profile %s) is not connected; the row stays pending until it reconnects",
			platform, pyrepr.Repr(identity.TransportProfile), pyrepr.Repr(runtime))
	}
	if multiplexed {
		return fmt.Sprintf("the %s route of profile %s has no saved receiving bot and no shared bot covers it; the row stays pending until a new message in that chat records the bot",
			platform, pyrepr.Repr(runtime))
	}
	return fmt.Sprintf("no live %s transport for profile %s; the row stays pending until it connects", platform, pyrepr.Repr(runtime))
}

// Session id of a raw API route event, empty for structured or partial messaging routes
func rawProcessEventSessionID(evt map[string]any) string {
	sessionKey := pystr.Strip(truthyText(evt["session_key"]))
	platform := pystr.Lower(pystr.Strip(truthyText(evt["platform"])))
	if strings.HasPrefix(sessionKey, "agent:") || (platform != "" && platform != "api_server") {
		return ""
	}
	if platform == "" {
		for _, field := range []string{"chat_id", "chat_type", "thread_id"} {
			if pyrepr.Truthy(evt[field]) {
				return ""
			}
		}
	}
	if origin := truthyText(evt["origin_session_id"]); origin != "" {
		return pystr.Strip(origin)
	}
	return pystr.Strip(sessionKey)
}

// Card of a local process notice, empty for a watch match
func localCompletionCard(evt map[string]any) map[string]string {
	evtType, ok := evt["type"]
	if !ok {
		evtType = "completion"
	}
	switch evtType {
	case "completion":
		text := truthyText(evt["_display_text"])
		if text == "" {
			text = tools.ProcessCompletionDisplayText([]map[string]any{evt})
		}
		return map[string]string{"display_kind": tools.ProcessCompleteDisplayKind, "display_text": text}
	case "async_delegation":
		return map[string]string{"display_kind": "async_delegation_complete", "display_text": tools.AsyncDelegationDisplayText(evt)}
	case "heartbeat":
		return map[string]string{"display_kind": gateway.InternalNotificationDisplayKind, "display_text": tools.HeartbeatDisplayText(evt)}
	}
	return map[string]string{}
}

// Synthetic event metadata of a watch or completion notification
func watchNotificationMetadata(evt map[string]any, source *gateway.SessionSource) map[string]any {
	metadata := map[string]any{"notification_origin": "process_registry_synthetic"}
	sessionKey := pystr.Strip(truthyText(evt["session_key"]))
	if agent.DiagnosticProcessEvent(evt) {
		metadata["notification_category"] = "diagnostic"
	}
	if strings.HasPrefix(sessionKey, "agent:") {
		metadata["gateway_session_key"] = sessionKey
	}
	if parent := pystr.Strip(truthyText(evt["parent_session_id"])); parent != "" {
		metadata["gateway_session_id"] = parent
	}
	if pyrepr.Truthy(evt["_automation_identities"]) {
		metadata["automation_identities"] = evt["_automation_identities"]
	}
	if source.Platform == gateway.PlatformLocal {
		for k, v := range localCompletionCard(evt) {
			metadata[k] = v
		}
	}
	if proven := pystr.Strip(truthyText(evt["proven_session_id"])); proven != "" {
		metadata["gateway_proven_session_id"] = proven
	}
	if ids := evt["completion_ids"]; pyrepr.Truthy(ids) {
		metadata["gateway_completion_ids"] = pyListOf(ids)
	}
	return metadata
}

// Python list of an iterable value
func pyListOf(v any) []any {
	switch items := v.(type) {
	case []any:
		return append([]any(nil), items...)
	case []string:
		out := make([]any, len(items))
		for i, s := range items {
			out[i] = s
		}
		return out
	}
	return []any{v}
}

// Pre flight outcome of one completion delivery
type completionClaim struct {
	delegationID    string
	claimID         string
	proceed         bool
	earlyResult     *bool
	ownerSessionID  string
	provenSessionID string
	deferred        bool
}

// Claim that proceeds with nothing settled
func newCompletionClaim() *completionClaim { return &completionClaim{proceed: true} }

// Sends a setup or operational notice by the platform's privacy rules
func (r *GatewayRunner) deliverPlatformNotice(ctx context.Context, source *gateway.SessionSource, content string) error {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter == nil {
		return nil
	}
	cfg := r.config
	if cfg != nil && source.Platform == gateway.PlatformSlack && isSlackIgnoredChannel(ctx, cfg, source.ChatID, adapter) {
		inboundLog().Info(fmt.Sprintf("Skipping Slack platform notice for configured ignored channel %s", source.ChatID))
		return nil
	}
	noticeDelivery := "public"
	var extra map[string]any
	if base := adapter.Base(); base != nil && base.Config != nil {
		extra = base.Config.Extra
	}
	if raw, ok := extra["notice_delivery"]; ok && extra != nil {
		noticeDelivery = gateway.NormalizeChoice(raw, map[string]bool{"public": true, "private": true}, "public")
	} else if cfg != nil {
		noticeDelivery = cfg.GetNoticeDelivery(source.Platform)
	}
	metadata := r.threadMetadataForSource(source, nil)
	if noticeDelivery == "private" && source.UserID != nil && *source.UserID != "" {
		result, err := adapter.SendPrivateNotice(ctx, source.ChatID, source.UserID, content, "", metadata)
		if err != nil {
			inboundLog().Debug(fmt.Sprintf("[%s] send_private_notice failed, falling back to public", source.Platform), "error", err)
		} else if result.Success {
			return nil
		}
	}
	_, err = adapter.Send(ctx, source.ChatID, content, "", metadata)
	return err
}

// Delivers the explicit attachments of an already streamed response
func (r *GatewayRunner) deliverMediaFromResponse(ctx context.Context, response string, event *gateway.MessageEvent, adapter gateway.PlatformAdapter, threadMetadata map[string]any) {
	err := func() error {
		forceDocument := strings.Contains(response, "[[as_document]]")
		mediaFiles, cleaned, err := gateway.ExtractMedia(ctx, response)
		if err != nil {
			return err
		}
		if mediaFiles, err = gateway.FilterMediaDeliveryPaths(ctx, mediaFiles); err != nil {
			return err
		}
		urlImages, _ := gateway.ExtractImages(cleaned)
		var meta map[string]any
		if threadMetadata != nil {
			meta = make(map[string]any, len(threadMetadata))
			for k, v := range threadMetadata {
				meta[k] = v
			}
		} else {
			meta = r.threadMetadataForSource(event.Source, gateway.ReplyAnchorForEvent(event))
		}
		chatID := event.Source.ChatID
		isPhoto := func(m gateway.MediaRef) bool {
			return notifyImageExts[strings.ToLower(filepath.Ext(m.Path))] && !m.IsVoice && !forceDocument
		}
		var images []gateway.ImageRef
		var others []gateway.MediaRef
		for _, m := range mediaFiles {
			if isPhoto(m) {
				images = append(images, gateway.ImageRef{URL: "file://" + pyurl.Quote(m.Path, "/")})
			} else {
				others = append(others, m)
			}
		}
		if len(images) > 0 || len(urlImages) > 0 {
			images = append(images, urlImages...)
			if _, err := adapter.SendMultipleImages(ctx, chatID, images, meta, 0); err != nil {
				inboundLog().Warn(fmt.Sprintf("[%s] Post-stream image batch delivery failed: %s", adapter.Name(), err))
			}
		}
		for _, m := range others {
			ext := strings.ToLower(filepath.Ext(m.Path))
			var err error
			switch {
			case gateway.ShouldSendMediaAsAudio(event.Source.Platform, ext, m.IsVoice):
				_, err = adapter.SendVoice(ctx, chatID, m.Path, "", "", meta, map[string]any{"is_voice": m.IsVoice})
			case notifyVideoExts[ext]:
				_, err = adapter.SendVideo(ctx, chatID, m.Path, "", "", meta, nil)
			default:
				_, err = adapter.SendDocument(ctx, chatID, m.Path, "", "", "", meta, nil)
			}
			if err != nil {
				inboundLog().Warn(fmt.Sprintf("[%s] Post-stream media delivery failed: %s", adapter.Name(), err))
			}
		}
		return nil
	}()
	logSuppressed(slog.LevelWarn, err, "Post-stream media extraction failed: %s")
}

// Visible text of a response before a direct send, attachments and delivery markers removed
func stripResponseAttachmentsForDirectSend(ctx context.Context, response string) (string, error) {
	_, cleaned, err := gateway.ExtractMedia(ctx, response)
	if err != nil {
		return "", err
	}
	_, cleaned = gateway.ExtractImages(cleaned)
	cleaned = strings.ReplaceAll(strings.ReplaceAll(cleaned, "[[audio_as_voice]]", ""), "[[as_document]]", "")
	return pystr.Strip(cleaned), nil
}

// Stream consumer state a queued final reconciles against, the message it sealed and its split delivery flag
type queuedStreamConsumer interface {
	MessageID() string
	TurnSplitDelivery() bool
}

// Delivers a queued response as text then attachments, false when the text send was refused
func (r *GatewayRunner) deliverQueuedFirstResponse(ctx context.Context, response string, source *gateway.SessionSource, adapter gateway.PlatformAdapter,
	metadata map[string]any, eventMessageID *string, textAlreadyDelivered, deliverMedia bool, consumer queuedStreamConsumer,
	sessionKey string, inboundMessageID *string) (bool, error) {
	if !textAlreadyDelivered {
		text, err := stripResponseAttachmentsForDirectSend(ctx, response)
		if err != nil {
			return false, err
		}
		if text != "" {
			reconciled := false
			sealed := ""
			if consumer != nil {
				sealed = consumer.MessageID()
			}
			if sealed != "" && sealed != "__no_edit__" && !consumer.TurnSplitDelivery() {
				edit, err := adapter.EditMessage(ctx, source.ChatID, sealed, text, true)
				if err != nil {
					inboundLog().Debug(fmt.Sprintf("Queued-lane reconcile edit failed (%s); falling back to send.", err))
				} else if edit.Success {
					reconciled = true
					inboundLog().Info(fmt.Sprintf("Queued-lane final reconciled by editing message %s in place (no duplicate send).", sealed))
				} else if gateway.DeclinedSend(&edit) {
					inboundLog().Warn("Queued-lane reconcile edit DECLINED by the connector's egress guard; not falling back to a send (the destination is not approved).")
					return true, nil
				}
			}
			if !reconciled {
				sent, err := r.sendQueuedFinalText(ctx, adapter, source, text, metadata, eventMessageID, sessionKey, inboundMessageID)
				if err != nil {
					return false, err
				}
				if !sent.Success {
					return false, nil
				}
			}
		}
	}
	if !deliverMedia {
		return true, nil
	}
	r.deliverMediaFromResponse(ctx, response, &gateway.MessageEvent{Text: "", Source: source, MessageID: eventMessageID}, adapter, metadata)
	return true, nil
}

// Sends a queued final through the ledger bracket of the normal final when a session key is known
func (r *GatewayRunner) sendQueuedFinalText(ctx context.Context, adapter gateway.PlatformAdapter, source *gateway.SessionSource, text string,
	metadata map[string]any, eventMessageID *string, sessionKey string, inboundMessageID *string) (gateway.SendResult, error) {
	var result gateway.SendResult
	var err error
	if base := adapter.Base(); sessionKey != "" && base != nil {
		result, _, err = base.SendFinalLedgered(ctx, &gateway.MessageEvent{Text: "", Source: source, LedgerMessageID: inboundMessageID},
			sessionKey, text, gateway.MarkNotifyMetadata(metadata), eventMessageID, false)
	} else {
		result, err = adapter.Send(ctx, source.ChatID, text, "", metadata)
	}
	if err != nil {
		return result, err
	}
	if !result.Success {
		reason := truthyText(result.Error)
		if reason == "" {
			reason = "no result"
		}
		inboundLog().Warn(fmt.Sprintf("Queued-lane final send to %s failed: %s", source.ChatID, reason))
	}
	return result, nil
}

// Restart notice target that was delivered
type restartNoticeTarget struct {
	platform string
	chatID   string
	threadID *string
}

// Tells the chat that requested the restart the gateway is back, the delivered target or nil
func (r *GatewayRunner) sendRestartNotification(ctx context.Context) *restartNoticeTarget {
	notifyPath := filepath.Join(home.Dir(ctx), ".restart_notify.json")
	if !updatePathExists(notifyPath) {
		return nil
	}
	defer os.Remove(notifyPath)
	target, err := r.deliverRestartNotification(ctx, notifyPath)
	if err != nil {
		inboundLog().Warn(fmt.Sprintf("Restart notification failed: %s", err))
		return nil
	}
	return target
}

// Sends the restarted notice the marker names over the requester's profile transport
func (r *GatewayRunner) deliverRestartNotification(ctx context.Context, notifyPath string) (*restartNoticeTarget, error) {
	data, err := readMarkerObject(notifyPath)
	if err != nil {
		return nil, err
	}
	platformStr, chatID, threadID := rowField(data, "platform"), rowField(data, "chat_id"), rowField(data, "thread_id")
	if !pyrepr.Truthy(platformStr) || !pyrepr.Truthy(chatID) {
		return nil, nil
	}
	platform, err := gateway.ParsePlatform(ctx, platformStr)
	if err != nil {
		return nil, err
	}
	live := map[gateway.Platform]any{}
	for p, a := range r.adaptersForProfile(ctx, markerProfile(data)).All() {
		live[p] = a
	}
	transport := gateway.ResolveDeliveryTransport(ctx, platform, r.config, live)
	if transport == nil {
		inboundLog().Debug(fmt.Sprintf("Restart notification skipped: no live transport for %s", pyrepr.Str(platformStr)))
		return nil, nil
	}
	if platformCfg := r.config.Platforms[platform]; platformCfg != nil && !platformCfg.GatewayRestartNotification {
		inboundLog().Info(fmt.Sprintf("Restart notification suppressed: %s has gateway_restart_notification=false", pyrepr.Str(platformStr)))
		return nil, nil
	}
	chat := pyrepr.Str(chatID)
	adapter, _ := transport.Adapter.(gateway.PlatformAdapter)
	metadata := r.pendingMarkerMetadata(platform, chat, data, adapter)
	if rowField(data, "delivered_via_upstream_relay") == true {
		merged := make(map[string]any, len(metadata)+2)
		for k, v := range metadata {
			merged[k] = v
		}
		for _, field := range []string{"user_id", "scope_id"} {
			if v := rowField(data, field); pyrepr.Truthy(v) {
				merged[field] = pyrepr.Str(v)
			}
		}
		metadata = merged
	}
	result, err := transport.Send(ctx, platform, chat, i18n.T(ctx, "gateway.startup.restarted", "", nil), nonConversationalMetadata(metadata, platform))
	if err != nil {
		return nil, err
	}
	if sendFailed(result) {
		inboundLog().Warn(fmt.Sprintf("Restart notification to %s:%s was not delivered: %s", pyrepr.Str(platformStr), chat, sendError(result)))
		return nil, nil
	}
	inboundLog().Info(fmt.Sprintf("Sent restart notification to %s:%s", pyrepr.Str(platformStr), chat))
	target := &restartNoticeTarget{platform: pyrepr.Str(platformStr), chatID: chat}
	if pyrepr.Truthy(threadID) {
		thread := pyrepr.Str(threadID)
		target.threadID = &thread
	}
	return target, nil
}

// One served profile's platform config, profile nil for the launch profile
type servedHomeConfig struct {
	profile  *string
	platform gateway.Platform
	cfg      *gateway.PlatformConfig
}

// Platform configs of every served profile, the launch profile first
func (r *GatewayRunner) servedHomeChannelConfigs() []servedHomeConfig {
	var out []servedHomeConfig
	for _, platform := range sortedPlatforms(r.config.Platforms) {
		out = append(out, servedHomeConfig{platform: platform, cfg: r.config.Platforms[platform]})
	}
	for _, name := range sortedKeys(r.profileConfigs) {
		profile := name
		profileCfg := r.profileConfigs[name]
		for _, platform := range sortedPlatforms(profileCfg.Platforms) {
			out = append(out, servedHomeConfig{profile: &profile, platform: platform, cfg: profileCfg.Platforms[platform]})
		}
	}
	return out
}

// Keys of a string keyed map in order
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Sends the planned restart online notice to every home channel still owed one, clearing the marker once all were reached
func (r *GatewayRunner) replayPendingPlannedRestartNotification(ctx context.Context) {
	r.plannedRestartNoticeLock.Lock()
	defer r.plannedRestartNoticeLock.Unlock()
	path := gateway.PlannedRestartMarkerPath(home.Dir(ctx))
	if !updatePathExists(path) {
		return
	}
	if err := r.replayPlannedRestartMarker(ctx, path); err != nil {
		inboundLog().Warn("Planned-restart notification remains pending", "error", err)
	}
}

// Delivers the owed targets of one planned restart marker and records the delivered ones
func (r *GatewayRunner) replayPlannedRestartMarker(ctx context.Context, path string) error {
	data, err := readMarkerObject(path)
	if err != nil {
		return err
	}
	delivered := map[noticeTargetKey]bool{}
	if raw, ok := data.Get("delivered_targets"); ok {
		items, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("TypeError: delivered_targets is not a list")
		}
		for _, item := range items {
			parts, isList := item.([]any)
			if !isList || len(parts) != 3 {
				return fmt.Errorf("TypeError: delivered target %s is not a triple", pyrepr.Repr(item))
			}
			key := noticeTargetKey{platform: pyrepr.Str(parts[0]), chatID: pyrepr.Str(parts[1])}
			if parts[2] != nil {
				key.threadID, key.threaded = pyrepr.Str(parts[2]), true
			}
			delivered[key] = true
		}
	}
	owed := map[noticeTargetKey]bool{}
	for _, served := range r.servedHomeChannelConfigs() {
		h := served.cfg.HomeChannel
		if h != nil && h.ChatID != "" && served.cfg.GatewayRestartNotification {
			owed[servedNoticeTargetKey(served.profile, string(served.platform), h.ChatID, optionalValue(h.ThreadID))] = true
		}
	}
	for key := range r.sendHomeChannelStartupNotifications(ctx, delivered) {
		delivered[key] = true
	}
	covered := true
	for key := range owed {
		covered = covered && delivered[key]
	}
	if covered {
		_ = os.Remove(path)
		return nil
	}
	targets := make([]any, 0, len(delivered))
	for key := range delivered {
		targets = append(targets, key.list())
	}
	data.Set("delivered_targets", targets)
	return config.AtomicJSONWriteWith(ctx, path, data)
}

// Optional text as a Python value, nil for an absent one
func optionalValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// Notifies every served home channel the gateway is back, once per home chat, the targets reached
func (r *GatewayRunner) sendHomeChannelStartupNotifications(ctx context.Context, skipped map[noticeTargetKey]bool) map[noticeTargetKey]bool {
	delivered := map[noticeTargetKey]bool{}
	message := i18n.T(ctx, "gateway.startup.online", "", nil)
	targets := r.servedHomeChannelTransports(ctx)
	notifiedChats := map[noticeTargetKey]bool{}
	for _, t := range targets {
		if skipped[servedNoticeTargetKey(t.profile, string(t.platform), t.home.ChatID, optionalValue(t.home.ThreadID))] {
			notifiedChats[deliveryTargetKey(string(t.platform), t.home.ChatID, optionalValue(t.home.ThreadID), t.profile)] = true
		}
	}
	for _, t := range targets {
		if !t.cfg.GatewayRestartNotification {
			inboundLog().Info(fmt.Sprintf("Home-channel startup notification suppressed: %s has gateway_restart_notification=false", t.platform))
			continue
		}
		target := servedNoticeTargetKey(t.profile, string(t.platform), t.home.ChatID, optionalValue(t.home.ThreadID))
		if skipped[target] || delivered[target] {
			continue
		}
		chat := deliveryTargetKey(string(t.platform), t.home.ChatID, optionalValue(t.home.ThreadID), t.profile)
		if notifiedChats[chat] {
			delivered[target] = true
			continue
		}
		if r.sendHomeChannelMessage(ctx, t.platform, t.home, t.transport, message, "Home-channel startup notification failed for %s:%s: %s") {
			notifiedChats[chat] = true
			delivered[target] = true
			inboundLog().Info(fmt.Sprintf("Sent home-channel startup notification to %s:%s", t.platform, t.home.ChatID))
		}
	}
	return delivered
}

// Broadcasts a state.db failure warning to every launch profile home channel unless the store recovered
func (r *GatewayRunner) sendSessionDBWarningNotifications(ctx context.Context) {
	if r.sessionDBInitError == nil || *r.sessionDBInitError == "" {
		return
	}
	failure := *r.sessionDBInitError
	if r.sessionDBHandleCache != nil {
		_, _ = r.openSessionDBForActiveScope(ctx, false)
		if r.sessionDBInitError == nil || *r.sessionDBInitError == "" {
			inboundLog().Info("state.db recovered before the home-channel warning went out; not broadcasting")
			return
		}
		failure = *r.sessionDBInitError
	}
	cause := state.ClassifyPersistenceError(failure)
	profileArg := home.ProfileCLISelector(home.Dir(ctx))
	var message string
	switch cause {
	case "corrupt":
		message = i18n.T(ctx, "gateway.startup.db_corrupt", "", map[string]any{"profile_arg": profileArg,
			"db_path": state.ResolveDefaultDBPath(ctx), "backups_dir": filepath.Join(home.Root(), "backups")})
	case "fts_index":
		message = i18n.T(ctx, "gateway.startup.db_fts_corrupt", "", map[string]any{"profile_arg": profileArg})
	default:
		described := state.DescribeStorageFailure(ctx, failure)
		action := described.Action
		if !selfClearingStorageCauses[described.Cause] {
			action = i18n.T(ctx, "gateway.startup.db_then_restart", "", map[string]any{"action": action, "profile_arg": profileArg})
		}
		message = i18n.T(ctx, "gateway.startup.db_unavailable", "", map[string]any{"cause": described.Gloss, "action": action})
	}
	inboundLog().Warn(fmt.Sprintf("Broadcasting state.db failure warning to home channels: %s", failure))
	for _, t := range r.homeChannelTransports(ctx) {
		target := t
		_, err := config.PresentNotification(ctx, func(ctx context.Context) error {
			r.sendHomeChannelMessage(ctx, target.platform, target.home, target.transport, message, "state.db warning notification failed for %s:%s: %s")
			return nil
		}, string(target.platform), true, nil)
		if err != nil {
			inboundLog().Debug(fmt.Sprintf("state.db warning presenter failed: %s", err))
		}
	}
}
