package tui

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

// Sessions screen over a canonical gateway listing one stored session, the mutate answer given by the case
func canonicalSessions(t *testing.T, viewing string, mutate func(map[string]any) map[string]any) (*app, *scriptedGateway, *cards.Hub) {
	t.Helper()
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.list": func(params map[string]any) map[string]any {
			if params["scope"] == "live" {
				return map[string]any{"sessions": []any{}}
			}
			return map[string]any{"sessions": []any{map[string]any{"id": "old", "title": "Retained old conversation", "message_count": float64(3)}}}
		},
		"session.revision": func(params map[string]any) map[string]any {
			return map[string]any{"session_id": params["session_id"], "revision": float64(4), "execution_generation": float64(2)}
		},
		"session.mutate": mutate,
	})
	a.store.ui.Sid = viewing
	drain(a, a.openSessions(""))
	h, isHub := a.wm.Overlay().(*cards.Hub)
	if !isHub {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	return a, g, h
}

// Presses a key on the app and runs what it returns
func press(a *app, key string) {
	_, cmd := a.Update(keys.Press(key))
	drain(a, cmd)
}

// Deletes the stored row of the sessions screen through its confirmation
func deleteStoredRow(a *app) {
	press(a, "x")
	press(a, "y")
}

// Answer of a session.mutate the owner refused with the reason
func refusedMutate(reason string) func(map[string]any) map[string]any {
	return func(map[string]any) map[string]any {
		return map[string]any{"rpc_error": map[string]any{"code": 4001, "message": reason, "data": map[string]any{"reason": reason}}}
	}
}

// A stored session is deleted through session.mutate fenced by the revision read without attaching to it
func TestCanonicalDeleteIsTheFencedMutateNotSessionDelete(t *testing.T) {
	a, g, _ := canonicalSessions(t, "live", func(params map[string]any) map[string]any {
		return map[string]any{"session_id": params["session_id"], "operation": "delete", "revision": float64(5), "deleted_ids": []any{params["session_id"]}}
	})
	deleteStoredRow(a)
	mutations := g.sent("session.mutate")
	if len(mutations) != 1 {
		t.Fatalf("mutations %v", mutations)
	}
	m := mutations[0]
	if m["session_id"] != "old" || m["operation"] != "delete" || m["expected_revision"] != float64(4) || m["expected_generation"] != float64(2) || m["request_id"] == "" {
		t.Fatalf("mutation %v", m)
	}
	if payload, _ := m["payload"].(map[string]any); payload == nil || len(payload) != 0 {
		t.Fatalf("payload %v", m["payload"])
	}
	if len(g.sent("session.delete")) != 0 || len(g.sent("session.resume")) != 0 || len(g.sent("session.detach")) != 0 {
		t.Fatal("a canonical delete attached, detached or sent session.delete")
	}
	if len(g.sent("session.list")) < 4 {
		t.Fatalf("the sessions screen did not read the sessions again %v", g.sent("session.list"))
	}
}

// A refused delete says why in the sessions screen and keeps the row
func TestARefusedCanonicalDeleteSaysWhyInTheScreen(t *testing.T) {
	a, _, h := canonicalSessions(t, "live", refusedMutate("session_busy"))
	deleteStoredRow(a)
	view := h.View(100, 12)
	if !strings.Contains(view, "session_busy") || !strings.Contains(view, "Retained old conversation") {
		t.Fatalf("the refusal or the row is missing:\n%s", view)
	}
}

func TestTheCanonicalSessionsScreenListsIdleLiveAndStoredSessions(t *testing.T) {
	a := testApp(t)
	attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.list": func(params map[string]any) map[string]any {
			if params["scope"] == "live" {
				return map[string]any{"sessions": []any{
					map[string]any{"id": "live", "title": "The conversation in view", "running": false},
					map[string]any{"id": "other", "title": "Idle second window", "running": false}}}
			}
			return map[string]any{"sessions": []any{map[string]any{"id": "old", "title": "Retained old conversation", "message_count": float64(3)}}}
		},
	})
	a.store.ui.Sid = "live"
	drain(a, a.openSessions(""))
	h, isHub := a.wm.Overlay().(*cards.Hub)
	if !isHub {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := h.View(110, 12)
	for _, want := range []string{"The conversation in view", "Idle second window", "Retained old conversation"} {
		if !strings.Contains(view, want) {
			t.Fatalf("sessions screen lacks %q:\n%s", want, view)
		}
	}
}
