package config

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// savedCall is one config saved observer delivery
type savedCall struct {
	h        string
	old, new map[string]any
}

// Installs a recording saved observer and restores the previous one afterwards
func recordSavedCalls(t *testing.T) *[]savedCall {
	t.Helper()
	var calls []savedCall
	facadeMu.Lock()
	prev := savedObserver
	facadeMu.Unlock()
	RegisterConfigSavedObserver(func(h string, oldRaw, newConfig map[string]any) {
		calls = append(calls, savedCall{h, oldRaw, newConfig})
	})
	t.Cleanup(func() { RegisterConfigSavedObserver(prev) })
	return &calls
}

func TestSavedObserverSeesUserWritesWithTheOwningHomeAndSkipsAppliedWrites(t *testing.T) {
	captureFacade(t)
	h := t.TempDir()
	t.Setenv(home.EnvHome, t.TempDir())
	calls := recordSavedCalls(t)
	ctx := context.Background()
	if err := SaveConfig(AppliedWrite(ctx), h, map[string]any{"memory": map[string]any{"memory_enabled": false}}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("applied save observed %v", *calls)
	}
	if err := SaveConfig(ctx, h, map[string]any{"memory": map[string]any{"memory_enabled": true}}, SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].h != h {
		t.Fatalf("user save observed %v", *calls)
	}
	if mem, _ := CfgGet((*calls)[0].old, "memory", "memory_enabled"); mem != false {
		t.Fatalf("old raw %v", (*calls)[0].old)
	}
	if mem, _ := CfgGet((*calls)[0].new, "memory", "memory_enabled"); mem != true {
		t.Fatalf("new config %v", (*calls)[0].new)
	}
	if err := SetConfigValue(ctx, h, "curator.enabled", "false", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[1].h != h {
		t.Fatalf("raw set observed %v", *calls)
	}
	if v, _ := CfgGet((*calls)[1].new, "curator", "enabled"); v != false {
		t.Fatalf("raw set wrote %v", (*calls)[1].new)
	}
	if v, ok := CfgGet((*calls)[1].old, "curator", "enabled"); ok {
		t.Fatalf("raw set old side already held %v", v)
	}
	if err := SetConfigValue(AppliedWrite(ctx), h, "curator.enabled", "true", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("applied raw set observed %v", (*calls)[2:])
	}
}

func TestSavedObserverGetsAnEmptySnapshotWhenTheRawFileIsNew(t *testing.T) {
	captureFacade(t)
	h := t.TempDir()
	calls := recordSavedCalls(t)
	if err := os.Remove(home.ConfigPath(h)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := SetConfigValue(context.Background(), h, "curator.enabled", "false", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || len((*calls)[0].old) != 0 || !reflect.DeepEqual((*calls)[0].h, h) {
		t.Fatalf("new file observed %v", *calls)
	}
}
