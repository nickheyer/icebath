package gatewayrun

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const visionAnalysisPrompt = "Concisely describe this image in 2-4 sentences " +
	"(~200 Chinese characters or ~150 English words). " +
	"Cover the main subject, key visible text/data/code, and overall context. " +
	"If it is a chart, diagram, or scientific figure, include the important " +
	"labels, legend, and key values. Skip decorative details."

// Inbound text the agent reads with attribution, media notes, transcripts, references and reply context, nil when refused
func (r *GatewayRunner) prepareInboundMessageText(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (*string, error) {
	rehomeInboundMedia(ctx, event)
	pendingText, _, pendingSTTPrepared := event.PendingSTT()
	messageText := event.Text
	if pendingSTTPrepared {
		messageText = pendingText
	}
	if sessionKey == "" {
		key, err := r.sessionKeyForSource(ctx, source)
		if err != nil {
			return nil, err
		}
		sessionKey = key
	}
	r.consumePendingNativeImagePaths(sessionKey)
	messageText = r.prefixInboundSenderContext(event, source, messageText)
	imagePaths, audioPaths, audioFilePaths, videoPaths := classifyInboundMedia(event, pendingSTTPrepared)
	if len(imagePaths) > 0 {
		messageText = r.enrichInboundImages(ctx, source, sessionKey, messageText, imagePaths)
	}
	if len(audioPaths) > 0 {
		enriched, err := r.enrichInboundVoice(ctx, event, source, messageText, audioPaths)
		if err != nil {
			return nil, err
		}
		messageText = enriched
	}
	messageText = prependInboundMediaFileNotes(ctx, messageText, audioFilePaths, videoPaths, inboundOriginalNames(event))
	messageText = prependInboundDocumentNotes(ctx, event, messageText)
	expanded, err := gateway.TurnReferences(ctx, r, event, source, sessionKey, messageText)
	if err != nil || expanded == nil {
		return nil, err
	}
	out := prependInboundReplyContext(ctx, event, source, *expanded)
	return &out, nil
}

// Inbound preprocessing under the routed profile scope when the runner multiplexes profiles
func (r *GatewayRunner) prepareProfileScopedInboundMessageText(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (*string, error) {
	scoped, err := r.routedScope(ctx, source)
	if err != nil {
		return nil, err
	}
	return r.prepareInboundMessageText(scoped, event, source, sessionKey)
}

// Expanded inbound text, nil after a refused injection whose refusal went to the chat
func (r *GatewayRunner) ExpandInboundContextReferences(ctx context.Context, source *gateway.SessionSource, sessionKey, text string) (*string, error) {
	expanded, refusal := gateway.InboundReferences(ctx, r, source, sessionKey, text)
	if refusal == nil {
		return &expanded, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	if adapter != nil {
		if _, err := adapter.Send(ctx, source.ChatID, *refusal, "", nil); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// Context window of the turn model, a global pin only for the configured route, custom limits winning
func (r *GatewayRunner) InboundModelContextLength(ctx context.Context, source *gateway.SessionSource, sessionKey string) (int, error) {
	var configCtx *int
	var cfg map[string]any
	var modelCfg any = map[string]any{}
	var customProviders []map[string]any
	func() {
		defer func() { _ = recover() }()
		cfg = gateway.LoadGatewayConfigMap(ctx, "")
		modelCfg = map[string]any{}
		if raw, ok := cfg["model"]; ok {
			modelCfg = raw
		}
		if m, ok := modelCfg.(map[string]any); ok {
			if raw, present := m["context_length"]; present && raw != nil {
				n, err := pyint.Int64(raw)
				if err != nil {
					return
				}
				value := int(n)
				configCtx = &value
			}
		}
		customProviders = provider.CompatibleCustomProviders(cfg)
	}()
	model, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, cfg, true)
	if err != nil {
		return 0, err
	}
	baseURL := noneText(runtime["base_url"])
	m, isDict := modelCfg.(map[string]any)
	var configuredModel any = modelCfg
	if isDict {
		configuredModel = m["default"]
		if !pyrepr.Truthy(configuredModel) {
			configuredModel = m["model"]
		}
	}
	if s, ok := configuredModel.(string); !ok || model != s {
		configCtx = nil
	}
	if configCtx != nil && provider.ShouldClearContextPin("", "", noneText(m["base_url"]), baseURL, noneText(m["provider"]), noneText(runtime["provider"])) {
		configCtx = nil
	}
	if len(customProviders) > 0 && baseURL != "" {
		if n := provider.CustomProviderContextLength(model, baseURL, customProviders, nil); n != 0 {
			configCtx = &n
		}
	}
	pinned := 0
	if configCtx != nil {
		pinned = *configCtx
	}
	return provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: model, BaseURL: baseURL,
		APIKey: noneText(runtime["api_key"]), ConfigContextLength: pinned, Provider: noneText(runtime["provider"]),
		CustomProviders: customProviders}), nil
}

// Images routed natively for the agent to attach, or analyzed into text under the session runtime
func (r *GatewayRunner) enrichInboundImages(ctx context.Context, source *gateway.SessionSource, sessionKey, messageText string, imagePaths []string) string {
	mode := r.decideImageInputMode(ctx, source, sessionKey)
	if mode == "native" {
		r.sessionState(sessionKey).persistent.nativeImagePaths = append([]string{}, imagePaths...)
		inboundLog().Info(fmt.Sprintf("Image routing: native (model supports vision). %d image(s) will be attached inline.", len(imagePaths)))
		return messageText
	}
	inboundLog().Info(fmt.Sprintf("Image routing: text (mode=%s). Pre-analyzing %d image(s) via vision_analyze.", mode, len(imagePaths)))
	var visionRuntime map[string]any
	if turnModel, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, nil, true); err == nil {
		visionRuntime = map[string]any{}
		for k, v := range runtime {
			visionRuntime[k] = v
		}
		visionRuntime["model"] = turnModel
	} else {
		inboundLog().Debug("vision enrichment: session runtime resolution failed", "error", err)
	}
	scoped, _ := provider.ScopedRuntimeMain(ctx, visionRuntime)
	return r.enrichMessageWithVision(scoped, messageText, imagePaths)
}

// Native or text image routing for the model this session's next turn runs on, text when the decision fails
func (r *GatewayRunner) decideImageInputMode(ctx context.Context, source *gateway.SessionSource, sessionKey string) (mode string) {
	defer func() {
		if rec := recover(); rec != nil {
			inboundLog().Debug(fmt.Sprintf("image_routing: decision failed, falling back to text — %s", pyrepr.Str(rec)))
			mode = "text"
		}
	}()
	env := inboundToolsEnv(ctx)
	resolvedProvider, resolvedModel, requestedProvider := "", "", ""
	if source != nil || sessionKey != "" {
		turnModel, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, nil, true)
		if err != nil {
			inboundLog().Debug(fmt.Sprintf("image_routing: session runtime resolution failed, falling back to config — %s", err))
		} else {
			resolvedModel = strings.TrimSpace(turnModel)
			if p, ok := runtime["provider"].(string); ok {
				resolvedProvider = strings.TrimSpace(p)
			}
			if p, ok := runtime["requested_provider"].(string); ok {
				requestedProvider = strings.TrimSpace(p)
			}
		}
	}
	if resolvedProvider == "" {
		resolvedProvider = provider.AuxReadMainProvider(ctx)
	}
	if resolvedModel == "" {
		resolvedModel = provider.AuxReadMainModel(ctx)
	}
	return tools.DecideImageInputMode(ctx, env, resolvedProvider, resolvedModel, requestedProvider)
}

// Text with a vision description and the cached path of each attached image prepended
func (r *GatewayRunner) enrichMessageWithVision(ctx context.Context, userText string, imagePaths []string) string {
	env := inboundToolsEnv(ctx)
	parts := make([]string, 0, len(imagePaths))
	for _, path := range imagePaths {
		inboundLog().Debug("Auto-analyzing user image: " + path)
		var result map[string]any
		if err := json.Unmarshal([]byte(tools.VisionAnalyzeTool(ctx, env, path, visionAnalysisPrompt)), &result); err != nil {
			inboundLog().Error(fmt.Sprintf("Vision auto-analysis error: %s", err))
			parts = append(parts, "[The user sent an image but something went wrong when I "+
				"tried to look at it~ You can try examining it yourself "+
				"with vision_analyze using image_url: "+path+"]")
			continue
		}
		if pyrepr.Truthy(result["success"]) {
			analysis, _ := result["analysis"].(string)
			parts = append(parts, "[The user sent an image~ Here's what I can see:\n"+memory.SanitizeContext(analysis)+"]\n"+
				"[If you need a closer look, use vision_analyze with "+
				"image_url: "+path+" ~]")
			continue
		}
		parts = append(parts, "[The user sent an image but I couldn't quite see it "+
			"this time (>_<) You can try looking at it yourself "+
			"with vision_analyze using image_url: "+path+"]")
	}
	if len(parts) == 0 {
		return userText
	}
	prefix := strings.Join(parts, "\n\n")
	if userText != "" {
		return prefix + "\n\n" + userText
	}
	return prefix
}

// Persists the active turn marker under the resolved routing key and stamps its token on the event
func (r *GatewayRunner) markDurableActiveTurn(ctx context.Context, event *gateway.MessageEvent, sessionKey string) bool {
	token, err := r.AsyncSessionStore().Store().MarkTurnActive(ctx, sessionKey)
	if err != nil {
		inboundLog().Warn(fmt.Sprintf("Could not persist active-turn marker for %s: %s", sessionKey, err))
		return false
	}
	if token == "" {
		return false
	}
	event.SetGatewayActiveTurnMarker(sessionKey, token)
	return true
}

// Compare and swap clear of the marker the event owns in three attempts, dropping the event's marker either way
func (r *GatewayRunner) clearDurableActiveTurn(ctx context.Context, event *gateway.MessageEvent) bool {
	defer event.ClearGatewayActiveTurnMarker()
	sessionKey := event.GatewayActiveTurnSessionKey()
	token, _ := event.GatewayActiveTurnToken().(string)
	if sessionKey == nil || *sessionKey == "" || token == "" {
		return false
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		cleared, err := r.AsyncSessionStore().Store().ClearTurnActive(ctx, *sessionKey, token)
		if err == nil {
			return cleared
		}
		lastErr = err
		if attempt < 3 {
			inboundLog().Debug(fmt.Sprintf("Retrying active-turn marker cleanup for %s (%d/3): %s", *sessionKey, attempt, err))
		}
	}
	inboundLog().Warn(fmt.Sprintf("Could not clear active-turn marker for %s after 3 attempts: %s", *sessionKey, lastErr))
	return false
}
