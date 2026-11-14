package slack

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Fresh plugin manager and a context bound to it
func actionCaseContext(t *testing.T) (*plugins.Manager, *plugins.Context) {
	t.Helper()
	mgr := plugins.NewManager(t.TempDir())
	return mgr, plugins.NewContext(&plugins.Manifest{Name: "test_plugin", Version: "0.1.0", Description: "test"}, mgr)
}

// Plugin action callback the cases register, never called
func actionCaseCallback(_ context.Context, ack func(map[string]any) error, _, _ map[string]any) error {
	return ack(nil)
}

// test_slack_plugin_action_handlers.py TestRegisterSlackActionHandlerAPI, one row per case
func TestRegisterSlackActionHandlerAPI(t *testing.T) {
	pattern := regexp.MustCompile(`^inbox_sweep_.*$`)
	cases := []struct {
		name     string
		actionID any
	}{
		{"test_string_action_id_is_queued", "inbox_sweep_approve"},
		{"test_regex_action_id_is_accepted", pattern},
		{"test_constraint_dict_action_id_is_accepted", map[string]any{"action_id": "approve", "block_id": "row_3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, ctx := actionCaseContext(t)
			if _, err := ctx.RegisterSlackActionHandler(tc.actionID, actionCaseCallback); err != nil {
				t.Fatal(err)
			}
			handlers := mgr.GetSlackActionHandlers()
			if len(handlers) != 1 {
				t.Fatalf("handlers = %d", len(handlers))
			}
			h := handlers[0]
			if !reflect.DeepEqual(h.ActionID, tc.actionID) || h.Plugin != "test_plugin" {
				t.Fatalf("handler = %+v", h)
			}
			if reflect.ValueOf(h.Callback).Pointer() != reflect.ValueOf(actionCaseCallback).Pointer() {
				t.Fatal("callback is not the registered function")
			}
			if re, ok := tc.actionID.(*regexp.Regexp); ok && h.ActionID.(*regexp.Regexp) != re {
				t.Fatal("pattern is not the registered object")
			}
		})
	}
}

// Action ids the connected adapter's app routes
func registeredActionIDs(a *SlackAdapter) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for _, r := range a.app.actions {
		ids = append(ids, r.matcher.id)
	}
	return ids
}

// Connects through the loopback Slack with the plugin manager answering handlers or failing
func connectWithPluginHandlers(t *testing.T, handlers []*plugins.SlackActionHandler, loaderErr error) *SlackAdapter {
	t.Helper()
	prev := slackActionHandlersFn
	slackActionHandlersFn = func(context.Context) ([]*plugins.SlackActionHandler, error) { return handlers, loaderErr }
	t.Cleanup(func() { slackActionHandlersFn = prev })
	a, f := socketCaseAdapter(t)
	connectSocketCase(t, a, f)
	return a
}

// test_slack_plugin_action_handlers.py TestSlackAdapterPluginActionWiring.test_no_plugin_handlers_does_not_break_connect
func TestSlackAdapterPluginActionWiringNoPluginHandlersDoesNotBreakConnect(t *testing.T) {
	a := connectWithPluginHandlers(t, []*plugins.SlackActionHandler{}, nil)
	ids := registeredActionIDs(a)
	for _, id := range ids {
		if id == "hermes_approve_once" {
			return
		}
	}
	t.Fatalf("action ids = %v", ids)
}

// test_slack_plugin_action_handlers.py TestSlackAdapterPluginActionWiring.test_plugin_loader_failure_does_not_break_connect
func TestSlackAdapterPluginActionWiringPluginLoaderFailureDoesNotBreakConnect(t *testing.T) {
	a := connectWithPluginHandlers(t, nil, errors.New("plugins broken"))
	ids := registeredActionIDs(a)
	for _, id := range ids {
		if id == "hermes_approve_once" {
			return
		}
	}
	t.Fatalf("action ids = %v", ids)
}
