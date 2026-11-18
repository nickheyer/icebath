package tui

import (
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Session switcher over a canonical gateway listing one stored session, the mutate answer given by the case
func canonicalSwitcher(t *testing.T, resolve func(string) string, viewing string, mutate func(map[string]any) map[string]any) (*app, *scriptedGateway, *cards.List) {
	t.Helper()
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.list": func(params map[string]any) map[string]any {
			if params["scope"] == "live" {
				return map[string]any{"sessions": []any{}}
			}
			return map[string]any{"sessions": []any{map[string]any{"id": "old", "title": "Retained old conversation", "message_count": float64(3)}}}
		},
		"session.resume": func(params map[string]any) map[string]any {
			sid, _ := params["session_id"].(string)
			resolved := resolve(sid)
			return map[string]any{"session_id": resolved, "subscription_id": "sub-" + resolved, "revision": float64(4), "execution_generation": float64(2)}
		},
		"session.mutate": mutate,
		"session.detach": fixed(map[string]any{"detached": true}),
	})
	a.gw.locked(func() { a.gw.isCanonical = true })
	a.store.ui.Sid = viewing
	drain(a, a.openSessionSwitcher())
	list, isList := a.wm.Overlay().(*cards.List)
	if !isList {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	return a, g, list
}

// Deletes the stored row of the switcher through its confirmation
func deleteStoredRow(a *app) {
	press(a, "d")
	press(a, "y")
}

// Answer of a session.mutate the owner refused with the reason
func refusedMutate(reason string) func(map[string]any) map[string]any {
	return func(map[string]any) map[string]any {
		return map[string]any{"rpc_error": map[string]any{"code": 4001, "message": reason, "data": map[string]any{"reason": reason}}}
	}
}

func sameSession(sid string) string { return sid }

// canonicalPickerDelete.test.tsx deletes a stored session through session.mutate with the resume CAS tuple
func TestCanonicalDeleteIsTheFencedMutateNotSessionDelete(t *testing.T) {
	a, g, _ := canonicalSwitcher(t, sameSession, "live", func(params map[string]any) map[string]any {
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
	if len(g.sent("session.delete")) != 0 || len(g.sent("session.detach")) != 0 {
		t.Fatal("a committed canonical delete sent session.delete or a detach")
	}
	if len(g.sent("session.list")) < 4 {
		t.Fatalf("the switcher did not read the sessions again %v", g.sent("session.list"))
	}
}

// canonicalPickerDelete.test.tsx keeps a refused delete on screen past the live-status poll
func TestARefusedCanonicalDeletePrintsTheRefusal(t *testing.T) {
	a, _, list := canonicalSwitcher(t, sameSession, "live", refusedMutate("session_busy"))
	deleteStoredRow(a)
	wantScreen(t, a, "error: session_busy")
	if view := list.View(100, 12); !strings.Contains(view, "Retained old conversation") {
		t.Fatalf("the refused row left the switcher:\n%s", view)
	}
}

// canonicalPickerDelete.test.tsx detaches the viewer its CAS read attached when the delete is refused
func TestARefusedCanonicalDeleteDetachesTheReadsViewer(t *testing.T) {
	a, g, _ := canonicalSwitcher(t, sameSession, "live", refusedMutate("revision_conflict"))
	deleteStoredRow(a)
	detaches := g.sent("session.detach")
	if len(detaches) != 1 || detaches[0]["session_id"] != "old" || detaches[0]["subscription_id"] != "sub-old" || len(detaches[0]) != 2 {
		t.Fatalf("detaches %v", detaches)
	}
}

// canonicalPickerDelete.test.tsx detaches by the id the read resolved to, not the history row id
func TestARefusedDeleteDetachesByTheResolvedID(t *testing.T) {
	a, g, _ := canonicalSwitcher(t, func(sid string) string {
		if sid == "old" {
			return "old-owner"
		}
		return sid
	}, "live", refusedMutate("revision_conflict"))
	deleteStoredRow(a)
	detaches := g.sent("session.detach")
	if len(detaches) != 1 || detaches[0]["session_id"] != "old-owner" || detaches[0]["subscription_id"] != "sub-old-owner" {
		t.Fatalf("detaches %v", detaches)
	}
}

// canonicalPickerDelete.test.tsx keeps the view of the session this picker watches when the read resolves to it
func TestARefusedDeleteKeepsTheViewThisSwitcherWatches(t *testing.T) {
	a, g, _ := canonicalSwitcher(t, func(sid string) string {
		if sid == "old" {
			return "live"
		}
		return sid
	}, "live", refusedMutate("revision_conflict"))
	deleteStoredRow(a)
	if len(g.sent("session.mutate")) != 1 || len(g.sent("session.detach")) != 0 {
		t.Fatalf("mutations %v detaches %v", g.sent("session.mutate"), g.sent("session.detach"))
	}
}

func TestLegacyDeleteNeedsTheDeletedIDBack(t *testing.T) {
	a := testApp(t)
	var mu sync.Mutex
	deletedID := "other"
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"session.delete": func(map[string]any) map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return map[string]any{"deleted": deletedID}
		},
	})
	done := 0
	drain(a, a.deleteStoredSession("old", func() tea.Cmd { done++; return nil }))
	if done != 0 {
		t.Fatal("a reply naming another session counted as the delete")
	}
	wantScreen(t, a, "session.delete")
	mu.Lock()
	deletedID = "old"
	mu.Unlock()
	drain(a, a.deleteStoredSession("old", func() tea.Cmd { done++; return nil }))
	if done != 1 || len(g.sent("session.delete")) != 2 || len(g.sent("session.mutate")) != 0 {
		t.Fatalf("done %d deletes %v", done, g.sent("session.delete"))
	}
}

func TestTheCanonicalSwitcherListsIdleLiveAndStoredSessions(t *testing.T) {
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
	a.gw.locked(func() { a.gw.isCanonical = true })
	a.store.ui.Sid = "live"
	drain(a, a.openSessionSwitcher())
	list, isList := a.wm.Overlay().(*cards.List)
	if !isList {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := list.View(110, 12)
	for _, want := range []string{"The conversation in view", "Idle second window", "Retained old conversation"} {
		if !strings.Contains(view, want) {
			t.Fatalf("switcher lacks %q:\n%s", want, view)
		}
	}
}
