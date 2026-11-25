package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

type registrant struct{}

func Register(registry *transport.Registry) {
	registry.Register(registrant{})
}

func (registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeChatCompletions: func(route provider.Route) (provider.Client, error) {
			return &client{route: route}, nil
		},
	}
}

type client struct {
	route provider.Route
}

type chunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) Route() provider.Route {
	return c.route
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	messages := []map[string]any{}
	for _, message := range request.Messages {
		switch message.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			messages = append(messages, map[string]any{"role": "system", "content": message.GetText()})
		case sessionsv1.Role_ROLE_USER:
			messages = append(messages, map[string]any{"role": "user", "content": message.GetText()})
		case sessionsv1.Role_ROLE_TOOL:
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": message.GetToolCallId(), "content": message.GetText()})
		case sessionsv1.Role_ROLE_ASSISTANT:
			entry := map[string]any{"role": "assistant", "content": message.GetText()}
			var calls []map[string]any
			for _, call := range message.GetToolCalls() {
				calls = append(calls, map[string]any{"id": call.GetId(), "type": "function", "function": map[string]any{"name": call.GetName(), "arguments": call.GetArguments()}})
			}
			if calls != nil {
				entry["tool_calls"] = calls
			}
			messages = append(messages, entry)
		}
	}
	body := map[string]any{"model": request.Model, "stream": true, "messages": messages}
	if request.MaxTokens > 0 {
		body["max_tokens"] = request.MaxTokens
	}
	if request.Reasoning.Effort != "" {
		body["reasoning_effort"] = request.Reasoning.Effort
	}
	if len(request.Tools) > 0 {
		body["tools"] = request.Tools
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	response, err := c.post(ctx, request, payload)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	stream := io.Reader(response.Body)
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		if stream, err = gzip.NewReader(response.Body); err != nil {
			return nil, c.failure(request, 0, err.Error(), provider.ErrNetwork, err)
		}
	}

	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT}
	reader := bufio.NewReader(stream)
	finish := ""
	for {
		line, err := reader.ReadString('\n')
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if data = strings.TrimSpace(data); ok && data != "[DONE]" {
			var part chunk
			if err := json.Unmarshal([]byte(data), &part); err != nil {
				return nil, err
			}
			if part.Error.Message != "" {
				return nil, c.failure(request, 0, part.Error.Message, provider.ErrServer, nil)
			}
			for _, choice := range part.Choices {
				if choice.FinishReason != "" {
					finish = choice.FinishReason
				}
				text, reasoning := choice.Delta.Content, choice.Delta.Reasoning+choice.Delta.ReasoningContent
				message.Text += text
				message.Reasoning += reasoning
				if err := emit(fn, text, reasoning); err != nil {
					return nil, err
				}
				for _, call := range choice.Delta.ToolCalls {
					if call.Index < 0 || call.Index > len(message.ToolCalls) {
						return nil, fmt.Errorf("tool call %d out of order", call.Index)
					}
					if call.Index == len(message.ToolCalls) {
						message.ToolCalls = append(message.ToolCalls, new(sessionsv1.ToolCall))
					}
					target := message.ToolCalls[call.Index]
					target.Id += call.ID
					target.Name += call.Function.Name
					target.Arguments += call.Function.Arguments
				}
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, c.failure(request, 0, err.Error(), provider.ErrNetwork, err)
		}
		if err != nil || data == "[DONE]" {
			break
		}
	}
	if finish == "" {
		return nil, c.failure(request, 0, "the reply ended before it finished", provider.ErrEmptyResponse, io.ErrUnexpectedEOF)
	}
	return &provider.Response{Message: message, FinishReason: finish}, nil
}

func emit(fn provider.StreamFunc, text, reasoning string) error {
	if reasoning != "" {
		if err := fn(provider.StreamEvent{Kind: provider.EventReasoningDelta, Text: reasoning}); err != nil {
			return err
		}
	}
	if text != "" {
		return fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: text})
	}
	return nil
}

func (c *client) post(ctx context.Context, request *provider.Request, payload []byte) (*http.Response, error) {
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.route.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for name, value := range c.route.Headers {
		call.Header.Set(name, value)
	}
	call.Header.Set("Content-Type", "application/json")
	if c.route.APIKey != "" {
		call.Header.Set("Authorization", "Bearer "+c.route.APIKey)
	}
	response, err := http.DefaultClient.Do(call)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, c.failure(request, 0, err.Error(), provider.ErrNetwork, err)
	}
	if response.StatusCode == http.StatusOK {
		return response, nil
	}
	text, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	response.Body.Close()
	failure := c.failure(request, response.StatusCode, string(bytes.TrimSpace(text)), provider.KindOf(response.StatusCode), nil)
	if seconds, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After"))); err == nil && seconds > 0 {
		failure.RetryAfter = time.Duration(seconds) * time.Second
	}
	return nil, failure
}

func (c *client) failure(request *provider.Request, status int, message string, kind provider.ErrorKind, err error) *provider.APIError {
	return &provider.APIError{Provider: c.route.Provider, Model: request.Model, StatusCode: status, Message: message, Kind: kind, Err: err}
}
