package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type providerRequestHeadersKey struct{}

// Context whose outgoing provider requests carry the extra headers of one call over the client headers
func withProviderRequestHeaders(ctx context.Context, headers map[string]string) context.Context {
	return context.WithValue(ctx, providerRequestHeadersKey{}, copyStringMap(headers))
}

// Per request extra headers bound on the context, nil when the call passed none
func providerRequestHeaders(ctx context.Context) map[string]string {
	headers, _ := ctx.Value(providerRequestHeadersKey{}).(map[string]string)
	return headers
}

// Sets the per request extra headers over every header the client already set
func applyProviderRequestHeaders(ctx context.Context, header http.Header) {
	for k, v := range providerRequestHeaders(ctx) {
		header.Set(k, v)
	}
}

// Auxiliary client over a profile built provider client, OpenAI chat kwargs in and chat completion maps out
type auxProviderClient struct {
	auxEffectiveStamp
	client Client
}

// Profile created provider client speaking the OpenAI chat completions surface of an auxiliary client
func auxProviderClientAux(ctx context.Context, client Client) AuxClient {
	return &auxProviderClient{client: client}
}

func (c *auxProviderClient) BaseURL() string { return c.client.Route().BaseURL }

func (c *auxProviderClient) APIKey() string { return c.client.Route().APIKey }

func (c *auxProviderClient) Close() error {
	if closer, ok := c.client.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Provider request of OpenAI chat completion kwargs
func auxRequestFromChatKwargs(kwargs map[string]any) (*Request, map[string]string, error) {
	req := &Request{Model: kwargString(kwargs, "model"), ToolChoice: kwargs["tool_choice"]}
	for _, m := range auxDictList(kwargs["messages"]) {
		req.Messages = append(req.Messages, state.MessageFromDict(m))
	}
	req.Tools = auxDictList(kwargs["tools"])
	if v, ok := kwargs["parallel_tool_calls"].(bool); ok {
		req.ParallelToolCalls = &v
	}
	req.MaxTokens = kwargInt(kwargs, "max_tokens")
	if req.MaxTokens == 0 {
		req.MaxTokens = kwargInt(kwargs, "max_completion_tokens")
	}
	var err error
	if req.Temperature, err = auxFloatKwarg("temperature", kwargs["temperature"]); err != nil {
		return nil, nil, err
	}
	if req.TopP, err = auxFloatKwarg("top_p", kwargs["top_p"]); err != nil {
		return nil, nil, err
	}
	switch stop := kwargs["stop"].(type) {
	case string:
		req.Stop = []string{stop}
	case []string:
		req.Stop = stop
	case []any:
		for _, s := range stop {
			req.Stop = append(req.Stop, pyrepr.Str(s))
		}
	}
	extraBody, _ := kwargs["extra_body"].(map[string]any)
	switch {
	case kwargs["reasoning_effort"] != nil:
		req.Reasoning = Reasoning{Effort: pyrepr.Str(kwargs["reasoning_effort"])}
	case extraBody["reasoning"] != nil:
		req.Reasoning = reasoningFromConfig(extraBody["reasoning"])
	}
	if rf, ok := kwargs["response_format"].(map[string]any); ok {
		req.ResponseFormat = rf
	}
	if len(extraBody) > 0 {
		req.ExtraBody = make(map[string]any, len(extraBody))
		for k, v := range extraBody {
			if k != "reasoning" || kwargs["reasoning_effort"] != nil {
				req.ExtraBody[k] = v
			}
		}
	}
	if timeout, err := auxFloatKwarg("timeout", kwargs["timeout"]); err != nil {
		return nil, nil, err
	} else if timeout != nil {
		req.Timeout = time.Duration(*timeout * float64(time.Second))
	}
	req.ServiceTier = kwargString(kwargs, "service_tier")
	req.User = kwargString(kwargs, "user")
	if md, ok := kwargs["metadata"].(map[string]any); ok {
		req.Metadata = make(map[string]string, len(md))
		for k, v := range md {
			req.Metadata[k] = pyrepr.Str(v)
		}
	}
	var headers map[string]string
	switch h := kwargs["extra_headers"].(type) {
	case map[string]string:
		headers = h
	case map[string]any:
		headers = make(map[string]string, len(h))
		for k, v := range h {
			headers[k] = pyrepr.Str(v)
		}
	}
	req.Stream = kwargBool(kwargs, "stream", false)
	return req, headers, nil
}

// OpenAI usage dict of provider usage counts
func auxUsageDict(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens,
		"prompt_tokens_details":     map[string]any{"cached_tokens": u.CacheReadTokens, "cache_write_tokens": u.CacheWriteTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
	}
}

// Chat completion dict of a provider response
func auxChatCompletionOf(resp *Response, model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": nil}
	if resp.Message != nil {
		message = messageToDict(resp.Message)
		message["role"] = "assistant"
		if resp.Message.Content == nil {
			message["content"] = nil
		}
	}
	finish := resp.FinishReason
	var finishValue any
	if finish != "" {
		finishValue = finish
	}
	return map[string]any{
		"id": resp.RequestID, "object": "chat.completion", "model": auxOrDefault(resp.Model, model),
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishValue}},
		"usage":   auxUsageDict(resp.Usage),
	}
}

// Chat completion dict, or a chunk stream when the kwargs ask for streaming
func (c *auxProviderClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	req, headers, err := auxRequestFromChatKwargs(kwargs)
	if err != nil {
		return nil, err
	}
	if len(headers) > 0 {
		ctx = withProviderRequestHeaders(ctx, headers)
	}
	if !req.Stream {
		resp, err := c.client.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		return auxChatCompletionOf(resp, req.Model), nil
	}
	return auxStartProviderStream(ctx, c.client, req), nil
}

type auxProviderStream struct {
	chunks chan map[string]any
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
	once   sync.Once
}

// Chunk stream fed by the provider stream callback on its own goroutine
func auxStartProviderStream(ctx context.Context, client Client, req *Request) *auxProviderStream {
	streamCtx, cancel := context.WithCancel(ctx)
	s := &auxProviderStream{chunks: make(chan map[string]any), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.chunks)
		send := func(chunk map[string]any) error {
			select {
			case s.chunks <- chunk:
				return nil
			case <-streamCtx.Done():
				return streamCtx.Err()
			}
		}
		toolIndex := map[string]int{}
		resp, err := client.Stream(streamCtx, req, func(ev StreamEvent) error {
			delta := map[string]any{}
			switch ev.Kind {
			case EventTextDelta:
				delta["content"] = ev.Text
			case EventReasoningDelta:
				delta["reasoning_content"] = ev.Text
			case EventToolCallStart, EventToolCallDelta:
				if ev.ToolCall == nil {
					return nil
				}
				idx, seen := toolIndex[ev.ToolCall.ID]
				if !seen {
					idx = len(toolIndex)
					toolIndex[ev.ToolCall.ID] = idx
				}
				call := map[string]any{"index": idx, "function": map[string]any{"arguments": ev.ToolCall.Function.Arguments}}
				if ev.Kind == EventToolCallStart {
					call["id"] = ev.ToolCall.ID
					call["type"] = "function"
					call["function"].(map[string]any)["name"] = ev.ToolCall.Function.Name
				}
				delta["tool_calls"] = []any{call}
			default:
				return nil
			}
			return send(map[string]any{"object": "chat.completion.chunk", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
		})
		if err == nil && resp != nil {
			err = send(map[string]any{"id": resp.RequestID, "object": "chat.completion.chunk", "model": auxOrDefault(resp.Model, req.Model),
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": auxOrDefault(resp.FinishReason, "stop")}},
				"usage":   auxUsageDict(resp.Usage)})
		}
		if err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
		}
	}()
	return s
}

// Next chunk, io.EOF after the final chunk, the provider error when the stream failed
func (s *auxProviderStream) Next(ctx context.Context) (map[string]any, error) {
	select {
	case chunk, ok := <-s.chunks:
		if ok {
			return chunk, nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err != nil && !errors.Is(s.err, context.Canceled) {
			return nil, s.err
		}
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *auxProviderStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		for range s.chunks {
		}
	})
	return nil
}

// AuxAdapter serves tools.Aux and the MCP sampling chat surface through CallLLM
type AuxAdapter struct{}

// Complete runs one auxiliary completion of transcript messages and returns its text
func (AuxAdapter) Complete(ctx context.Context, messages []*state.Message, opts tools.AuxOptions) (string, error) {
	dicts := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		dicts = append(dicts, messageToDict(m))
	}
	kwargs := auxAdapterKwargs(dicts, nil, opts)
	resp, err := CallLLM(ctx, kwargs)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(ExtractContentOrReasoning(resp, -1)), nil
}

// CompleteChat runs one auxiliary chat completion forwarding tool definitions and returns the completion dict
func (AuxAdapter) CompleteChat(ctx context.Context, messages []map[string]any, toolDefs []map[string]any, opts tools.AuxOptions) (map[string]any, error) {
	resp, err := CallLLM(ctx, auxAdapterKwargs(messages, toolDefs, opts))
	if err != nil {
		return nil, err
	}
	if m, ok := resp.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("auxiliary %s call returned %s, not a chat completion", auxTaskOrCall(opts.Task), auxTypeName(resp))
}

// call_llm kwargs of adapter options
func auxAdapterKwargs(messages, toolDefs []map[string]any, opts tools.AuxOptions) map[string]any {
	kwargs := map[string]any{"messages": messages}
	if opts.Task != "" {
		kwargs["task"] = opts.Task
	}
	if opts.Model != "" {
		kwargs["model"] = opts.Model
	}
	if opts.MaxTokens > 0 {
		kwargs["max_tokens"] = opts.MaxTokens
	}
	if opts.Temperature != nil {
		kwargs["temperature"] = *opts.Temperature
	}
	if opts.Timeout > 0 {
		kwargs["timeout"] = opts.Timeout.Seconds()
	}
	if len(toolDefs) > 0 {
		kwargs["tools"] = toolDefs
	}
	if opts.JSONSchema != nil {
		kwargs["extra_body"] = map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": opts.JSONSchema}}
	}
	return kwargs
}
