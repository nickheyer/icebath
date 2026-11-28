package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Answers a server asking the model to write: on the helper model of the session whose call is running, else on the profile's own, within
// the server's rate limit and model allowlist
type sampler struct {
	server *server
	config samplingConfig

	mu      sync.Mutex
	window  []time.Time
	metrics map[string]int
}

// Sampling without tools: sampling with them needs the helper models to hand back tool calls
func (s *sampler) Capabilities() map[string]any { return map[string]any{} }

// Requests answered, errors and tool uses refused, for the server's status
func (s *sampler) Metrics() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.metrics)
}

func (s *sampler) fail(message string) *mcp.RPCError {
	s.mu.Lock()
	s.metrics["errors"]++
	s.mu.Unlock()
	return &mcp.RPCError{Code: -1, Message: message}
}

// Whether the request fits in the last minute's budget, counting it when it does
func (s *sampler) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	s.window = slices.DeleteFunc(s.window, func(t time.Time) bool { return !t.After(cut) })
	if len(s.window) >= s.config.MaxRPM {
		return false
	}
	s.window = append(s.window, time.Now())
	return true
}

func (s *sampler) CreateMessage(ctx context.Context, params *mcp.CreateMessageParams) (any, *mcp.RPCError) {
	name := s.server.name
	if !s.admit() {
		slog.Warn("MCP server sampling rate limit exceeded", "server", name, "per_minute", s.config.MaxRPM)
		return nil, s.fail(fmt.Sprintf("Sampling rate limit exceeded for server '%s' (%d requests/minute)", name, s.config.MaxRPM))
	}
	model := s.config.Model
	if model == "" && params.ModelPreferences != nil {
		for _, hint := range params.ModelPreferences.Hints {
			if hint.Name != "" {
				model = hint.Name
				break
			}
		}
	}
	if model != "" && len(s.config.Allowed) > 0 && !slices.Contains(s.config.Allowed, model) {
		slog.Warn("MCP server asked for a model not in its allowed_models", "server", name, "model", model)
		return nil, s.fail(fmt.Sprintf("Model '%s' not allowed for server '%s'. Allowed: %s", model, name, strings.Join(s.config.Allowed, ", ")))
	}
	if len(params.Tools) > 0 {
		s.mu.Lock()
		s.metrics["tool_use_count"]++
		s.mu.Unlock()
		return nil, s.fail(fmt.Sprintf("Sampling with tools is not supported for server '%s': this client does not declare the sampling tools capability", name))
	}
	var messages []*sessionsv1.Message
	if params.SystemPrompt != "" {
		messages = append(messages, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_SYSTEM, Text: params.SystemPrompt})
	}
	for _, msg := range params.Messages {
		converted, err := sampled(msg)
		if err != nil {
			return nil, &mcp.RPCError{Code: mcp.CodeInvalidParams, Message: err.Error()}
		}
		messages = append(messages, converted)
	}
	maxTokens := min(params.MaxTokens, s.config.MaxTokens)
	slog.Log(context.Background(), s.config.LogLevel, "MCP server sampling request", "server", name, "model", model, "max_tokens", maxTokens, "messages", len(messages))
	callCtx, base := s.server.callContext(ctx)
	callCtx, cancel := context.WithTimeout(callCtx, s.config.Timeout)
	defer cancel()
	client := s.server.helpers(base)
	request := auxiliary.Request{Task: mcpconfig.Task, Messages: messages, Model: model, MaxTokens: maxTokens, Temperature: params.Temperature,
		Timeout: s.config.Timeout}
	// A model picked with its provider runs there, whatever provider the helper task of MCP servers uses
	if s.config.Provider != "" && model == s.config.Model {
		request.Model, request.Entry = "", providerconfig.Pair(s.config.Provider, model)
	}
	text, err := client.Complete(callCtx, request)
	switch {
	case errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return nil, s.fail(fmt.Sprintf("Sampling LLM call timed out after %ss for server '%s'", strings.TrimSuffix(fmt.Sprintf("%g", s.config.Timeout.Seconds()), ".0"), name))
	case err != nil:
		return nil, s.fail("Sampling LLM call failed: " + mcp.Redact(err.Error()))
	}
	s.mu.Lock()
	s.metrics["requests"]++
	s.mu.Unlock()
	slog.Log(context.Background(), s.config.LogLevel, "MCP server sampling response", "server", name, "model", model)
	return map[string]any{"role": "assistant", "model": model, "content": map[string]any{"type": "text", "text": mcp.Redact(text)}, "stopReason": "endTurn"}, nil
}

// One sampling message as a message of the helper model: its text blocks joined, its images attached, its tool exchanges as text
func sampled(msg mcp.SamplingMessage) (*sessionsv1.Message, error) {
	role := sessionsv1.Role_ROLE_USER
	if msg.Role == "assistant" {
		role = sessionsv1.Role_ROLE_ASSISTANT
	}
	var blocks []map[string]any
	if json.Unmarshal(msg.Content, &blocks) != nil {
		var one map[string]any
		if err := json.Unmarshal(msg.Content, &one); err != nil {
			return nil, fmt.Errorf("sampling message content is neither a content block nor a list of them")
		}
		blocks = []map[string]any{one}
	}
	out := &sessionsv1.Message{Role: role}
	var texts []string
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			texts = append(texts, fmt.Sprint(block["text"]))
		case "image":
			data, err := base64.StdEncoding.DecodeString(fmt.Sprint(block["data"]))
			if err != nil {
				return nil, fmt.Errorf("sampling image is not base64: %w", err)
			}
			out.Media = append(out.Media, &sessionsv1.Media{MimeType: fmt.Sprint(block["mimeType"]), Data: data})
		case "audio":
			texts = append(texts, fmt.Sprintf("[audio %v omitted]", block["mimeType"]))
		case "tool_use":
			input, _ := json.Marshal(block["input"])
			texts = append(texts, fmt.Sprintf("[called tool %v with %s]", block["name"], input))
		case "tool_result":
			content, _ := json.Marshal(block["content"])
			texts = append(texts, fmt.Sprintf("[result of tool call %v: %s]", block["toolUseId"], content))
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out, nil
}

// Context of the call running on the server and its environment, else the request's own and none
func (s *server) callContext(ctx context.Context) (context.Context, *tools.Env) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingCtx != nil {
		return context.WithoutCancel(s.pendingCtx), s.pending
	}
	return ctx, nil
}

// Helper models of the calling session, else of the profile routed by its own settings
func (s *server) helpers(env *tools.Env) *auxiliary.Client {
	if env != nil && env.Aux != nil {
		return env.Aux
	}
	m := s.pool.m
	return &auxiliary.Client{Tasks: m.helpers.Tasks, Config: s.pool.cfg, Providers: m.helpers.Providers, Transports: m.helpers.Transports,
		Credentials: m.helpers.Credentials, State: m.learned}
}
