package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// MaxPlatformOutput caps cron output before truncation for adapters that do not split long messages
const MaxPlatformOutput = 4000

const pyWS = `\t\n\x0b\f\r\x1c-\x1f \x85\p{Z}`

var silenceNarration = regexp2.MustCompile(
	`^[`+pyWS+`*_~\x60]*\(?[`+pyWS+`]*(silent|silence|no[`+pyWS+`]+response|no[`+pyWS+`]+reply)[`+pyWS+`]*\.?\)?[`+pyWS+`*_~\x60]*$`+
		`|^[`+pyWS+`*_~\x60]*[\x{1F507}\.\x{2026}]+[`+pyWS+`*_~\x60]*$`, regexp2.IgnoreCase|regexp2.Unicode)

var threadRoutingKeys = []string{"thread_id", "message_thread_id", "direct_messages_topic_id", "telegram_direct_messages_topic_id"}

// True when the content is only a short silence narration token
func isSilenceNarration(content string) bool {
	stripped := pystr.Strip(content)
	if stripped == "" || len([]rune(stripped)) > 64 {
		return false
	}
	ok, _ := silenceNarration.MatchString(stripped)
	return ok
}

// PartialDeliveryError is a split send that failed after earlier chunks were delivered
type PartialDeliveryError struct {
	Msg    string
	Result any
}

func (e *PartialDeliveryError) Error() string { return e.Msg }

// DeliveryAdapter is the native adapter send surface a delivery transport calls
type DeliveryAdapter interface {
	Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (SendResult, error)
}

// RelayDeliveryAdapter is the relay transport that sends for the logical platforms it fronts
type RelayDeliveryAdapter interface {
	SendForPlatform(ctx context.Context, platform Platform, chatID, content string, metadata map[string]any) (SendResult, error)
	FrontsPlatform(platform Platform) bool
}

// DMTopicEnsurer creates or recreates a named Telegram private DM topic and returns its thread id
type DMTopicEnsurer interface {
	EnsureDMTopic(ctx context.Context, chatID, name string, forceCreate bool) (string, error)
}

// LongMessageSplitter reports whether an adapter chunks long messages natively
type LongMessageSplitter interface {
	SplitsLongMessages() bool
}

// DeliveryTransport is the resolved live transport for one logical delivery platform
type DeliveryTransport struct {
	Adapter  any
	Config   *PlatformConfig
	Platform Platform
}

// True when the transport is the relay
func (t *DeliveryTransport) IsRelay() bool { return t.Platform == PlatformRelay }

// Sends through the transport keeping the logical platform
func (t *DeliveryTransport) Send(ctx context.Context, logical Platform, chatID, content string, metadata map[string]any) (SendResult, error) {
	if t.IsRelay() {
		relay, ok := t.Adapter.(RelayDeliveryAdapter)
		if !ok {
			return SendResult{}, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'send_for_platform'", t.Adapter)}
		}
		return relay.SendForPlatform(ctx, logical, chatID, content, metadata)
	}
	native, ok := t.Adapter.(DeliveryAdapter)
	if !ok {
		return SendResult{}, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'send'", t.Adapter)}
	}
	return native.Send(ctx, chatID, content, "", metadata)
}

// Live adapter of a platform from a map keyed by platform value
func adapterFor(adapters any, platform Platform) any {
	switch m := adapters.(type) {
	case map[string]any:
		return m[string(platform)]
	case map[Platform]any:
		return m[platform]
	case *pydict.Dict[Platform, PlatformAdapter]:
		adapter, _ := m.Get(platform)
		return adapter
	}
	return nil
}

// Logical platform resolved to its live transport, a native adapter winning over a relay that fronts it
func ResolveDeliveryTransport(ctx context.Context, platform Platform, config *GatewayConfig, adapters any) *DeliveryTransport {
	native, nativeConfig := adapterFor(adapters, platform), config.Platforms[platform]
	if native != nil && (nativeConfig == nil || nativeConfig.Enabled) {
		return &DeliveryTransport{Adapter: native, Config: nativeConfig, Platform: platform}
	}
	relay, relayConfig := adapterFor(adapters, PlatformRelay), config.Platforms[PlatformRelay]
	fronts, ok := relay.(RelayDeliveryAdapter)
	if relay != nil && (relayConfig == nil || relayConfig.Enabled) && ok && fronts.FrontsPlatform(platform) {
		return &DeliveryTransport{Adapter: relay, Config: relayConfig, Platform: PlatformRelay}
	}
	return nil
}

// True when the chat id is a positive integer, the Telegram private chat shape
func LooksLikeTelegramPrivateChatID(chatID string) bool {
	n, ok := pyint.Parse(pystr.Strip(chatID))
	return ok && n.Sign() > 0
}

// True when Python int accepts the value
func LooksLikeInt(value string) bool {
	_, ok := pyint.Parse(pystr.Strip(value))
	return ok
}

// Error text of a failed send result, nil on success
func sendResultError(result any) *string {
	success, ok := sendResultAttr(result, "success")
	if !ok {
		success = true
	}
	if b, isBool := success.(bool); !isBool || b {
		return nil
	}
	errVal, _ := sendResultAttr(result, "error")
	text := ""
	if pyrepr.Truthy(errVal) {
		text = pyrepr.Str(errVal)
	}
	return &text
}

// DeliveryTarget is one delivery destination of origin, local, a home channel or an explicit chat
type DeliveryTarget struct {
	Platform        Platform
	ChatID          string
	ThreadID        *string
	IsOrigin        bool
	IsExplicit      bool
	UnknownPlatform *string
}

// Target parsed from origin, local, a platform name or platform chat and thread
func ParseDeliveryTarget(ctx context.Context, target string, origin *SessionSource) DeliveryTarget {
	target = pystr.Strip(target)
	if strings.ToLower(target) == "origin" {
		if origin != nil {
			return DeliveryTarget{Platform: origin.Platform, ChatID: origin.ChatID, ThreadID: origin.ThreadID, IsOrigin: true}
		}
		return DeliveryTarget{Platform: PlatformLocal, IsOrigin: true}
	}
	parts := strings.SplitN(target, ":", 3)
	platform, err := ParsePlatform(ctx, strings.ToLower(parts[0]))
	if err != nil {
		raw := target
		return DeliveryTarget{Platform: PlatformLocal, UnknownPlatform: &raw}
	}
	if len(parts) > 1 {
		t := DeliveryTarget{Platform: platform, ChatID: parts[1], IsExplicit: true}
		if len(parts) > 2 {
			thread := parts[2]
			t.ThreadID = &thread
		}
		return t
	}
	return DeliveryTarget{Platform: platform}
}

// Target in its parseable string form
func (t DeliveryTarget) String() string {
	if t.IsOrigin {
		return "origin"
	}
	if t.UnknownPlatform != nil {
		return *t.UnknownPlatform
	}
	if t.Platform == PlatformLocal {
		return "local"
	}
	parts := []string{string(t.Platform)}
	if t.ChatID != "" {
		parts = append(parts, t.ChatID)
		if t.ThreadID != nil && *t.ThreadID != "" {
			parts = append(parts, *t.ThreadID)
		}
	}
	return strings.Join(parts, ":")
}

// Creates or force recreates a named Telegram private DM topic and returns its thread id
func ensureNamedDMTopic(ctx context.Context, adapter any, chatID, name string, refresh bool) (string, error) {
	verb := "create"
	if refresh {
		verb = "refresh"
	}
	ensurer, ok := adapter.(DMTopicEnsurer)
	if !ok {
		return "", fmt.Errorf("Telegram adapter cannot %s named private DM topics", verb)
	}
	threadID, err := ensurer.EnsureDMTopic(ctx, chatID, name, refresh)
	if err != nil {
		return "", err
	}
	if threadID == "" {
		return "", fmt.Errorf("Failed to %s Telegram private DM topic '%s'", verb, name)
	}
	return threadID, nil
}

// DeliveryRouter resolves delivery targets and dispatches messages to platform adapters
type DeliveryRouter struct {
	Config      *GatewayConfig
	Adapters    any
	OutputDir   string
	DeadTargets *DeadTargetRegistry
}

// Router over the config and live adapters with the process profile output dir and dead target registry
func NewDeliveryRouter(config *GatewayConfig, adapters any) *DeliveryRouter {
	return NewDeliveryRouterIn(context.Background(), config, adapters, nil)
}

// Router whose output dir and default dead target registry belong to the profile of ctx
func NewDeliveryRouterIn(ctx context.Context, config *GatewayConfig, adapters any, deadTargets *DeadTargetRegistry) *DeliveryRouter {
	if adapters == nil {
		adapters = map[string]any{}
	}
	if deadTargets == nil {
		deadTargets = NewDeadTargetRegistry(ctx, "")
	}
	return &DeliveryRouter{Config: config, Adapters: adapters, OutputDir: filepath.Join(home.Dir(ctx), "cron", "output"), DeadTargets: deadTargets}
}

// DeliveryOutcome is one target result of a router delivery
type DeliveryOutcome = map[string]any

// Delivers content to every target and returns per target results keyed by target string
func (r *DeliveryRouter) Deliver(ctx context.Context, content string, targets []DeliveryTarget, jobID, jobName string, metadata [][2]any) map[string]DeliveryOutcome {
	results := map[string]DeliveryOutcome{}
	for _, target := range targets {
		key := target.String()
		if target.UnknownPlatform != nil {
			results[key] = DeliveryOutcome{"success": false, "error": "unknown_platform: " + *target.UnknownPlatform}
			continue
		}
		tracked := target.Platform != PlatformLocal && target.ChatID != ""
		if tracked && r.DeadTargets.IsDead(string(target.Platform), target.ChatID) {
			slog.Info(fmt.Sprintf("Skipping delivery to known-dead target %s:%s (send to it again to clear)", target.Platform, target.ChatID))
			results[key] = DeliveryOutcome{"success": false, "skipped": "dead_target", "error": "target previously confirmed unreachable"}
			continue
		}
		var result any
		var err error
		if target.Platform == PlatformLocal {
			result, err = r.deliverLocal(content, jobID, jobName, metadata)
		} else {
			meta := map[string]any(nil)
			if metadata != nil {
				meta = map[string]any{}
				for _, kv := range metadata {
					meta[pyrepr.Str(kv[0])] = kv[1]
				}
			}
			result, err = r.DeliverToPlatform(ctx, target, content, meta, nil)
			if err == nil && target.ChatID != "" && sendResultError(result) == nil {
				r.DeadTargets.Clear(string(target.Platform), target.ChatID)
			}
		}
		if err != nil {
			if tracked {
				if kind := ClassifyDeadError(err.Error()); kind != "" {
					msg := []rune(err.Error())
					if len(msg) > 120 {
						msg = msg[:120]
					}
					r.DeadTargets.MarkDead(string(target.Platform), target.ChatID, kind+": "+string(msg))
				}
			}
			results[key] = DeliveryOutcome{"success": false, "error": err.Error()}
			continue
		}
		results[key] = DeliveryOutcome{"success": true, "result": result}
	}
	return results
}

// Saves content under the cron output dir as a markdown record
func (r *DeliveryRouter) deliverLocal(content, jobID, jobName string, metadata [][2]any) (map[string]any, error) {
	now := time.Now()
	timestamp := now.Format("20060102_150405")
	dir := jobID
	if dir == "" {
		dir = "misc"
	}
	outputPath := filepath.Join(r.OutputDir, dir, timestamp+".md")
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, err
	}
	title := "# Delivery Output"
	if jobName != "" {
		title = "# " + jobName
	}
	lines := []string{title, "", "**Timestamp:** " + now.Format("2006-01-02 15:04:05")}
	if jobID != "" {
		lines = append(lines, "**Job ID:** "+jobID)
	}
	for _, kv := range metadata {
		lines = append(lines, fmt.Sprintf("**%s:** %s", pyrepr.Str(kv[0]), pyrepr.Str(kv[1])))
	}
	lines = append(lines, "", "---", "", content)
	if err := os.WriteFile(outputPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return nil, err
	}
	return map[string]any{"path": outputPath, "timestamp": timestamp}, nil
}

// Saves the full cron output under the profile cron output dir and returns its path
func saveFullOutput(ctx context.Context, content, jobID string) (string, error) {
	timestamp := time.Now().Format("20060102_150405")
	path := filepath.Join(home.Dir(ctx), "cron", "output", fmt.Sprintf("%s_%s.txt", jobID, timestamp))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(content), 0o644)
}

// Audit saves oversized output and truncates it with a footer for adapters that do not chunk
func (r *DeliveryRouter) capOversizedOutput(ctx context.Context, adapter any, content, jobID string) (string, error) {
	runes := []rune(content)
	if len(runes) <= MaxPlatformOutput {
		return content, nil
	}
	savedPath, err := saveFullOutput(ctx, content, jobID)
	if err != nil {
		slog.Warn(fmt.Sprintf("Audit save failed for cron output (%d chars, job=%s): %v — delivery proceeds without audit copy", len(runes), jobID, err))
		savedPath = ""
	}
	if splitter, ok := adapter.(LongMessageSplitter); ok && splitter.SplitsLongMessages() {
		if savedPath != "" {
			slog.Info(fmt.Sprintf("Cron output preserved for chunking adapter (%d chars) — full output saved to %s", len(runes), savedPath))
		}
		return content, nil
	}
	if savedPath == "" {
		if savedPath, err = saveFullOutput(ctx, content, jobID); err != nil {
			return "", err
		}
	}
	footer := fmt.Sprintf("\n\n... [truncated, full output saved to %s]", savedPath)
	slog.Info(fmt.Sprintf("Cron output truncated (%d chars) — full output: %s", len(runes), savedPath))
	keep := MaxPlatformOutput - len([]rune(footer))
	if keep < 0 {
		keep = 0
	}
	return string(runes[:keep]) + footer, nil
}

// Delivers content to a messaging platform through a resolved or caller authorized transport
func (r *DeliveryRouter) DeliverToPlatform(ctx context.Context, target DeliveryTarget, content string, metadata map[string]any, transport *DeliveryTransport) (any, error) {
	if transport == nil {
		transport = ResolveDeliveryTransport(ctx, target.Platform, r.Config, r.Adapters)
	}
	if transport == nil {
		return nil, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("No adapter configured for %s", target.Platform)}
	}
	if target.ChatID == "" {
		return nil, &PyError{Kind: "ValueError", Msg: fmt.Sprintf("No chat ID for %s delivery", target.Platform)}
	}
	adapter := transport.Adapter
	jobID := "unknown"
	if v, ok := metadata["job_id"]; ok {
		jobID = pyrepr.Str(v)
	}
	content, err := r.capOversizedOutput(ctx, adapter, content, jobID)
	if err != nil {
		return nil, err
	}
	_, isCronArtifact := metadata["job_id"]
	if r.Config.FilterSilenceNarration && !isCronArtifact && isSilenceNarration(content) {
		head := []rune(content)
		if len(head) > 40 {
			head = head[:40]
		}
		slog.Warn(fmt.Sprintf("Dropped silence-narration outbound to %s (chat=%s): %s", target.Platform, target.ChatID, pyrepr.Repr(string(head))))
		return map[string]any{"success": true, "filtered": "silence_narration", "delivered": false}, nil
	}
	sendMetadata := map[string]any{}
	for k, v := range metadata {
		sendMetadata[k] = v
	}
	if transport.IsRelay() {
		if hc := r.Config.GetHomeChannel(target.Platform); hc != nil && hc.ChatID == target.ChatID {
			if hc.UserID != nil && *hc.UserID != "" {
				sendMetadata["user_id"] = *hc.UserID
			}
			if hc.ScopeID != nil && *hc.ScopeID != "" {
				sendMetadata["scope_id"] = *hc.ScopeID
			}
		}
	}
	namedTopic := ""
	if target.ThreadID != nil && *target.ThreadID != "" && !hasAnyKey(sendMetadata, threadRoutingKeys) {
		threadID := *target.ThreadID
		sendMetadata["thread_id"] = threadID
		if target.Platform == PlatformTelegram && LooksLikeTelegramPrivateChatID(target.ChatID) {
			switch {
			case !LooksLikeInt(threadID):
				namedTopic = threadID
				created, err := ensureNamedDMTopic(ctx, adapter, target.ChatID, threadID, false)
				if err != nil {
					return nil, err
				}
				sendMetadata["thread_id"] = created
				sendMetadata["telegram_dm_topic_created_for_send"] = true
			case sendMetadata["telegram_reply_to_message_id"] == nil:
				return nil, errors.New("Telegram private DM topic delivery requires telegram_reply_to_message_id; send to the bare chat or provide a reply anchor")
			default:
				sendMetadata["telegram_dm_topic_reply_fallback"] = true
			}
		}
	}
	var result SendResult
	var errText *string
	for _, retry := range []bool{false, true} {
		meta := sendMetadata
		if len(meta) == 0 {
			meta = nil
		}
		if result, err = transport.Send(ctx, target.Platform, target.ChatID, content, meta); err != nil {
			return nil, err
		}
		errText = sendResultError(result)
		if retry || errText == nil || namedTopic == "" || !strings.Contains(strings.ToLower(*errText), "thread not found") {
			break
		}
		recreated, err := ensureNamedDMTopic(ctx, adapter, target.ChatID, namedTopic, true)
		if err != nil {
			return nil, err
		}
		sendMetadata["thread_id"] = recreated
		sendMetadata["telegram_dm_topic_created_for_send"] = true
	}
	if errText != nil {
		if IsPartialDelivery(result) {
			return nil, &PartialDeliveryError{Msg: *errText, Result: result}
		}
		if *errText != "" {
			return nil, errors.New(*errText)
		}
		return nil, fmt.Errorf("%s delivery failed", target.Platform)
	}
	return result, nil
}

func hasAnyKey(m map[string]any, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
