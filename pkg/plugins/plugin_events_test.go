package plugins

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Port of the emit half of test_broadcast_reaches_a_registered_client_as_a_namespaced_global_event
func TestBroadcastReachesTheGatewayAsANamespacedGlobalEvent(t *testing.T) {
	var got [][2]any
	installActivationServices(t, &Services{BroadcastGlobalEvent: func(name string, payload map[string]any) {
		got = append(got, [2]any{name, payload})
	}})
	payload := map[string]any{"count": 3}
	if err := BroadcastPluginEvent("rss-reader", "feed.updated", payload); err != nil {
		t.Fatal(err)
	}
	if err := BroadcastPluginEvent("kanban", "changed", nil); err != nil {
		t.Fatal(err)
	}
	want := [][2]any{{"plugin.rss-reader.feed.updated", map[string]any{"count": 3}}, {"plugin.kanban.changed", map[string]any{}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("emitted %#v", got)
	}
	got[0][1].(map[string]any)["count"] = 4
	if payload["count"] != 3 {
		t.Fatal("the emitted payload aliases the caller's dict")
	}
}

// Port of test_broadcast_without_a_gateway_module_is_a_logged_no_op
func TestBroadcastWithoutAGatewayIsALoggedNoOp(t *testing.T) {
	installActivationServices(t, &Services{})
	buf := captureLogs(t)
	if err := BroadcastPluginEvent("rss-reader", "feed.updated", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "plugin.rss-reader.feed.updated") {
		t.Fatalf("log = %s", buf.String())
	}
}

// Port of test_names_that_cannot_form_a_namespaced_event_are_refused_before_emit
func TestNamesThatCannotFormANamespacedEventAreRefusedBeforeEmit(t *testing.T) {
	installActivationServices(t, &Services{BroadcastGlobalEvent: func(string, map[string]any) { t.Fatal("must not emit") }})
	var nameErr *PluginEventNameError
	var payloadErr *PluginEventPayloadError
	for _, c := range []struct {
		id, event string
		payload   any
		typeError bool
	}{
		{"", "items", nil, false}, {"Bad Id", "items", nil, false}, {"has/slash", "items", nil, false},
		{"other.x", "items", nil, false}, {"ok", "", nil, false}, {"ok", "bad name", nil, false},
		{"ok", "../x", nil, false}, {"ok", ".leading", nil, false}, {"ok", "a..b", nil, false},
		{"ok", strings.Repeat("a", 129), nil, false}, {"ok", "items", []any{"not", "a", "dict"}, true},
	} {
		err := BroadcastPluginEvent(c.id, c.event, c.payload)
		if c.typeError && !errors.As(err, &payloadErr) || !c.typeError && !errors.As(err, &nameErr) {
			t.Fatalf("%q %q %v: err = %v", c.id, c.event, c.payload, err)
		}
	}
	if name, err := PluginEventName("ok", "items\n"); err != nil || name != "plugin.ok.items\n" {
		t.Fatalf("re.match $ accepts a trailing newline: %q, %v", name, err)
	}
}

// Port of plugin_session.context_surface and context_cwd for the classic CLI and a gateway session
func TestContextSurfaceAndCwdFollowTheCLIReference(t *testing.T) {
	m := NewManager(t.TempDir())
	installActivationServices(t, &Services{
		ActiveSessionPolicy: func(context.Context) (string, string, bool) { return "telegram", "/work", true },
	})
	ctx := context.Background()
	if s, _ := contextSurface(ctx, m); s != "telegram" {
		t.Fatalf("surface = %q", s)
	}
	if c, _ := contextCwd(ctx, m); c != "/work" {
		t.Fatalf("cwd = %q", c)
	}
	m.cliRef = struct{ CLIHost }{}
	wd, _ := os.Getwd()
	if s, _ := contextSurface(ctx, m); s != "cli" {
		t.Fatalf("cli surface = %q", s)
	}
	if c, err := contextCwd(ctx, m); c != wd || err != nil {
		t.Fatalf("cli cwd = %q, %v", c, err)
	}
}
