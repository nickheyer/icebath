package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var visionImagePartTypes = map[string]bool{"image_url": true, "input_image": true}

var visionTextPartTypes = map[string]bool{"text": true, "input_text": true}

var dataURLSuffixes = map[string]string{
	"image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/jpeg": ".jpg", "image/jpg": ".jpg",
}

// Largest base64 payload a data URL may carry before materialization refuses it
const maxDataURLBase64Bytes = 20 * 1024 * 1024

// Session caches the vision preparation methods read and fill, embedded in AIAgent
type visionPrepState struct {
	anthropicImageFallbackCache map[string]string
	noListToolContentModels     map[[2]string]bool
	transportCache              map[string]provider.Transport
}

// Logger records carry the run_agent logger name of the origin module
func runAgentLog() *slog.Logger { return slog.Default().With("logger", "run_agent") }

// Python str of a value after the or default, empty for falsy values
func pyStrOr(v any, def string) string {
	if !pyrepr.Truthy(v) {
		return def
	}
	return pyrepr.Str(v)
}

// True for a dict whose type is an image part type of the vision module
func isImagePart(part any) bool {
	m, ok := part.(map[string]any)
	if !ok {
		return false
	}
	kind, ok := m["type"].(string)
	return ok && visionImagePartTypes[kind]
}

// Stripped non empty text from string parts and text typed dict parts, or every dict text when anyDictText
func salvageTextParts(content []any, anyDictText bool) []string {
	var texts []string
	for _, part := range content {
		var text string
		switch p := part.(type) {
		case string:
			text = pystr.Strip(p)
		case map[string]any:
			kind, _ := p["type"].(string)
			if !anyDictText && !visionTextPartTypes[kind] {
				continue
			}
			raw, ok := p["text"]
			if !ok {
				raw = ""
			}
			text = pystr.Strip(pyStrOr(raw, ""))
		default:
			continue
		}
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

// Lowercased provider and stripped model as recorded in the list tool content rejection set
func providerModelKey(a *AIAgent) [2]string {
	return [2]string{pystr.Lower(pystr.Strip(a.Provider)), pystr.Strip(a.Model)}
}

// True when content is a list holding at least one image part
func contentHasImageParts(content any) bool {
	list, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range list {
		if isImagePart(part) {
			return true
		}
	}
	return false
}

// Writes a data URL payload to a temp image file and returns its path, empty when the payload is too large
func materializeDataURLForVision(imageURL any) (string, error) {
	header, data, _ := strings.Cut(pyStrOr(imageURL, ""), ",")
	if n := pystr.Len(data); n > maxDataURLBase64Bytes {
		runAgentLog().Warn(fmt.Sprintf("data-URL payload too large (%d bytes), skipping", n))
		return "", nil
	}
	mime := ""
	if strings.HasPrefix(header, "data:") {
		mime = header[len("data:"):]
		mime, _, _ = strings.Cut(mime, ";")
		mime = pystr.Strip(mime)
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	suffix, ok := dataURLSuffixes[mime]
	if !ok {
		suffix = ".jpg"
	}
	tmp, err := os.CreateTemp("", "anthropic_image_*"+suffix)
	if err != nil {
		return "", err
	}
	raw, err := pybase64.B64Decode(data)
	if err == nil {
		_, err = tmp.Write(raw)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// Cached vision_analyze description note standing in for an image the active model cannot read
func (a *AIAgent) describeImageForAnthropicFallback(ctx context.Context, imageURL any, role string) (string, error) {
	source := pyStrOr(imageURL, "")
	if err := pystr.StrictUTF8EncodeError(source); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(source))
	cacheKey := hex.EncodeToString(sum[:])
	if cached := a.anthropicImageFallbackCache[cacheKey]; cached != "" {
		return cached, nil
	}
	roleLabel := map[string]string{"assistant": "assistant", "tool": "tool result"}[role]
	if roleLabel == "" {
		roleLabel = "user"
	}
	analysisPrompt := "Describe everything visible in this image in thorough detail. " +
		"Include any text, code, UI, data, objects, people, layout, colors, " +
		"and any other notable visual information."
	isDataURL := strings.HasPrefix(source, "data:")
	cleanupPath := ""
	if isDataURL {
		path, err := materializeDataURLForVision(source)
		if err != nil {
			return "", err
		}
		source, cleanupPath = path, path
	}
	description := analyzeImageDescription(ctx, a.Env, source, analysisPrompt)
	if cleanupPath != "" {
		if _, err := os.Stat(cleanupPath); err == nil {
			_ = os.Remove(cleanupPath)
		}
	}
	if description == "" {
		description = "Image analysis failed."
	}
	note := fmt.Sprintf("[The %s attached an image. Here's what it contains:\n%s]", roleLabel, description)
	if source != "" && !isDataURL {
		note += fmt.Sprintf("\n[If you need a closer look, use vision_analyze with image_url: %s]", source)
	}
	if a.anthropicImageFallbackCache == nil {
		a.anthropicImageFallbackCache = map[string]string{}
	}
	a.anthropicImageFallbackCache[cacheKey] = note
	return note, nil
}

// Stripped analysis text of a vision_analyze run, or the failure text the Python except clause builds
func analyzeImageDescription(ctx context.Context, env *tools.Env, source, prompt string) (description string) {
	defer func() {
		if r := recover(); r != nil {
			description = fmt.Sprintf("Image analysis failed: %v", r)
		}
	}()
	parsed, err := pyjson.Loads(tools.VisionAnalyzeTool(ctx, env, source, prompt))
	if err != nil {
		return "Image analysis failed: " + err.Error()
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return fmt.Sprintf("Image analysis failed: '%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	analysis, _ := obj.Get("analysis")
	if !pyrepr.Truthy(analysis) {
		return ""
	}
	text, ok := analysis.(string)
	if !ok {
		return fmt.Sprintf("Image analysis failed: '%s' object has no attribute 'strip'", pyrepr.TypeName(analysis))
	}
	return pystr.Strip(text)
}

// True when the active provider and model report native vision through the config override or the probes
func (a *AIAgent) modelSupportsVision(ctx context.Context) bool {
	v, ok := tools.LookupSupportsVision(ctx, a.Env, pystr.Strip(a.Provider), pystr.Strip(a.Model))
	return ok && v
}

// True unless the routed provider profile rejects list typed tool message content
func (a *AIAgent) providerSupportsVisionToolMessages(ctx context.Context) bool {
	return !provider.RoutedModelRejectsVisionToolMessages(ctx, pystr.Strip(a.Provider), pystr.Strip(a.Model))
}

// Image parts of a content list replaced by vision descriptions followed by the salvaged text
func (a *AIAgent) preprocessAnthropicContent(ctx context.Context, content any, role string) (any, error) {
	if !contentHasImageParts(content) {
		return content, nil
	}
	parts := content.([]any)
	var notes []string
	for _, part := range parts {
		if !isImagePart(part) {
			continue
		}
		imageData, ok := part.(map[string]any)["image_url"]
		if !ok {
			imageData = map[string]any{}
		}
		var imageURL any
		if dict, isDict := imageData.(map[string]any); isDict {
			if imageURL, ok = dict["url"]; !ok {
				imageURL = ""
			}
		} else {
			imageURL = pyStrOr(imageData, "")
		}
		if !pyrepr.Truthy(imageURL) {
			notes = append(notes, "[An image was attached but no image source was available.]")
			continue
		}
		note, err := a.describeImageForAnthropicFallback(ctx, imageURL, role)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	var kept []string
	for _, note := range notes {
		if note != "" {
			kept = append(kept, note)
		}
	}
	prefix := pystr.Strip(strings.Join(kept, "\n\n"))
	suffix := pystr.Strip(strings.Join(salvageTextParts(parts, true), "\n"))
	switch {
	case prefix != "" && suffix != "":
		return prefix + "\n\n" + suffix, nil
	case prefix != "":
		return prefix, nil
	case suffix != "":
		return suffix, nil
	}
	return "[A multimodal message was converted to text for Anthropic compatibility.]", nil
}

// Cached transport for the given api mode or the active one, nil when the mode is unregistered
func (a *AIAgent) getTransport(apiMode string) provider.Transport {
	mode := apiMode
	if mode == "" {
		mode = a.APIMode
	}
	if a.transportCache == nil {
		a.transportCache = map[string]provider.Transport{}
	}
	if a.transportCache[mode] == nil {
		a.transportCache[mode] = provider.GetTransport(mode)
	}
	return a.transportCache[mode]
}

// Deep copy of the messages with image parts turned into vision text when the active model lacks vision
func (a *AIAgent) prepareMessagesForNonVisionModel(ctx context.Context, apiMessages []any) ([]any, error) {
	hasImages := false
	for _, msg := range apiMessages {
		if m, ok := msg.(map[string]any); ok && contentHasImageParts(m["content"]) {
			hasImages = true
			break
		}
	}
	if !hasImages || a.modelSupportsVision(ctx) {
		return apiMessages, nil
	}
	transformed := pycopy.Of(apiMessages)
	for _, msg := range transformed {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, ok := m["role"]
		if !ok {
			role = "user"
		}
		content, err := a.preprocessAnthropicContent(ctx, m["content"], pyStrOr(role, "user"))
		if err != nil {
			return nil, err
		}
		m["content"] = content
	}
	return transformed, nil
}

// The same image to text transform bound under the Anthropic route name
func (a *AIAgent) prepareAnthropicMessagesForAPI(ctx context.Context, apiMessages []any) ([]any, error) {
	return a.prepareMessagesForNonVisionModel(ctx, apiMessages)
}

// Tool message content safe for the active model, image parts reduced to text where the route cannot take them
func (a *AIAgent) toolResultContentForActiveModel(ctx context.Context, toolName string, result any) any {
	if !tools.IsMultimodalToolResult(result) {
		return result
	}
	content := result.(map[string]any)["content"]
	if !pyrepr.Truthy(content) {
		content = []any{}
	}
	if !contentHasImageParts(content) {
		return content
	}
	if a.modelSupportsVision(ctx) {
		if !a.providerSupportsVisionToolMessages(ctx) {
			runAgentLog().Debug(fmt.Sprintf("Tool %s: provider %s does not accept list-type tool content — sending text summary", toolName, a.Provider))
			return tools.MultimodalTextSummary(result)
		}
		key := providerModelKey(a)
		if a.noListToolContentModels[key] {
			runAgentLog().Debug(fmt.Sprintf("Tool %s: model %s/%s known to reject list-type tool content this session — sending text summary", toolName, key[0], key[1]))
			return tools.MultimodalTextSummary(result)
		}
		return content
	}
	summary := tools.MultimodalTextSummary(result)
	if toolName == "computer_use" {
		payload := pyjson.NewObject()
		payload.Set("error", "computer_use returned screenshot/image content, but the active "+
			"model/provider does not support image input. Switch to a "+
			"vision-capable model for desktop computer use, or use browser "+
			"tools for browser tasks.")
		payload.Set("text_summary", summary)
		text, _ := pyjson.Dumps(payload)
		return text
	}
	runAgentLog().Warn(fmt.Sprintf("Tool %s returned image content for non-vision model %s/%s; falling back to text summary", toolName, a.Provider, a.Model))
	return summary
}

// Downgrades list typed tool messages holding images to text in place, remembering the route unless told not to
func (a *AIAgent) tryStripImagePartsFromToolMessages(apiMessages any, rememberModel bool) bool {
	list, ok := apiMessages.([]any)
	if !ok {
		return false
	}
	if rememberModel {
		key := providerModelKey(a)
		if a.noListToolContentModels == nil {
			a.noListToolContentModels = map[[2]string]bool{}
		}
		if key[1] != "" {
			a.noListToolContentModels[key] = true
		}
	}
	changed := false
	for _, msg := range list {
		m, ok := msg.(map[string]any)
		if !ok || m["role"] != "tool" {
			continue
		}
		content := m["content"]
		if !contentHasImageParts(content) {
			continue
		}
		text := strings.Join(salvageTextParts(content.([]any), false), "\n\n")
		if text == "" {
			text = "[image content removed — provider does not accept list-type tool message content]"
		}
		m["content"] = text
		changed = true
	}
	return changed
}

var preserveDotsProviders = map[string]bool{
	"alibaba": true, "minimax": true, "minimax-cn": true, "opencode-go": true, "opencode-zen": true,
	"zai": true, "bedrock": true, "xiaomi": true, "vertex": true,
}

// True for anthropic compatible endpoints that keep dots in model names
func (a *AIAgent) anthropicPreserveDots() bool {
	if preserveDotsProviders[pystr.Lower(a.Provider)] {
		return true
	}
	base := pystr.Lower(a.BaseURL)
	host := provider.Hostname(base)
	return strings.Contains(host, "dashscope") ||
		provider.HostMatches(base, "aliyuncs.com") ||
		strings.Contains(host, "minimax") ||
		(provider.HostMatches(base, "opencode.ai") && strings.Contains(base, "/zen/")) ||
		provider.HostMatches(base, "bigmodel.cn") ||
		provider.HostMatches(base, "xiaomimimo.com") ||
		provider.HostMatches(base, "aiplatform.googleapis.com") ||
		strings.HasPrefix(host, "bedrock-runtime.")
}

// True when the base URL targets Qwen Portal
func (a *AIAgent) isQwenPortal() bool {
	return provider.HostMatches(a.baseURLLower, "portal.qwen.ai")
}

// Deep copy of the messages shaped for Qwen Portal
func (a *AIAgent) qwenPrepareChatMessages(apiMessages []any) []any {
	prepared := pycopy.Of(apiMessages)
	a.qwenPrepareChatMessagesInplace(prepared)
	return prepared
}

// Turns every content into a part list and marks the last system part with ephemeral cache control
func (a *AIAgent) qwenPrepareChatMessagesInplace(messages []any) {
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		switch content := m["content"].(type) {
		case string:
			m["content"] = []any{map[string]any{"type": "text", "text": content}}
		case []any:
			var normalized []any
			for _, part := range content {
				switch p := part.(type) {
				case string:
					normalized = append(normalized, map[string]any{"type": "text", "text": p})
				case map[string]any:
					normalized = append(normalized, p)
				}
			}
			if len(normalized) > 0 {
				m["content"] = normalized
			}
		}
	}
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok || m["role"] != "system" {
			continue
		}
		if content, ok := m["content"].([]any); ok && len(content) > 0 {
			if last, ok := content[len(content)-1].(map[string]any); ok {
				last["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
		break
	}
}
