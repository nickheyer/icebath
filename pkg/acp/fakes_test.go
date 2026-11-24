package acp

import (
	"context"
	"sync"
	"testing"

	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func swap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

type connUpdate struct {
	sessionID string
	update    acputil.SessionUpdate
}

type captureConn struct {
	mu         sync.Mutex
	updates    []connUpdate
	permission func(options []acputil.PermissionOption, sessionID string, toolCall *acputil.ToolCallUpdate) (*acputil.RequestPermissionResponse, error)
}

func (c *captureConn) SessionUpdate(ctx context.Context, sessionID string, update acputil.SessionUpdate, meta *jsonutil.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, connUpdate{sessionID, update})
	return nil
}

func (c *captureConn) RequestPermission(ctx context.Context, options []acputil.PermissionOption, sessionID string, toolCall *acputil.ToolCallUpdate, meta *jsonutil.Object) (*acputil.RequestPermissionResponse, error) {
	if c.permission != nil {
		return c.permission(options, sessionID, toolCall)
	}
	return &acputil.RequestPermissionResponse{Outcome: &acputil.AllowedOutcome{OptionID: "allow_once"}}, nil
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

func textPrompt(sessionID, text string) *acputil.PromptRequest {
	return &acputil.PromptRequest{SessionID: sessionID, Prompt: []acputil.ContentBlock{acputil.TextBlock(text)}}
}
