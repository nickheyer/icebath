package gatewayrun

import (
	"context"
	"path/filepath"
	"reflect"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Session key and run generation of the refused turn an offer belongs to
type usageLimitOffer struct {
	sessionKey string
	generation int
}

// Schedules the plan's model offer under the refusal reply, as a post delivery callback when the adapter has one
func (r *GatewayRunner) scheduleUsageLimitOffer(ctx context.Context, event *gateway.MessageEvent, limit map[string]any, sessionKey string, generation *int) {
	var adapter gateway.PlatformAdapter
	if event.Source != nil {
		var err error
		if adapter, err = r.deliveryAdapterFor(ctx, event.Source); err != nil {
			runLog().Debug("usage-limit offer scheduling failed", "error", err)
			return
		}
	}
	offer := func(offerCtx context.Context) (any, error) {
		r.offerUsageLimitSwitch(offerCtx, event, limit, generation)
		return nil, nil
	}
	if sessionKey != "" && adapter != nil && adapter.Base() != nil {
		adapter.Base().RegisterPostDeliveryCallback(sessionKey, func(cbCtx context.Context) error {
			_, err := offer(cbCtx)
			return err
		}, generation)
		return
	}
	r.trackBackgroundTask(ctx, "usage-limit-offer", offer)
}

// Model suggestions of a limit payload that name a model
func usageLimitSuggestions(limit map[string]any) []map[string]any {
	raw, _ := limit["suggestions"].([]any)
	var out []map[string]any
	for _, item := range raw {
		if s, ok := item.(map[string]any); ok && pyrepr.Truthy(s["model"]) {
			out = append(out, s)
		}
	}
	return out
}

// Offers the recommended models as a picker where the platform has one, else the next steps text line
func (r *GatewayRunner) offerUsageLimitSwitch(ctx context.Context, event *gateway.MessageEvent, limit map[string]any, generation *int) {
	if err := r.offerUsageLimitSwitchBody(ctx, event, limit, generation); err != nil {
		runLog().Debug("usage-limit offer failed", "error", err)
	}
}

// Offer body whose failure the caller logs
func (r *GatewayRunner) offerUsageLimitSwitchBody(ctx context.Context, event *gateway.MessageEvent, limit map[string]any, generation *int) error {
	suggestions := usageLimitSuggestions(limit)
	source := event.Source
	if source == nil {
		return nil
	}
	prefix := r.typedCommandPrefixFor(source.Platform)
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return err
	}
	if _, picks := adapter.(gateway.ChoicePickerSender); len(suggestions) > 0 && adapter != nil && picks {
		onChoice := func(choiceCtx context.Context, _ string, value string) (string, error) {
			if generation != nil && r.usageLimitOfferIsStale(sessionKey, *generation) {
				return i18n.T(choiceCtx, "platform.discord.picker.choice_expired", "", nil), nil
			}
			var offer *usageLimitOffer
			if generation != nil {
				offer = &usageLimitOffer{sessionKey: sessionKey, generation: *generation}
			}
			return r.acceptUsageLimitSwitch(choiceCtx, event, value, offer)
		}
		choices := make([]map[string]any, len(suggestions))
		for i, s := range suggestions {
			label := pyrepr.Str(pyrepr.Or(s["label"], s["model"]))
			choices[i] = map[string]any{"value": pyrepr.Str(s["model"]),
				"label":      i18n.T(ctx, "gateway.usage_limit.switch_choice", "", map[string]any{"model": label, "note": pyrepr.Str(pyrepr.Or(s["note"], ""))}),
				"is_current": false}
		}
		if r.trySendChoicePicker(ctx, event, sessionKey, i18n.T(ctx, "gateway.usage_limit.picker_title", "", nil), choices, onChoice) {
			return nil
		}
	}
	text := provider.LimitNextStepsFromPayload(limit, prefix+"model", "", true, false)
	if adapter != nil && text != "" {
		_, err := adapter.Send(ctx, source.ChatID, text, "", r.replyMetadata(event))
		return err
	}
	return nil
}

// Whether the session started a newer run than the refused one, failing open as not stale
func (r *GatewayRunner) usageLimitOfferIsStale(sessionKey string, generation int) bool {
	current := 0
	r.withSessionState(sessionKey, func(s *SessionState) { current = s.persistent.runGeneration })
	return current != generation
}

// Identity of a map value, the Python is check of two dicts
func mapIdentity(m map[string]any) uintptr {
	if m == nil {
		return 0
	}
	return reflect.ValueOf(m).Pointer()
}

// Switches this session to the tapped model on Nous, then re sends the refused message
func (r *GatewayRunner) acceptUsageLimitSwitch(ctx context.Context, event *gateway.MessageEvent, modelID string, offer *usageLimitOffer) (string, error) {
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	profileHome := ""
	if r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
		if profileHome, err = r.resolveProfileHomeForSource(ctx, event.Source); err != nil {
			return "", err
		}
	}
	configHome := profileHome
	if configHome == "" {
		configHome = home.Dir(context.Background())
	}
	sw := &gateway.ModelSwitchContext{SessionKey: sessionKey, Source: source, ConfigPath: filepath.Join(configHome, "config.yaml"), CurrentProvider: "openrouter"}
	switchCtx := ctx
	if profileHome != "" {
		switchCtx = gateway.ProfileRuntimeScope(ctx, profileHome, true)
	}
	before, _ := r.sessionModelOverrides().Get(sessionKey)
	reply, err := func() (string, error) {
		sw.ReadConfig(switchCtx)
		current, _ := r.sessionModelOverrides().Get(sessionKey)
		sw.ApplyOverride(current)
		result, errorText, err := r.performModelSwitch(switchCtx, sw, modelID, "nous", event.Source)
		if err != nil {
			return "", err
		}
		if result == nil {
			return errorText, nil
		}
		return r.commitModelSwitch(switchCtx, result, sw, event.Source, true)
	}()
	if err != nil {
		return "", err
	}
	after, _ := r.sessionModelOverrides().Get(sessionKey)
	if after != nil && mapIdentity(after) != mapIdentity(before) && after["provider"] == "nous" {
		r.trackBackgroundTask(ctx, "usage-limit-retry", func(taskCtx context.Context) (any, error) {
			r.dispatchUsageLimitRetry(taskCtx, event, offer)
			return nil, nil
		})
	}
	return reply, nil
}

// Re sends the refused message as a typed retry unless a newer turn ran since the offer
func (r *GatewayRunner) dispatchUsageLimitRetry(ctx context.Context, event *gateway.MessageEvent, offer *usageLimitOffer) {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		runLog().Debug("usage-limit retry dispatch failed", "error", err)
		return
	}
	if adapter == nil || (offer != nil && r.usageLimitOfferIsStale(offer.sessionKey, offer.generation)) {
		return
	}
	retry := gateway.NewMessageEvent("/retry")
	retry.Source, retry.RawMessage, retry.ChannelPrompt = event.Source, event.RawMessage, event.ChannelPrompt
	if err := adapter.HandleMessage(ctx, retry); err != nil {
		runLog().Debug("usage-limit retry dispatch failed", "error", err)
	}
}

// Sends an interactive choice picker when the adapter supports one, false for the text fallback
func (r *GatewayRunner) trySendChoicePicker(ctx context.Context, event *gateway.MessageEvent, sessionKey, title string, choices []map[string]any,
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error)) bool {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil {
		runLog().Warn("send_choice_picker failed, falling back to text: " + pyrepr.ExceptionMessage(err))
		return false
	}
	picker, ok := adapter.(gateway.ChoicePickerSender)
	if adapter == nil || !ok {
		return false
	}
	result, err := picker.SendChoicePicker(ctx, event.Source.ChatID, title, choices, sessionKey, onChoiceSelected, r.replyMetadata(event))
	if err != nil {
		runLog().Warn("send_choice_picker failed, falling back to text: " + pyrepr.ExceptionMessage(err))
		return false
	}
	return result.Success
}
