package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Resolves the per request num_ctx from the model.ollama_num_ctx setting of ctx or local server detection, capped by context_length
func configureOllamaNumCtx(ctx context.Context, a *AIAgent, configContextLength int) {
	a.ollamaNumCtx = 0
	var numCtx *int
	override, hasOverride := settings.ModelOllamaNumCtx.Lookup(ctx)
	if hasOverride {
		numCtx = &override
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
		pkgLog().Info(fmt.Sprintf("Ollama num_ctx capped: %d -> %d (model.context_length override)", a.ollamaNumCtx, configContextLength))
		a.ollamaNumCtx = configContextLength
	}
	if a.ollamaNumCtx != 0 && !a.quietMode {
		source := "model max from Ollama /api/show"
		if hasOverride {
			source = "model.ollama_num_ctx"
		}
		pkgLog().Info(fmt.Sprintf("Local server num_ctx: will request %d tokens (%s)", a.ollamaNumCtx, source))
	}
}

// Recalibrates the compressor to the served num_ctx window so its trigger can fire below it
func clampCompressorToOllamaNumCtx(a *AIAgent) {
	if a.contextCompressor == nil {
		return
	}
	window := engineContextLength(a.contextCompressor)
	if a.ollamaNumCtx > 0 && window != 0 && a.ollamaNumCtx < window {
		pkgLog().Info(fmt.Sprintf("Compressor window clamped to Ollama num_ctx: %d -> %d", window, a.ollamaNumCtx))
		a.contextCompressor.UpdateModel(a.model, a.ollamaNumCtx, a.baseURL, a.apiKey, a.provider, a.apiMode)
	}
}
