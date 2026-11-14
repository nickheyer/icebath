package plugins

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

// Registers the compiled-in late_cmd plugin with a command, a telegram handler factory and a declared tool, then enables it
func addLatePlugin() {
	RegisterBundled(ledgerBundled{m: Manifest{Name: "late_cmd", Version: "0.1", Description: "t", ProvidesTools: []string{"late_tool"}},
		register: func(ctx *Context) error {
			ctx.RegisterCommand("late", func(context.Context, string) (any, error) { return "hi", nil }, "late", "", "")
			return ctx.RegisterPlatformHandler("telegram", func(app, adapter any) {})
		}})
}

func TestOnPluginLoadedFiresWithActivationSummaryAfterALateLoad(t *testing.T) {
	ledgerSwapBundled(t)
	h := casesHome(t, "")
	m := casesManager(t, h)
	ctx := homeCtx(h)
	m.DiscoverAndLoad(ctx, false)
	var mu sync.Mutex
	var events [][]map[string]any
	unsubscribe, err := m.OnPluginLoaded(func(summaries []map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, summaries)
	})
	if err != nil {
		t.Fatal(err)
	}
	addLatePlugin()
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [late_cmd]\n")
	m.DiscoverAndLoad(ctx, true)
	mu.Lock()
	fired := len(events)
	var late []map[string]any
	if fired > 0 {
		for _, e := range events[fired-1] {
			if e["name"] == "late_cmd" {
				late = append(late, e)
			}
		}
	}
	mu.Unlock()
	got, _ := json.Marshal(late)
	want := `[{"activated_now":{"callbacks":["telegram"],"gateway_commands":["late"]},"deferred":{"tools":["late_tool"]},"key":"late_cmd","name":"late_cmd"}]`
	if string(got) != want {
		t.Fatalf("late summaries %s (load error %q)", got, loadedError(m, "late_cmd"))
	}
	if fired != 1 {
		t.Fatalf("the sweep that added the plugin fired %d events", fired)
	}
	m.DiscoverAndLoad(ctx, true)
	mu.Lock()
	again := len(events)
	mu.Unlock()
	if again != 1 {
		t.Fatalf("a sweep that added nothing fired, events = %d", again)
	}
	unsubscribe()
}
