package acp

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

type connUpdate struct {
	sessionID string
	update    pyacp.SessionUpdate
}

type captureConn struct {
	mu         sync.Mutex
	updates    []connUpdate
	permission func(options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate) (*pyacp.RequestPermissionResponse, error)
}

func (c *captureConn) SessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate, meta *pyjson.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, connUpdate{sessionID, update})
	return nil
}

func (c *captureConn) RequestPermission(ctx context.Context, options []pyacp.PermissionOption, sessionID string, toolCall *pyacp.ToolCallUpdate, meta *pyjson.Object) (*pyacp.RequestPermissionResponse, error) {
	if c.permission != nil {
		return c.permission(options, sessionID, toolCall)
	}
	return &pyacp.RequestPermissionResponse{Outcome: &pyacp.AllowedOutcome{OptionID: "allow_once"}}, nil
}

func (c *captureConn) seen() []connUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connUpdate(nil), c.updates...)
}

func (c *captureConn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = nil
}

func textPrompt(sessionID, text string) *pyacp.PromptRequest {
	return &pyacp.PromptRequest{SessionID: sessionID, Prompt: []pyacp.ContentBlock{pyacp.TextBlock(text)}}
}
