package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Single line quote of a background prompt clipped to the limit with an ellipsis
func bgPromptPreview(prompt string, limit int) string {
	text := strings.Join(pystr.SplitWhitespace(prompt, -1), " ")
	if pystr.Len(text) <= limit {
		return text
	}
	return pystr.Rstrip(pystr.Slice(text, 0, limit-1)) + "…"
}

// Runs a background task inside the source's profile scope
func (r *GatewayRunner) runBackgroundTask(ctx context.Context, prompt string, source *gateway.SessionSource, taskID string,
	eventMessageID *string, mediaURLs, mediaTypes []string) error {
	scoped, err := r.profileScopeForSource(ctx, source)
	if err != nil {
		return err
	}
	return r.runBackgroundTaskInner(scoped, prompt, source, taskID, eventMessageID, mediaURLs, mediaTypes)
}

// Runs a fresh agent on the prompt and delivers its text, images and media to the chat
func (r *GatewayRunner) runBackgroundTaskInner(ctx context.Context, prompt string, source *gateway.SessionSource, taskID string,
	eventMessageID *string, mediaURLs, mediaTypes []string) error {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter == nil {
		runLog().Warn(fmt.Sprintf("No adapter for platform %s in background task %s", source.Platform, taskID))
		return nil
	}
	threadMetadata := r.threadMetadataForSource(source, eventMessageID)
	if err := r.backgroundTaskBody(ctx, adapter, prompt, source, taskID, threadMetadata, mediaURLs, mediaTypes); err != nil {
		runLog().Error(fmt.Sprintf("Background task %s failed", taskID), "error", err)
		_, _ = adapter.EmitWarning(ctx, source.ChatID,
			i18n.T(ctx, "gateway.background.failed", "", map[string]any{"preview": bgPromptPreview(prompt, 60)}),
			"", threadMetadata, source.Platform)
	}
	return nil
}

// Agent run and delivery of a background task, an error for the failure notice
func (r *GatewayRunner) backgroundTaskBody(ctx context.Context, adapter gateway.PlatformAdapter, prompt string, source *gateway.SessionSource,
	taskID string, threadMetadata map[string]any, mediaURLs, mediaTypes []string) error {
	userConfig := gateway.LoadGatewayConfigMap(ctx, "")
	model, runtimeKwargs, err := r.resolveSessionAgentRuntime(ctx, source, "", userConfig, true)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(runtimeKwargs["api_key"]) {
		_, err := adapter.Send(ctx, source.ChatID, i18n.T(ctx, "gateway.background.no_credentials", "", nil), "", threadMetadata)
		return err
	}
	platformKey := platformConfigKey(source.Platform)
	enabledToolsets, disabledToolsets := r.resolveTurnToolsets(ctx, userConfig, source, platformKey)
	pr := r.providerRouting
	maxIterations := gateway.CurrentMaxIterations(ctx)
	reasoningConfig, err := r.resolveSessionReasoningConfig(ctx, source, "", model)
	if err != nil {
		return err
	}
	r.reasoningConfig = reasoningConfig
	serviceTier, err := r.resolveSessionServiceTier(ctx, source, "")
	if err != nil {
		return err
	}
	r.serviceTier = serviceTier
	route := r.resolveTurnAgentConfig(model, runtimeKwargs, tierFromRunner)
	enrichedPrompt := prompt
	var imagePaths []string
	for i, path := range mediaURLs {
		mtype := ""
		if i < len(mediaTypes) {
			mtype = mediaTypes[i]
		}
		if strings.HasPrefix(mtype, "image/") {
			imagePaths = append(imagePaths, path)
		}
	}
	if len(imagePaths) > 0 {
		enrichedPrompt = r.enrichMessageWithVision(ctx, prompt, imagePaths)
	}
	runSync := func(ctx context.Context) (any, error) {
		bgEnv := tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
		if base := tools.ToolEnvFrom(ctx); base != nil {
			bgEnv = *base
		}
		values := &sync.Map{}
		if bgEnv.Values != nil {
			bgEnv.Values.Range(func(k, v any) bool { values.Store(k, v); return true })
		}
		bgEnv.Values = values
		tools.DeclareStatelessChannel(&bgEnv)
		bgAgent, err := r.newBackgroundAgent(ctx, route, userConfig, maxIterations, enabledToolsets, disabledToolsets,
			reasoningConfig, pr, taskID, platformKey, source, &bgEnv)
		if err != nil {
			return nil, err
		}
		defer r.cleanupAgentResources(ctx, bgAgent)
		return bgAgent.RunConversation(ctx, enrichedPrompt, agent.RunConversationOptions{TaskID: taskID})
	}
	raw, err := r.runInExecutorWithContext(ctx, runSync)
	if err != nil {
		return err
	}
	result, _ := raw.(map[string]any)
	response := backgroundResponse(ctx, result)
	header := i18n.T(ctx, "gateway.background.complete_header", "", map[string]any{"preview": preview(prompt, 60)})
	var images []gateway.ImageRef
	var mediaFiles []gateway.MediaRef
	textContent := ""
	if response != "" {
		if mediaFiles, response, err = gateway.ExtractMedia(ctx, response); err != nil {
			return err
		}
		if mediaFiles, err = gateway.FilterMediaDeliveryPaths(ctx, mediaFiles); err != nil {
			return err
		}
		images, textContent = gateway.ExtractImages(response)
	}
	if textContent != "" {
		if _, err := adapter.Send(ctx, source.ChatID, header+textContent, "", threadMetadata); err != nil {
			return err
		}
	} else if len(images) == 0 && len(mediaFiles) == 0 {
		if _, err := adapter.Send(ctx, source.ChatID, header+i18n.T(ctx, "gateway.background.no_response", "", nil), "", threadMetadata); err != nil {
			return err
		}
	}
	for _, image := range images {
		_, _ = adapter.SendImage(ctx, source.ChatID, image.URL, image.Alt, "", threadMetadata)
	}
	for _, m := range mediaFiles {
		_, ext := pypath.Splitext(m.Path)
		ext = pystr.Lower(ext)
		switch {
		case gateway.ShouldSendMediaAsAudio(source.Platform, ext, m.IsVoice):
			_, _ = adapter.SendVoice(ctx, source.ChatID, m.Path, "", "", threadMetadata, map[string]any{"is_voice": m.IsVoice})
		case notifyVideoExts[ext]:
			_, _ = adapter.SendVideo(ctx, source.ChatID, m.Path, "", "", threadMetadata, nil)
		case notifyImageExts[ext]:
			_, _ = adapter.SendImageFile(ctx, source.ChatID, m.Path, "", "", threadMetadata, nil)
		default:
			_, _ = adapter.SendDocument(ctx, source.ChatID, m.Path, "", "", "", threadMetadata, nil)
		}
	}
	return nil
}

// Final text of a background run, its error when it gave none, media paths repaired from the run's messages
func backgroundResponse(ctx context.Context, result map[string]any) string {
	if result == nil {
		return ""
	}
	response := ""
	if result["final_response"] != nil {
		response = pyrepr.Str(result["final_response"])
	}
	if response == "" && pyrepr.Truthy(result["error"]) {
		response = i18n.T(ctx, "gateway.shared.error_prefix", "", map[string]any{"error": result["error"]})
	}
	if response == "" {
		return ""
	}
	rows := resultMessages(result)
	messages := make([]any, len(rows))
	for i, m := range rows {
		messages[i] = m
	}
	return gateway.RepairExplicitComputerUseMediaPaths(response, messages)
}

// Fresh quiet agent of a background task with the turn route, toolsets, provider routing and source identity
func (r *GatewayRunner) newBackgroundAgent(ctx context.Context, route turnRoute, userConfig map[string]any, maxIterations int,
	enabledToolsets, disabledToolsets []string, reasoningConfig, pr map[string]any, taskID, platformKey string,
	source *gateway.SessionSource, env *tools.Env) (*agent.AIAgent, error) {
	kwargs := map[string]any{"model": route.model}
	add := func(values map[string]any) error {
		for k, v := range values {
			if _, dup := kwargs[k]; dup {
				return &pyTypeError{fmt.Sprintf("AIAgent() got multiple values for keyword argument '%s'", k)}
			}
			kwargs[k] = v
		}
		return nil
	}
	if err := add(route.runtime); err != nil {
		return nil, err
	}
	if err := add(gateway.CheckpointAgentKwargs(userConfig)); err != nil {
		return nil, err
	}
	var serviceTier any
	if r.serviceTier != nil {
		serviceTier = *r.serviceTier
	}
	var sessionDB any
	if db := r.sessionDB(ctx); db != nil {
		sessionDB = db.DB()
	}
	requireParameters, present := pr["require_parameters"]
	if !present {
		requireParameters = false
	}
	if err := add(map[string]any{
		"max_iterations": maxIterations, "quiet_mode": true, "verbose_logging": false,
		"enabled_toolsets": enabledToolsets, "disabled_toolsets": disabledToolsets, "reasoning_config": reasoningConfig,
		"service_tier": serviceTier, "request_overrides": route.requestOverrides, "providers_allowed": pr["only"],
		"providers_ignored": pr["ignore"], "providers_order": pr["order"], "provider_sort": pr["sort"],
		"provider_require_parameters": requireParameters, "provider_data_collection": pr["data_collection"],
		"session_id": taskID, "platform": platformKey, "user_id": optAny(source.UserID), "user_id_alt": optAny(source.UserIDAlt),
		"user_name": optAny(source.UserName), "chat_id": source.ChatID, "chat_name": optAny(source.ChatName),
		"chat_type": source.ChatType, "thread_id": optAny(source.ThreadID), "session_db": sessionDB,
		"fallback_model": r.refreshFallbackModel(ctx),
	}); err != nil {
		return nil, err
	}
	opts, err := agent.AIAgentOptionsFromKwargs(kwargs)
	if err != nil {
		return nil, err
	}
	opts.Env = env
	return newAIAgent(ctx, opts)
}
