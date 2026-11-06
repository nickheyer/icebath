package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Resolves the per request num_ctx from the config override or local server detection, capped by context_length
func configureOllamaNumCtx(ctx context.Context, a *AIAgent, modelCfg map[string]any, configContextLength int) {
	a.ollamaNumCtx = 0
	var numCtx *int
	override, hasOverride := modelCfg["ollama_num_ctx"]
	hasOverride = hasOverride && override != nil
	if hasOverride {
		if n, err := pyint.Of(override); err == nil && n.IsInt64() {
			v := int(n.Int64())
			numCtx = &v
		} else {
			runAgentLog().Debug("Invalid ollama_num_ctx config value: " + pyrepr.Repr(override))
		}
	}
	if numCtx == nil && a.baseURL != "" && provider.IsLocalEndpoint(a.baseURL) {
		if detected, ok := provider.QueryOllamaNumCtx(ctx, a.model, a.baseURL, a.apiKey); ok && detected > 0 {
			numCtx = &detected
		}
	}
	if numCtx != nil {
		a.ollamaNumCtx = *numCtx
	}
	if a.ollamaNumCtx != 0 && configContextLength != 0 && !hasOverride && a.ollamaNumCtx > configContextLength {
		runAgentLog().Info(fmt.Sprintf("Ollama num_ctx capped: %d -> %d (model.context_length override)", a.ollamaNumCtx, configContextLength))
		a.ollamaNumCtx = configContextLength
	}
	if a.ollamaNumCtx != 0 && !a.quietMode {
		source := "model max from Ollama /api/show"
		if hasOverride {
			source = "model.ollama_num_ctx"
		}
		runAgentLog().Info(fmt.Sprintf("Local server num_ctx: will request %d tokens (%s)", a.ollamaNumCtx, source))
	}
}

// Recalibrates the compressor to the served num_ctx window so its trigger can fire below it
func clampCompressorToOllamaNumCtx(a *AIAgent) {
	if a.contextCompressor == nil {
		return
	}
	window := a.contextCompressor.Base().ContextLength
	if a.ollamaNumCtx > 0 && window != 0 && a.ollamaNumCtx < window {
		runAgentLog().Info(fmt.Sprintf("Compressor window clamped to Ollama num_ctx: %d -> %d", window, a.ollamaNumCtx))
		a.contextCompressor.UpdateModel(a.model, a.ollamaNumCtx, a.baseURL, a.apiKey, a.provider, a.apiMode)
	}
}
