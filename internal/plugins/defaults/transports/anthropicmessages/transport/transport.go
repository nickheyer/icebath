package transport

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"

	"github.com/nickheyer/icebath/internal/config"
	anthropicmessagesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/transports/anthropicmessages/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const carrier = string(provider.ModeAnthropicMessages)

type registrant struct {
	config *config.Config
}

func Register(registry *transport.Registry, cfg *config.Config) {
	registry.Register(registrant{config: cfg})
}

func (r registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeAnthropicMessages: func(route provider.Route) (provider.Client, error) {
			return &client{config: r.config, route: route}, nil
		},
	}
}

type client struct {
	config *config.Config
	route  provider.Route
}

func (c *client) Route() provider.Route {
	return c.route
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	options := []option.RequestOption{option.WithMaxRetries(0), option.WithBaseURL(normalized(c.route.BaseURL))}
	token := isOAuthToken(strings.TrimSpace(c.route.APIKey))
	subscription := token && !bearer(c.route.BaseURL) && !isThirdPartyAnthropicEndpoint(c.route.BaseURL)
	identity := token && (isNativeAnthropicHost(c.route.BaseURL) || c.route.Spec == "anthropic")
	switch key := c.route.APIKey; {
	case key == "":
	case c.route.Auth == provider.AuthOAuth || bearer(c.route.BaseURL) || subscription:
		options = append(options, option.WithAuthToken(key))
	case official(c.route.BaseURL):
		options = append(options, option.WithAPIKey(key))
	default:
		options = append(options, option.WithAPIKey(key), option.WithMiddleware(func(call *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			call.Header.Del("Authorization")
			call.Header.Set("X-Api-Key", key)
			return next(call)
		}))
	}
	for name, value := range c.route.Headers {
		options = append(options, option.WithHeader(name, value))
	}
	if subscription {
		options = append(options, option.WithHeader("User-Agent", anthClaudeCodeUserAgent()), option.WithHeader("x-app", "cli"))
	}
	if identity {
		options = append(options, option.WithMiddleware(identify))
	}
	params := anthropic.BetaMessageNewParams{
		Model:        request.Model,
		MaxTokens:    int64(cmp.Or(request.MaxTokens, c.config.GetInt(anthropicmessagesconfig.MaxTokens))),
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	if request.Reasoning.Effort != "none" {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{Display: anthropic.BetaThinkingConfigAdaptiveDisplaySummarized}}
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(request.Reasoning.Effort)
	}
	if c.config.GetBool(anthropicmessagesconfig.Fallbacks) && official(c.route.BaseURL) && !subscription {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	if subscription {
		params.Betas = append(params.Betas, anthropicOAuthOnlyBetas...)
	}
	known := map[string]bool{}
	for _, tool := range request.Tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		known[name] = true
		description, _ := function["description"].(string)
		params.Tools = append(params.Tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        name,
			Description: anthropic.String(description),
			InputSchema: param.Override[anthropic.BetaToolInputSchemaParam](function["parameters"]),
		}})
	}
	results := false
	for _, message := range request.Messages {
		switch message.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			params.System = append(params.System, anthropic.BetaTextBlockParam{Text: message.GetText()})
		case sessionsv1.Role_ROLE_USER:
			params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(message.GetText())))
		case sessionsv1.Role_ROLE_TOOL:
			result := anthropic.NewBetaToolResultBlock(message.GetToolCallId(), message.GetText(), message.GetFailed())
			if results {
				last := params.Messages[len(params.Messages)-1].OfUser
				last.Content = append(last.Content, result)
			} else {
				params.Messages = append(params.Messages, anthropic.NewBetaUserMessage(result))
			}
		case sessionsv1.Role_ROLE_ASSISTANT:
			if content := echo(message, known); len(content) > 0 {
				params.Messages = append(params.Messages, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{Content: content}})
			}
		}
		results = message.GetRole() == sessionsv1.Role_ROLE_TOOL
	}

	api := anthropic.NewClient(options...)
	stream := api.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var reply anthropic.BetaMessage
	for stream.Next() {
		event := stream.Current()
		if err := reply.Accumulate(event); err != nil {
			return nil, err
		}
		if block, ok := event.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			var err error
			switch part := block.Delta.AsAny().(type) {
			case anthropic.BetaTextDelta:
				err = fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: part.Text})
			case anthropic.BetaThinkingDelta:
				err = fn(provider.StreamEvent{Kind: provider.EventReasoningDelta, Text: part.Thinking})
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, c.failure(ctx, request, err)
	}
	finish := "stop"
	switch reply.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, &provider.APIError{Provider: c.route.Provider, Model: request.Model, Kind: provider.ErrContentFilter,
			Message: strings.TrimSpace("the model declined this request " + reply.StopDetails.Explanation)}
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		finish = "length"
	case anthropic.BetaStopReasonToolUse:
		finish = "tool_calls"
	}
	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Carriers: map[string][]byte{carrier: []byte(reply.RawJSON())}}
	edge := boundary(reply.Content)
	for i, block := range reply.Content {
		switch block.Type {
		case "text":
			message.Text += block.Text
		case "thinking":
			message.Reasoning += block.Thinking
		case "tool_use":
			if i > edge {
				name := block.Name
				if subscription || identity {
					name = inboundToolName(name, known)
				}
				message.ToolCalls = append(message.ToolCalls, &sessionsv1.ToolCall{Id: block.ID, Name: name, Arguments: string(block.Input)})
			}
		}
	}
	return &provider.Response{Message: message, FinishReason: finish}, nil
}

func (c *client) failure(ctx context.Context, request *provider.Request, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := &provider.APIError{Provider: c.route.Provider, Model: request.Model, Message: err.Error(), Kind: provider.ErrNetwork, Err: err}
	var status *anthropic.Error
	if errors.As(err, &status) {
		failure.StatusCode, failure.Kind = status.StatusCode, provider.KindOf(status.StatusCode)
	}
	return failure
}

var trailingV1 = regexp.MustCompile(`/v1/?$`)

var minimaxPrefixes = []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"}

func normalized(base string) string {
	return trailingV1.ReplaceAllString(strings.TrimRight(strings.TrimSpace(base), "/"), "")
}

func bearer(base string) bool {
	lower := strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
	if strings.Contains(lower, "azure.com") || slices.ContainsFunc(minimaxPrefixes, func(prefix string) bool { return strings.HasPrefix(lower, prefix) }) {
		return true
	}
	for _, domain := range []string{"palantirfoundry.com", "api.commandcode.ai"} {
		if provider.BaseHostMatches(lower, domain) {
			return true
		}
	}
	return false
}

func official(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.anthropic.com")
}

func identify(call *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if call.Body == nil {
		return next(call)
	}
	raw, err := io.ReadAll(call.Body)
	if err != nil {
		return nil, err
	}
	if err := call.Body.Close(); err != nil {
		return nil, err
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	tools, messages := objects(body["tools"]), objects(body["messages"])
	body["system"] = anthApplyClaudeCodeIdentity(body["system"], tools, messages, anthOAuthWireNamer(tools))
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return nil, err
	}
	sent := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	call.Body, call.ContentLength = io.NopCloser(bytes.NewReader(sent)), int64(len(sent))
	call.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(sent)), nil }
	return next(call)
}

func objects(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if object, ok := item.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}

func echo(message *sessionsv1.Message, known map[string]bool) []anthropic.BetaContentBlockParamUnion {
	var content []anthropic.BetaContentBlockParamUnion
	var reply anthropic.BetaMessage
	if raw := message.GetCarriers()[carrier]; len(raw) > 0 && json.Unmarshal(raw, &reply) == nil {
		edge := boundary(reply.Content)
		for i, block := range reply.Content {
			if block.Type == "text" && block.Text != "" || block.Type != "text" && i > edge {
				part := block.ToParam()
				if use := part.OfToolUse; use != nil {
					use.Name = inboundToolName(use.Name, known)
				}
				content = append(content, part)
			}
		}
		return content
	}
	if message.GetText() != "" {
		content = append(content, anthropic.NewBetaTextBlock(message.GetText()))
	}
	for _, call := range message.GetToolCalls() {
		input := json.RawMessage(call.GetArguments())
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		content = append(content, anthropic.NewBetaToolUseBlock(call.GetId(), input, call.GetName()))
	}
	return content
}

func boundary(content []anthropic.BetaContentBlockUnion) int {
	edge := -1
	for i, block := range content {
		if block.Type == "fallback" {
			edge = i
		}
	}
	return edge
}
