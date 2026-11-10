package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	warnedPinsMu              sync.Mutex
	warnedPins                = map[contextPinKey]bool{}
	advertisedContextLengthFn = advertisedContextLength
)

type contextPinKey struct {
	model string
	pin   int
}

func contextPinLog() *slog.Logger { return slog.Default().With("logger", "agent.context_pin") }

// True when the displayed context length is the positive integer model.context_length pin
func isContextPinned(contextLength, configContextLength any) bool {
	pin, ok := configContextLength.(int)
	if !ok || pin <= 0 {
		return false
	}
	shown, ok := contextLength.(int)
	return ok && shown == pin
}

// IsContextPinned reports a displayed window equal to the positive model.context_length pin
func IsContextPinned(contextLength, configContextLength any) bool {
	return isContextPinned(contextLength, configContextLength)
}

// Pinned label when the shown window comes from model.context_length, empty otherwise
func contextPinSuffix(contextLength, configContextLength any) string {
	if isContextPinned(contextLength, configContextLength) {
		return " (pinned)"
	}
	return ""
}

// ContextPinSuffix is the pinned label of a window equal to the model.context_length pin, empty otherwise
func ContextPinSuffix(contextLength, configContextLength any) string {
	return contextPinSuffix(contextLength, configContextLength)
}

// Provider advertised window from the endpoint cache, the models.dev disk cache or the family table, never a probe
func advertisedContextLength(ctx context.Context, model, baseURL string) int {
	model = provider.StripProviderPrefix(model)
	if model == "" {
		return 0
	}
	if baseURL != "" {
		if cached, ok := provider.GetCachedContextLength(ctx, model, baseURL, false); ok && cached != 0 {
			return cached
		}
	}
	if entry := provider.LoadModelMetadataDiskCache(ctx)[model]; entry != nil {
		if n, ok := jsonDecodedInt(entry["context_length"]); ok && n > 0 {
			return n
		}
	}
	if n, ok := provider.LongestDefaultContextMatch(strings.ToLower(model)); ok {
		return n
	}
	return 0
}

// Integer of a json.load value as isinstance int sees it, decoded whole numbers and bools included
func jsonDecodedInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return int(x), true
		}
	}
	return 0, false
}

// Groups digits by thousands like the Python comma format spec
func commaInt(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Logs one warning per model and pin when the pin disagrees with the advertised window, the pin still winning
func warnOnceOnPinDisagreement(ctx context.Context, model, baseURL string, configContextLength any) bool {
	if !isContextPinned(configContextLength, configContextLength) {
		return false
	}
	pin := configContextLength.(int)
	advertised := advertisedContextLengthFn(ctx, model, baseURL)
	if advertised == 0 || advertised == pin {
		return false
	}
	key := contextPinKey{model, pin}
	warnedPinsMu.Lock()
	seen := warnedPins[key]
	warnedPins[key] = true
	warnedPinsMu.Unlock()
	if seen {
		return false
	}
	contextPinLog().Warn(fmt.Sprintf("model.context_length pins %s at %s tokens but the provider advertises %s; the pin wins. Remove model.context_length from config.yaml to use the advertised window.",
		model, commaInt(pin), commaInt(advertised)))
	return true
}
