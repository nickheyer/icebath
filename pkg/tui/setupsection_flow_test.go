package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Gateway playing a scripted setup flow, one step result per call after start, recording every answer
type flowGateway struct {
	steps   []map[string]any
	answers []map[string]any
	started map[string]any
}

func (g *flowGateway) canonical() bool { return false }

func (g *flowGateway) request(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	switch method {
	case "setup.flow.start":
		g.started = params
		return map[string]any{"session": "flow-1"}, nil
	case "setup.flow.step":
		if answer, given := params["answer"].(map[string]any); given {
			g.answers = append(g.answers, answer)
		}
		if len(g.steps) == 0 {
			return map[string]any{"lines": []any{}, "question": nil, "done": true, "ok": true, "error": ""}, nil
		}
		next := g.steps[0]
		g.steps = g.steps[1:]
		return next, nil
	}
	return map[string]any{}, nil
}

// Step result holding lines and an open question
func flowStep(lines []any, question map[string]any) map[string]any {
	return map[string]any{"lines": lines, "question": question, "done": false, "ok": false, "error": ""}
}

// Presses a key on the open overlay and runs what it returns
func pressOverlay(t *testing.T, a *app, key string) {
	t.Helper()
	card := a.wm.Overlay()
	if card == nil {
		t.Fatalf("no overlay for %s", key)
	}
	_, cmd := a.Update(keys.Press(key))
	drain(a, cmd)
}

func TestSetupFlowRendersEachQuestionAndSendsTheAnswers(t *testing.T) {
	a := testApp(t)
	g := &flowGateway{steps: []map[string]any{
		flowStep([]any{"󰆧 Icebath Tool Configuration"}, map[string]any{"id": 1, "kind": "choose", "question": "Select an option:", "default": 0,
			"options": []any{map[string]any{"title": "Configure CLI", "detail": "19/30"}, map[string]any{"title": "Done", "detail": ""}}}),
		flowStep([]any{}, map[string]any{"id": 2, "kind": "check", "question": "Tools for CLI", "selected": []any{0},
			"items":  []any{map[string]any{"title": "Web", "detail": "search", "tools": []any{"web_search"}}, map[string]any{"title": "Vision", "detail": "", "tools": []any{"vision"}}},
			"tokens": map[string]any{"web_search": 600, "vision": 700}}),
		flowStep([]any{"  + Vision"}, map[string]any{"id": 3, "kind": "text", "question": "OPENROUTER_API_KEY", "default_text": "", "secret": true}),
	}}
	a.events.gw = g
	drain(a, a.runSetupFlow("tools", "Tools", nil))
	menu, isPalette := a.wm.Overlay().(*wm.Palette)
	if !isPalette || !strings.HasPrefix(menu.Matches()[0].Title, "● Configure CLI") || !strings.Contains(screen(a), "Icebath Tool Configuration") {
		t.Fatalf("menu %+v transcript\n%s", a.wm.Overlay(), screen(a))
	}
	pressOverlay(t, a, "enter")
	check := a.wm.Overlay().(*wm.Palette)
	rows := check.Matches()
	if rows[0].Detail != "Est. tool context ~600 tokens" || !strings.HasPrefix(rows[1].Title, "[x] Web") || !strings.HasPrefix(rows[2].Title, "[ ] Vision") {
		t.Fatalf("checklist %+v", rows)
	}
	pressOverlay(t, a, "down")
	pressOverlay(t, a, "down")
	pressOverlay(t, a, "enter")
	rows = a.wm.Overlay().(*wm.Palette).Matches()
	if !strings.HasPrefix(rows[2].Title, "[x] Vision") || rows[0].Detail != "Est. tool context ~1.3k tokens" {
		t.Fatalf("toggle %+v", rows)
	}
	drain(a, rows[0].Run())
	secret, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || !secret.fields[0].masked {
		t.Fatalf("secret prompt %+v", a.wm.Overlay())
	}
	drain(a, secret.onAnswer(nil, nil, []string{"sk-or-1"}))
	want := []map[string]any{{"index": int64(0), "question": int64(1)}, {"indices": []any{int64(0), int64(1)}, "question": int64(2)}, {"text": "sk-or-1", "question": int64(3)}}
	if !reflect.DeepEqual(g.answers, want) || g.started["flow"] != "tools" {
		t.Fatalf("answers %+v started %+v", g.answers, g.started)
	}
	if !strings.Contains(screen(a), "Tools done") || !strings.Contains(screen(a), "+ Vision") {
		t.Fatalf("transcript\n%s", screen(a))
	}
}

func TestSetupFlowEscapeCancelsTheOpenQuestion(t *testing.T) {
	a := testApp(t)
	g := &flowGateway{steps: []map[string]any{
		flowStep(nil, map[string]any{"id": 4, "kind": "choose", "question": "Select STT model:", "default": 1,
			"options": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}}}),
		{"lines": []any{}, "question": nil, "done": true, "ok": false, "error": "setup cancelled"},
	}}
	a.events.gw = g
	drain(a, a.runSetupFlow("tools", "Tools", nil))
	if rows := a.wm.Overlay().(*wm.Palette).Matches(); !strings.HasPrefix(rows[0].Title, "● b") {
		t.Fatalf("default not first %+v", rows)
	}
	_, cmd := a.Update(keys.Press("esc"))
	drain(a, cmd)
	if !reflect.DeepEqual(g.answers, []map[string]any{{"cancel": true, "question": int64(4)}}) || !strings.Contains(screen(a), "Tools stopped · setup cancelled") {
		t.Fatalf("answers %+v transcript\n%s", g.answers, screen(a))
	}
}
