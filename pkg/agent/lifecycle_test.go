package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

const lifecycleSafeChildEnv = "ICEBATH_AGENT_TEST_LIFECYCLE_SAFE_CHILD"

// Swaps the lifecycle observer and plugin seams for recorders and restores them after the test
func recordLifecycleSeams(t *testing.T, calls *[]string, pluginResult []any) {
	t.Helper()
	prevObs, prevInv, prevHandles, prevHas := lifecycleObserveLifecycle, lifecyclePluginInvokeHook, lifecycleHandlesHook, lifecyclePluginHasHook
	prevFin, prevKey := lifecycleFinalizeConversation, lifecycleRelayProfileKey
	t.Cleanup(func() {
		lifecycleObserveLifecycle, lifecyclePluginInvokeHook, lifecycleHandlesHook, lifecyclePluginHasHook = prevObs, prevInv, prevHandles, prevHas
		lifecycleFinalizeConversation, lifecycleRelayProfileKey = prevFin, prevKey
	})
	lifecycleObserveLifecycle = func(ctx context.Context, env *tools.Env, name string, kwargs map[string]any) {
		*calls = append(*calls, "builtin:"+name)
	}
	lifecyclePluginInvokeHook = func(ctx context.Context, name string, kwargs map[string]any) []any {
		*calls = append(*calls, "plugin:"+name)
		return pluginResult
	}
}

func TestInvokeHookNotifiesBuiltinObserversBeforePlugins(t *testing.T) {
	var calls []string
	recordLifecycleSeams(t, &calls, []any{"ok"})
	result := lifecycleInvokeHook(context.Background(), "on_session_start", map[string]any{"session_id": "session-1"})
	if !reflect.DeepEqual(result, []any{"ok"}) || !reflect.DeepEqual(calls, []string{"builtin:on_session_start", "plugin:on_session_start"}) {
		t.Fatalf("result %v calls %v", result, calls)
	}
}

func TestFinalizeSessionClosesCoreBeforePluginExport(t *testing.T) {
	var calls []string
	recordLifecycleSeams(t, &calls, []any{})
	var core []string
	lifecycleRelayProfileKey = func(ctx context.Context) string { return "profile-1" }
	lifecycleFinalizeConversation = func(ctx context.Context, profileKey, sessionID string) {
		calls = append(calls, "core")
		core = []string{profileKey, sessionID}
	}
	FinalizeSession(context.Background(), map[string]any{"session_id": "session-1", "platform": "cli"})
	if !reflect.DeepEqual(calls, []string{"builtin:on_session_finalize", "core", "plugin:on_session_finalize"}) {
		t.Fatalf("calls %v", calls)
	}
	if !reflect.DeepEqual(core, []string{"profile-1", "session-1"}) {
		t.Fatalf("core %v", core)
	}
	calls = nil
	lifecycleFinalizeConversation = func(ctx context.Context, profileKey, sessionID string) { panic("relay down") }
	FinalizeSession(context.Background(), map[string]any{"session_id": "session-2"})
	if !reflect.DeepEqual(calls, []string{"builtin:on_session_finalize", "plugin:on_session_finalize"}) {
		t.Fatalf("a failed core close must still export to plugins: %v", calls)
	}
	calls = nil
	FinalizeSession(context.Background(), map[string]any{"session_id": ""})
	if len(calls) != 2 {
		t.Fatalf("an empty session id skips only the core close: %v", calls)
	}
}

func TestSessionEndMessagesAcceptsStrAndMessageDictOnly(t *testing.T) {
	got := SessionEndMessages([]any{"  hi  ", map[string]any{"message": "there"}, map[string]any{"context": "x"}, map[string]any{"message": 3}, "", nil, 7})
	if !reflect.DeepEqual(got, []string{"hi", "there"}) {
		t.Fatalf("got %v", got)
	}
	if got := SessionEndMessages(nil); len(got) != 0 {
		t.Fatalf("nil results gave %v", got)
	}
}

func TestHasHookAsksObserversThenPlugins(t *testing.T) {
	var calls []string
	recordLifecycleSeams(t, &calls, nil)
	lifecycleHandlesHook = func(ctx context.Context, name string) bool { return name == "pre_llm_call" }
	lifecyclePluginHasHook = func(ctx context.Context, name string) bool { return name == "custom" }
	ctx := context.Background()
	if !lifecycleHasHook(ctx, "pre_llm_call") || !lifecycleHasHook(ctx, "custom") || lifecycleHasHook(ctx, "other") {
		t.Fatal("has hook must consult the observers then the plugins")
	}
	lifecycleHandlesHook = func(ctx context.Context, name string) bool { panic("broken observer") }
	if !lifecycleHasHook(ctx, "custom") || lifecycleHasHook(ctx, "other") {
		t.Fatal("a failed observer inspection falls through to the plugins")
	}
	lifecycleObserveLifecycle = func(ctx context.Context, env *tools.Env, name string, kwargs map[string]any) { panic("boom") }
	if got := lifecycleInvokeHook(ctx, "x", nil); got != nil {
		t.Fatalf("a failing observer must not stop the plugin dispatch: %v", got)
	}
	if !reflect.DeepEqual(calls, []string{"plugin:x"}) {
		t.Fatalf("calls %v", calls)
	}
}

// Child mode binding the safe worker policy the env names, then printing the lifecycle events
func dispatchLifecycleChild() {
	mode := os.Getenv(lifecycleSafeChildEnv)
	if mode == "" {
		return
	}
	if mode != "ordinary" {
		if _, err := config.BindSafeWorkerPolicy(mode == "safe", true, map[string]any{}); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
	}
	var events []string
	lifecycleObserveLifecycle = func(ctx context.Context, env *tools.Env, name string, kwargs map[string]any) {
		events = append(events, "observer")
	}
	lifecyclePluginInvokeHook = func(ctx context.Context, name string, kwargs map[string]any) []any {
		events = append(events, "plugin-hook")
		return []any{}
	}
	lifecyclePluginAInvokeHook = lifecyclePluginInvokeHook
	lifecycleHandlesHook = func(ctx context.Context, name string) bool { return true }
	ctx := context.Background()
	lifecycleInvokeHook(ctx, "on_session_start", nil)
	lifecycleAInvokeHook(ctx, "on_session_start", nil)
	out, _ := json.Marshal(map[string]any{"events": events, "has_hook": lifecycleHasHook(ctx, "on_session_start")})
	fmt.Println(string(out))
	os.Exit(0)
}

func TestSafeWorkerNeverInvokesLifecycleCustomizations(t *testing.T) {
	for _, mode := range []string{"safe", "config", "ordinary"} {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), lifecycleSafeChildEnv+"="+mode)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s child: %v %s", mode, err, out)
		}
		var got struct {
			Events  []string `json:"events"`
			HasHook bool     `json:"has_hook"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s child output %q: %v", mode, out, err)
		}
		if mode == "safe" {
			if !reflect.DeepEqual(got.Events, []string{"plugin-hook"}) || got.HasHook {
				t.Fatalf("safe worker reached customizations: %+v", got)
			}
			continue
		}
		if !reflect.DeepEqual(got.Events, []string{"observer", "plugin-hook", "observer", "plugin-hook"}) || !got.HasHook {
			t.Fatalf("%s worker skipped customizations: %+v", mode, got)
		}
	}
}
