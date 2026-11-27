package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The answer of a stateless call holds no JSON-RPC response this client recognizes
var ErrUnrecognized = errors.New("The server answered in a form of MCP that icebath does not know.")

// Network failure of a stateless call, before any answer came
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return "request failed: " + e.Err.Error() }

func (e *RequestError) Unwrap() error { return e.Err }

// Calls one tool of a public endpoint that needs no session, in one POST: the answer as JSON or as events. A failure is a RequestError,
// an HTTPStatusError holding the head of the body, an RPCError or ErrUnrecognized
func CallStateless(ctx context.Context, endpoint, tool string, arguments map[string]any) (*CallToolResult, error) {
	client, err := NewClient(HTTPOptions{URL: endpoint})
	if err != nil {
		return nil, &RequestError{Err: err}
	}
	msg, err := NewRequest(MethodToolsCall, map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &RequestError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", ClientName)
	resp, err := client.Do(req)
	if err != nil {
		return nil, &RequestError{Err: err}
	}
	defer resp.Body.Close()
	raw, err := readCapped(resp.Body, MaxBodyBytes, endpoint)
	if err != nil {
		return nil, &RequestError{Err: err}
	}
	text := strings.ToValidUTF8(string(raw), "�")
	if resp.StatusCode >= 400 {
		runes := []rune(text)
		return nil, &HTTPStatusError{Status: resp.StatusCode, Method: http.MethodPost, URL: endpoint, Body: string(runes[:min(len(runes), 300)])}
	}
	return statelessAnswer(text)
}

// First JSON-RPC response of a body that is JSON or a stream of events
func statelessAnswer(body string) (*CallToolResult, error) {
	var candidates []string
	if trimmed := strings.TrimSpace(body); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		candidates = append(candidates, trimmed)
	}
	reader := NewSSEReader(strings.NewReader(body), MaxBodyBytes, "stateless call")
	for {
		event, err := reader.Next()
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
		if event.Event == "" || event.Event == "message" {
			candidates = append(candidates, event.Data)
		}
	}
	// A result without text is kept while a later one may have some
	var found *CallToolResult
	for _, candidate := range candidates {
		msgs, err := DecodeMessages([]byte(candidate))
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			if msg.Error != nil {
				return nil, msg.Error
			}
			if msg.Result == nil {
				continue
			}
			// A result of another shape holds no content
			var result CallToolResult
			json.Unmarshal(msg.Result, &result)
			if result.IsError || result.Text() != "" {
				return &result, nil
			}
			found = &result
		}
	}
	if found != nil {
		return found, nil
	}
	return nil, ErrUnrecognized
}
