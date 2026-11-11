package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One time note a fresh session after an automatic reset carries to the agent, by reset reason
var autoResetContextNotes = map[string]string{
	"suspended": "[System note: The user's previous session was stopped and suspended. This is a fresh conversation with no prior context.]",
}

// Session info block of the auto reset notice, resolved inside the profile serving the source
func (r *GatewayRunner) resetNoticeSessionInfo(ctx context.Context, source *gateway.SessionSource) (string, error) {
	scoped, err := r.profileScopeForSource(ctx, source)
	if err != nil {
		return "", err
	}
	return r.formatSessionInfo(scoped), nil
}

// Model, provider, context length and local endpoint lines so users can spot bad context detection
func (r *GatewayRunner) formatSessionInfo(ctx context.Context) string {
	resolved := resolveGatewayModelContext(ctx, "", nil)
	sources := map[string]string{"config": "config", "default": "default — set model.context_length in config to override"}
	ctxSource, ok := sources[resolved.contextSource]
	if !ok {
		ctxSource = "detected"
	}
	n := resolved.contextLength
	display := fmt.Sprint(n)
	switch {
	case n >= 1_000_000:
		display = fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		display = fmt.Sprintf("%dK", n/1_000)
	}
	providerName := resolved.provider
	if providerName == "" {
		providerName = "openrouter"
	}
	lines := []string{
		i18n.T(ctx, "gateway.session.info_model", "", map[string]any{"model": resolved.model}),
		i18n.T(ctx, "gateway.session.info_provider", "", map[string]any{"provider": providerName}),
		i18n.T(ctx, "gateway.session.info_context", "", map[string]any{"tokens": display, "source": ctxSource}),
	}
	if resolved.provider == "moa" {
		cfg := provider.LoadConfigSafe(ctx, home.Dir(ctx))
		presets, _ := provider.NormalizeMoAConfig(cfg["moa"])["presets"].(map[string]any)
		preset, _ := presets[resolved.model].(map[string]any)
		if agg, _ := preset["aggregator"].(map[string]any); len(agg) > 0 {
			lines = append(lines, i18n.T(ctx, "gateway.session.info_acting_model", "",
				map[string]any{"provider": pyrepr.Str(agg["provider"]), "model": pyrepr.Str(agg["model"])}))
		}
	}
	if resolved.baseURL != "" {
		if host, err := pyurl.BaseURLHostname(resolved.baseURL); err == nil && (host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0") {
			lines = append(lines, i18n.T(ctx, "gateway.session.info_endpoint", "", map[string]any{"url": resolved.baseURL}))
		}
	}
	return strings.Join(lines, "\n")
}

// Stages the auto reset sidecar note and notifies the user of a suspended session, clearing the reset reason
func (r *GatewayRunner) hmwaDeliverAutoResetNotice(ctx context.Context, entry *gateway.SessionEntry, source *gateway.SessionSource, turnSidecarNotes *[]string) {
	resetReason := "suspended"
	if entry.AutoResetReason != nil && *entry.AutoResetReason != "" {
		resetReason = *entry.AutoResetReason
	}
	contextNote, ok := autoResetContextNotes[resetReason]
	if !ok {
		contextNote = autoResetContextNotes["suspended"]
	}
	if continuity := gateway.BuildChannelContinuityNote(entry, source); continuity != "" {
		contextNote = contextNote + "\n\n" + continuity
	}
	*turnSidecarNotes = append(*turnSidecarNotes, contextNote)
	if err := r.sendAutoResetNotice(ctx, source, resetReason == "suspended"); err != nil {
		runLog().Debug(fmt.Sprintf("Auto-reset notification failed (non-fatal): %s", err))
	}
	entry.AutoResetReason = nil
}

// Sends the auto reset notice with the session info block when the reset policy notifies
func (r *GatewayRunner) sendAutoResetNotice(ctx context.Context, source *gateway.SessionSource, shouldNotify bool) error {
	if !shouldNotify {
		return nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter == nil {
		return nil
	}
	notice := i18n.T(ctx, "gateway.session.auto_reset_notice", "", nil)
	if info, err := r.resetNoticeSessionInfo(ctx, source); err == nil && info != "" {
		notice = notice + "\n\n" + info
	}
	_, err = adapter.Send(ctx, source.ChatID, notice, "", r.threadMetadataForSource(source, nil))
	return err
}
