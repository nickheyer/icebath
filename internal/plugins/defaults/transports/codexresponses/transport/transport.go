package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const carrier = string(provider.ModeCodexResponses)

// Codex response status to finish reason
var codexStopReasonMap = map[string]string{"completed": "stop", "incomplete": "length", "failed": "stop", "cancelled": "stop"}

type registrant struct{}

func Register(registry *transport.Registry) {
	registry.Register(registrant{})
}

func (registrant) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{
		provider.ModeCodexResponses: func(route provider.Route) (provider.Client, error) {
			return &client{route: route}, nil
		},
	}
}

type client struct {
	route provider.Route
}

type event struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Message  string          `json:"message"`
	Item     json.RawMessage `json:"item"`
	Response struct {
		Status string `json:"status"`
		Error  struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

type item struct {
	Type             string `json:"type"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

func (c *client) Route() provider.Route {
	return c.route
}

func (c *client) Stream(ctx context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	instructions, input := "", []any{}
	for _, message := range request.Messages {
		switch message.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			if instructions == "" && len(input) == 0 {
				instructions = strings.TrimSpace(message.GetText())
				continue
			}
			input = append(input, map[string]any{"role": "system", "content": message.GetText()})
		case sessionsv1.Role_ROLE_USER:
			input = append(input, map[string]any{"role": "user", "content": []map[string]any{{"type": "input_text", "text": message.GetText()}}})
		case sessionsv1.Role_ROLE_TOOL:
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.GetToolCallId(), "output": message.GetText()})
		case sessionsv1.Role_ROLE_ASSISTANT:
			var reasoning []json.RawMessage
			if raw := message.GetCarriers()[carrier]; len(raw) > 0 && json.Unmarshal(raw, &reasoning) == nil {
				for _, entry := range reasoning {
					input = append(input, entry)
				}
			}
			if text := message.GetText(); text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text}}})
			}
			for _, call := range message.GetToolCalls() {
				input = append(input, map[string]any{"type": "function_call", "call_id": call.GetId(), "name": call.GetName(), "arguments": call.GetArguments()})
			}
		}
	}
	body := map[string]any{"model": request.Model, "instructions": instructions, "input": input, "store": false, "stream": true}
	if tools := responsesTools(request.Tools); len(tools) > 0 {
		body["tools"], body["tool_choice"], body["parallel_tool_calls"] = tools, "auto", true
	}
	headers := map[string]string{}
	switch {
	case request.SessionID == "":
	case isXAI(c.route.BaseURL):
		headers["x-grok-conv-id"] = request.SessionID
	default:
		body["prompt_cache_key"] = request.SessionID
	}
	maps.Copy(body, reasoningFields(request.Model, c.route.BaseURL, request.Reasoning.Effort))
	if request.MaxTokens > 0 {
		body["max_output_tokens"] = request.MaxTokens
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	response, err := c.post(ctx, request, payload, headers)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	message := &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT}
	var reasoning []json.RawMessage
	reader := bufio.NewReader(response.Body)
	finish := ""
	for finish == "" {
		line, err := reader.ReadString('\n')
		if data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok && strings.TrimSpace(data) != "[DONE]" {
			var part event
			if err := json.Unmarshal([]byte(data), &part); err != nil {
				return nil, err
			}
			switch part.Type {
			case "response.output_text.delta", "response.text.delta", "response.refusal.delta":
				message.Text += part.Delta
				if err := fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: part.Delta}); err != nil {
					return nil, err
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				message.Reasoning += part.Delta
				if err := fn(provider.StreamEvent{Kind: provider.EventReasoningDelta, Text: part.Delta}); err != nil {
					return nil, err
				}
			case "response.output_item.done":
				var done item
				if err := json.Unmarshal(part.Item, &done); err != nil {
					return nil, err
				}
				switch done.Type {
				case "function_call":
					message.ToolCalls = append(message.ToolCalls, &sessionsv1.ToolCall{Id: done.CallID, Name: done.Name, Arguments: done.Arguments})
				case "reasoning":
					if done.EncryptedContent != "" {
						reasoning = append(reasoning, part.Item)
					}
				case "message":
					if message.Text == "" {
						for _, content := range done.Content {
							message.Text += content.Text + content.Refusal
						}
					}
				}
			case "response.completed", "response.incomplete":
				finish = codexStopReasonMap[part.Response.Status]
				if finish == "" {
					finish = "stop"
				}
			case "response.failed":
				return nil, c.failure(request, 0, part.Response.Error.Message, provider.ErrServer, nil)
			case "error":
				return nil, c.failure(request, 0, part.Message, provider.ErrServer, nil)
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, c.failure(request, 0, err.Error(), provider.ErrNetwork, err)
		}
		if err != nil {
			break
		}
	}
	if finish == "" {
		return nil, c.failure(request, 0, "the reply ended before it finished", provider.ErrEmptyResponse, io.ErrUnexpectedEOF)
	}
	if len(reasoning) > 0 {
		raw, err := json.Marshal(reasoning)
		if err != nil {
			return nil, err
		}
		message.Carriers = map[string][]byte{carrier: raw}
	}
	return &provider.Response{Message: message, FinishReason: finish}, nil
}

func responsesTools(defs []map[string]any) []map[string]any {
	var converted []map[string]any
	for _, def := range defs {
		function, _ := def["function"].(map[string]any)
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		strict, _ := function["strict"].(bool)
		parameters, has := function["parameters"]
		if !has {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		description, _ := function["description"].(string)
		converted = append(converted, map[string]any{"type": "function", "name": name, "description": description, "strict": strict, "parameters": parameters})
	}
	return converted
}

func (c *client) post(ctx context.Context, request *provider.Request, payload []byte, headers map[string]string) (*http.Response, error) {
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.route.BaseURL, "/")+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for name, value := range c.route.Headers {
		call.Header.Set(name, value)
	}
	for name, value := range headers {
		call.Header.Set(name, value)
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set("Accept", "text/event-stream")
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
