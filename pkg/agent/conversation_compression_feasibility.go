package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seams the feasibility tests replace like the hermes patch of the auxiliary client and model metadata
var (
	feasibilityTaskProvider   = provider.ResolveAuxTaskProvider
	feasibilityAuxClient      = provider.GetTextAuxiliaryClient
	feasibilityFallbackClient = provider.TryConfiguredFallbackForUnavailableClient
	feasibilityContextLength  = provider.GetModelContextLength
)

// auxContextBelowMinimumError is the ValueError that refuses a session whose summariser window is too small
type auxContextBelowMinimumError struct{ msg string }

func (e *auxContextBelowMinimumError) Error() string { return e.msg }

// Live trigger of any context engine, the derived property on the built in compressor
func engineThresholdTokens(e ContextEngine) int {
	if cc, ok := e.(*ContextCompressor); ok {
		return cc.ThresholdTokens()
	}
	return e.Base().ThresholdTokens
}

// Resolved window of any context engine
func engineContextLength(e ContextEngine) int {
	if cc, ok := e.(*ContextCompressor); ok {
		return cc.ContextLength()
	}
	return e.Base().ContextLength
}

// Stores and emits a feasibility verdict once per distinct text
func emitFeasibilityNotice(a *AIAgent, msg string) {
	if a.lastFeasibilityNotice != nil && *a.lastFeasibilityNotice == msg {
		return
	}
	a.lastFeasibilityNotice = &msg
	a.compressionWarning = msg
	a.emitDiagnosticStatus(msg)
}

// Lowers the live trigger to the summariser window and tells the user how to make it permanent
func lowerThresholdToAuxContext(a *AIAgent, auxModel string, auxContext int, auxProvider, auxBaseURL string) {
	compressor := a.contextCompressor
	oldThreshold := engineThresholdTokens(compressor)
	newThreshold := auxContext
	cc, isCC := compressor.(*ContextCompressor)
	if isCC {
		cc.setThresholdTokens(auxContext)
		ceiling := auxContext
		cc.auxContextCeiling = &ceiling
		if cc.tailMode == "lean" {
			cc.tailTokenBudget = nil
		} else {
			budget := int(float64(newThreshold) * cc.summaryTargetRatio)
			cc.tailTokenBudget = &budget
		}
	} else {
		compressor.Base().ThresholdTokens = auxContext
	}
	mainCtx := engineContextLength(compressor)
	if mainCtx != 0 {
		compressor.Base().ThresholdPercent = float64(newThreshold) / float64(mainCtx)
	}
	safePct := 50
	if mainCtx != 0 {
		safePct = int(float64(auxContext) / float64(mainCtx) * 100)
	}
	var recomputed *int
	if mainCtx != 0 && isCC {
		v := computeThresholdTokens(mainCtx, effectiveThresholdPercent(mainCtx, float64(safePct)/100), cc.maxTokens)
		recomputed = &v
	}
	viable := recomputed == nil || *recomputed <= auxContext
	mainModel := a.model
	if mainModel == "" {
		mainModel = "?"
	}
	auxLabel := ""
	if auxProvider != "" && auxProvider != "auto" {
		auxLabel = auxProvider
	}
	if auxLabel == "" {
		if u, err := pyurl.Parse(auxBaseURL); err != nil {
			auxLabel = auxBaseURL
			if auxLabel == "" {
				auxLabel = "auto"
			}
		} else if auxLabel = u.Hostname(); auxLabel == "" {
			auxLabel = auxBaseURL
		}
	}
	mainLabel := mainModel
	if a.provider != "" {
		mainLabel = fmt.Sprintf("%s (%s)", mainModel, a.provider)
	}
	auxFull := fmt.Sprintf("%s (%s)", auxModel, auxLabel)
	msg := fmt.Sprintf("⚠ Compression model %s context is %s tokens, but the main model %s's compression threshold was %s tokens. Auto-lowered this session's threshold to %s tokens so compression can run.\n",
		auxFull, groupThousands(auxContext), mainLabel, groupThousands(oldThreshold), groupThousands(newThreshold))
	if viable {
		msg += fmt.Sprintf("  To make this permanent, edit config.yaml — either:\n  1. Use a larger compression model:\n       auxiliary:\n         compression:\n           model: <model-with-%s+-context>\n  2. Lower the compression threshold:\n       compression:\n         threshold: 0.%02d",
			groupThousands(oldThreshold), safePct)
	} else {
		msg += fmt.Sprintf("  To make this permanent, use a larger compression model in config.yaml:\n       auxiliary:\n         compression:\n           model: <model-with-%s+-context>\n  (Lowering compression.threshold cannot help here — with %s's %s-token window, Icebath's small-context floor and output reservation would recompute the trigger to %s tokens, still above the compression model's %s.)",
			groupThousands(oldThreshold), mainLabel, groupThousands(mainCtx), groupThousands(*recomputed), groupThousands(auxContext))
	}
	emitFeasibilityNotice(a, msg)
	compressionLog().Warn(fmt.Sprintf("Auxiliary compression model %s has %d token context, below the main model's compression threshold of %d tokens — auto-lowered session threshold to %d to keep compression working.",
		auxModel, auxContext, oldThreshold, newThreshold))
}

// Reports whether the summariser is the main model on the main endpoint
func auxInheritsMainRoute(a *AIAgent, auxModel, auxBaseURL string) bool {
	if strings.ToLower(strings.TrimSpace(auxModel)) != strings.ToLower(strings.TrimSpace(a.model)) {
		return false
	}
	mainBase := provider.NormalizeRouteBaseURL(a.baseURL)
	return mainBase == "" || provider.NormalizeRouteBaseURL(auxBaseURL) == mainBase
}

// Warns when the summariser window is below the trigger and refuses one below the minimum window
func checkCompressionModelFeasibility(ctx context.Context, a *AIAgent) (err error) {
	if !a.compressionEnabled {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Debug(fmt.Sprintf("Compression feasibility check failed (non-fatal): %v", r))
			err = nil
		}
	}()
	err = probeCompressionModelFeasibility(ctx, a)
	var below *auxContextBelowMinimumError
	if err != nil && !errors.As(err, &below) {
		compressionLog().Debug(fmt.Sprintf("Compression feasibility check failed (non-fatal): %v", err))
		return nil
	}
	return err
}

func probeCompressionModelFeasibility(ctx context.Context, a *AIAgent) error {
	cfgProvider := func() (p string) {
		defer func() {
			if recover() != nil {
				p = ""
			}
		}()
		return feasibilityTaskProvider(ctx, "compression")
	}()
	runtime := map[string]any{}
	for k, v := range a.currentMainRuntime() {
		runtime[k] = v
	}
	client, auxModel, err := feasibilityAuxClient(ctx, "compression", runtime)
	if err != nil {
		return err
	}
	if client == nil || auxModel == "" {
		fbClient, fbModel, fbLabel, err := feasibilityFallbackClient(ctx, "compression", cfgProvider)
		if err != nil {
			return err
		}
		if fbClient != nil && fbModel != "" {
			client, auxModel = fbClient, fbModel
			if strings.Contains(fbLabel, "(") && strings.HasSuffix(fbLabel, ")") {
				tail := fbLabel[strings.LastIndex(fbLabel, "(")+1:]
				cfgProvider = tail[:len(tail)-1]
			}
		}
	}
	if client == nil || auxModel == "" {
		var msg string
		if cfgProvider != "" && cfgProvider != "auto" {
			msg = fmt.Sprintf("⚠ Configured auxiliary compression provider '%s' is unavailable, so older messages in long chats will be cut without a summary. Sign in to that provider again, or change auxiliary.compression in your config.", cfgProvider)
		} else {
			msg = "⚠ No auxiliary LLM provider configured: Icebath has no helper model for summarising long chats, so older messages will be cut without a summary. Run `icebath setup` to add one."
		}
		emitFeasibilityNotice(a, msg)
		compressionLog().Warn("No auxiliary LLM provider for compression — summaries will be unavailable.")
		return nil
	}
	auxBaseURL := client.BaseURL()
	auxAPIKey := client.APIKey()
	auxProvider := a.provider
	if cfgProvider != "" && cfgProvider != "auto" {
		auxProvider = cfgProvider
	}
	var auxContext int
	if a.auxCompressionContextLengthConfig == nil && auxInheritsMainRoute(a, auxModel, auxBaseURL) {
		auxContext = engineContextLength(a.contextCompressor)
	} else {
		var custom []map[string]any
		for _, entry := range a.customProviders {
			if m, ok := entry.(map[string]any); ok {
				custom = append(custom, m)
			}
		}
		cfgCtx := 0
		if a.auxCompressionContextLengthConfig != nil {
			cfgCtx = *a.auxCompressionContextLengthConfig
		}
		auxContext = feasibilityContextLength(ctx, provider.ContextLengthRequest{
			Model: auxModel, BaseURL: auxBaseURL, APIKey: auxAPIKey, Provider: auxProvider,
			ConfigContextLength: cfgCtx, CustomProviders: custom,
		})
	}
	if auxContext != 0 && auxContext < provider.MinimumContextLength {
		return &auxContextBelowMinimumError{msg: fmt.Sprintf("Auxiliary compression model %s has a context window of %s tokens, which is below the minimum %s required by Icebath Agent.  Choose a compression model with at least %dK context (set auxiliary.compression.model in config.yaml), or set auxiliary.compression.context_length to override the detected value if it is wrong.",
			auxModel, groupThousands(auxContext), groupThousands(provider.MinimumContextLength), provider.MinimumContextLength/1000)}
	}
	if auxContext < engineThresholdTokens(a.contextCompressor) {
		lowerThresholdToAuxContext(a, auxModel, auxContext, cfgProvider, auxBaseURL)
	} else if a.lastFeasibilityNotice != nil {
		a.lastFeasibilityNotice = nil
		a.compressionWarning = nil
	}
	return nil
}

// Reprobes the summariser after a main runtime change and latches only on a clean probe
func revalidateCompressionFeasibility(ctx context.Context, a *AIAgent) {
	a.compressionFeasibilityChecked = false
	if a.contextCompressor == nil {
		return
	}
	if err := checkCompressionModelFeasibility(ctx, a); err != nil {
		compressionLog().Debug(fmt.Sprintf("Compression feasibility re-check deferred to the next compaction: %v", err))
		return
	}
	a.compressionFeasibilityChecked = true
}

// Runs the deferred probe once a request first reaches the minimum summariser window
func ensureCompressionFeasibilityChecked(ctx context.Context, a *AIAgent, estimatedTokens int) {
	if a.compressionFeasibilityChecked || a.contextCompressor == nil {
		return
	}
	if estimatedTokens < provider.MinimumContextLength {
		return
	}
	if err := checkCompressionModelFeasibility(ctx, a); err != nil {
		compressionLog().Debug(fmt.Sprintf("Compression feasibility probe deferred to the first compaction: %v", err))
		return
	}
	a.compressionFeasibilityChecked = true
}

// Resends the stored compression warning to the status callback as a classified diagnostic
func (a *AIAgent) replayCompressionWarning() {
	msg, _ := a.compressionWarning.(string)
	if msg == "" || a.statusCallback == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		_ = callStatusCallbackValue(a.statusCallback, "lifecycle", config.DiagnosticText(msg))
	}()
}

// Invokes a status callback with a typed value, string shaped callbacks receive its text
func callStatusCallbackValue(cb any, kind string, value any) error {
	switch f := cb.(type) {
	case func(string, any):
		f(kind, value)
		return nil
	case func(...any):
		f(kind, value)
		return nil
	}
	return callStatusCallback(cb, kind, fmt.Sprint(value))
}
