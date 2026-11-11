package gatewayrun

import (
	"context"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Transient auxiliary and compression statuses that stay in logs instead of gateway chats
var telegramNoisyStatusRe = pyre.MustCompile(`(?is)(` +
	`auxiliary\s+.+\s+failed` +
	`|compression\s+summary\s+failed` +
	`|fallback\s+context\s+marker` +
	`|configured\s+compression\s+model\s+.+\s+failed` +
	`|no\s+auxiliary\s+llm\s+provider\s+configured` +
	`|auto-lowered\s+compression\s+threshold` +
	`|auto-lowered\s+(?:this\s+)?session'?s?\s+threshold` +
	`|configured\s+auxiliary\s+compression\s+provider\s+.+\s+unavailable` +
	`|skipping\s+concurrent\s+compression` +
	`|` + regexp.QuoteMeta(agent.CompactionStatus) +
	`|` + regexp.QuoteMeta(agent.CompactionHeartbeatStatus) +
	`|resumed\s+after\s+\d+s\s+idle\s+[—-]\s+compacting` +
	`|preflight\s+compression` +
	`|pre[- ]api\s+compression` +
	`|context\s+too\s+large\s+\(~[\d,]+\s+tokens\)\s+[—-]+\s+compressing` +
	`|compressed\s+\d[\d,]*\s+(?:→|->)\s+\d[\d,]*\s+messages,\s+retrying` +
	`|compressed\s+~[\d,]+\s+(?:→|->)\s+~[\d,]+\s+tokens,\s+retrying` +
	`|context\s+reduced\s+to\s+[\d,]+\s+tokens\s+\(was\s+[\d,]+\),\s+retrying` +
	`|session\s+compressed\s+\d+\s+times` +
	`|rate\s+limited\.\s+waiting\s+\d` +
	`|retrying\s+in\s+\d` +
	`|max\s+retries\s+\(\d+\).*(?:trying\s+fallback|exhausted|invalid\s+responses)` +
	`|stream\s+(?:drop|drop\s+mid\s+tool-call).+retry\s+\d` +
	`|stale\s+connections\s+from\s+a\s+previous\s+provider\s+issue` +
	`|` + regexp.QuoteMeta(agent.CompactionDoneStatus) +
	`)`)

// Compression status templates the agent formats its routine progress statuses from
var compressionProgressTemplates = []string{
	agent.CompactionStatus, agent.CompactionHeartbeatStatus, agent.CompactionDoneStatus,
	"📦 Pre-API compression: ~{tokens:,} tokens near the context/output limit. Compacting before the next model call.",
	"📦 Preflight compression: ~{tokens:,} tokens >= {threshold:,} threshold. This may take a moment.",
	"💤 Resumed after {idle_seconds}s idle — compacting ~{tokens:,} tokens before continuing.",
	"🗜️ Context too large (~{tokens:,} tokens) — compressing ({attempt}/{cap})...",
	"🗜️ Compressed {before} → {after} messages, retrying...",
	"🗜️ Compressed ~{before:,} → ~{after:,} tokens, retrying...",
	"🗜️ Context reduced to {new_ctx:,} tokens (was {old_ctx:,}), retrying...",
}

// Format fields of a status template
var templateFieldRe = regexp.MustCompile(`\{[^{}]*\}`)

// Regex source of a status template with its literal text escaped and each field matching a number
func statusTemplateToRegex(template string) string {
	parts := templateFieldRe.Split(template, -1)
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return strings.Join(parts, `[\d,]+`)
}

// Routine compression progress statuses derived from the agent's status templates
var compressionProgressStatusRe = func() *regexp2.Regexp {
	sources := make([]string, len(compressionProgressTemplates))
	for i, template := range compressionProgressTemplates {
		sources[i] = statusTemplateToRegex(template)
	}
	return pyre.MustCompile(`(?i)` + strings.Join(sources, "|"))
}()

// Whether compression.progress_notices is on in the gateway config, false on a read failure
func gatewayCompressionProgressNoticesEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = false
		}
	}()
	compression, ok := gateway.LoadGatewayConfigMap(ctx, "")["compression"].(map[string]any)
	if !ok {
		return false
	}
	value, present := compression["progress_notices"]
	if !present {
		value = false
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(value))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// Status text a surface may show, raw for programmatic surfaces and filtered of noise on chats, nil when suppressed
func prepareGatewayStatusMessage(ctx context.Context, platform gateway.Platform, eventType string, message any) *string {
	text := ""
	if pyrepr.Truthy(message) {
		text = pystr.Strip(pyrepr.Str(message))
	}
	if text == "" {
		return nil
	}
	if gatewaySurfacePassesRawText(platform) {
		return &text
	}
	text = redactGatewayUserFacingSecrets(ctx, text)
	if noisy, _ := telegramNoisyStatusRe.MatchString(text); noisy {
		progress := false
		if gatewayCompressionProgressNoticesEnabled(ctx) {
			progress, _ = compressionProgressStatusRe.MatchString(text)
		}
		if !progress {
			return nil
		}
	}
	if looksLikeGatewayProviderError(text) {
		reply := gatewayProviderErrorReply(ctx, text)
		return &reply
	}
	return &text
}

// Adapter that edits the previous status bubble of the same key instead of appending a new one
type statusUpdater interface {
	SendOrUpdateStatus(ctx context.Context, chatID, statusKey, content string, metadata map[string]any) (gateway.SendResult, error)
}

// Sends a status through the adapter's keyed status update when it has one, else as a plain message
func sendOrUpdateStatus(ctx context.Context, adapter gateway.PlatformAdapter, chatID, statusKey, content string, metadata map[string]any) (gateway.SendResult, error) {
	if updater, ok := adapter.(statusUpdater); ok {
		return updater.SendOrUpdateStatus(ctx, chatID, statusKey, content, metadata)
	}
	return adapter.Send(ctx, chatID, content, "", metadata)
}
