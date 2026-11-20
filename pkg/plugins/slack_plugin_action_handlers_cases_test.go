package plugins

import (
	"context"
	"regexp"
	"testing"
)

// Fresh manager and a context of a plugin bound to it
func slackActionCtx(t *testing.T) (*Manager, *Context) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	m := NewManager(t.TempDir())
	return m, NewContext(&Manifest{Name: "test_plugin", Version: "0.1.0", Description: "test"}, m)
}

func slackActionCallback(context.Context, func(map[string]any) error, map[string]any, map[string]any) error {
	return nil
}

func TestRegisterSlackActionHandlerAPIStringActionIdIsQueued(t *testing.T) {
	m, c := slackActionCtx(t)
	if _, err := c.RegisterSlackActionHandler("inbox_sweep_approve", slackActionCallback); err != nil {
		t.Fatal(err)
	}
	handlers := m.GetSlackActionHandlers()
	if len(handlers) != 1 || handlers[0].ActionID != "inbox_sweep_approve" || handlers[0].Plugin != "test_plugin" {
		t.Fatalf("handlers %+v", handlers)
	}
	if _, ok := handlers[0].Callback.(func(context.Context, func(map[string]any) error, map[string]any, map[string]any) error); !ok {
		t.Fatalf("callback %T", handlers[0].Callback)
	}
}

func TestRegisterSlackActionHandlerAPIRegexActionIdIsAccepted(t *testing.T) {
	m, c := slackActionCtx(t)
	pat := regexp.MustCompile(`^inbox_sweep_.*$`)
	if _, err := c.RegisterSlackActionHandler(pat, slackActionCallback); err != nil {
		t.Fatal(err)
	}
	if got := m.GetSlackActionHandlers()[0].ActionID; got != pat {
		t.Fatalf("action id %v", got)
	}
}

func TestRegisterSlackActionHandlerAPIConstraintDictActionIdIsAccepted(t *testing.T) {
	m, c := slackActionCtx(t)
	constraint := map[string]any{"action_id": "approve", "block_id": "row_3"}
	if _, err := c.RegisterSlackActionHandler(constraint, slackActionCallback); err != nil {
		t.Fatal(err)
	}
	got, ok := m.GetSlackActionHandlers()[0].ActionID.(map[string]any)
	if !ok || len(got) != 2 || got["action_id"] != "approve" || got["block_id"] != "row_3" {
		t.Fatalf("action id %v", m.GetSlackActionHandlers()[0].ActionID)
	}
}
