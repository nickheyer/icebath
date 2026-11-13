package config

import (
	"context"
	"errors"
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

// rawWrite is one raw write recorder delivery with the file content it replaced
type rawWrite struct {
	h, path     string
	before, doc map[string]any
}

// Installs a raw write recorder that snapshots the file, runs each write and restores the previous recorder afterwards
func recordRawWrites(t *testing.T) *[]rawWrite {
	t.Helper()
	var writes []rawWrite
	facadeMu.Lock()
	prev := rawRecorder
	facadeMu.Unlock()
	RegisterRawConfigWriteRecorder(func(ctx context.Context, h, configPath string, userConfig map[string]any, write func(string, map[string]any) error) error {
		before, err := ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return err
		}
		writes = append(writes, rawWrite{h, configPath, before, userConfig})
		return write(configPath, userConfig)
	})
	t.Cleanup(func() { RegisterRawConfigWriteRecorder(prev) })
	return &writes
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
	writes := recordRawWrites(t)
	if err := SetConfigValue(ctx, h, "curator.enabled", "false", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("raw set reached the save observer %v", (*calls)[1:])
	}
	if len(*writes) != 1 || (*writes)[0].h != h || (*writes)[0].path != home.ConfigPath(h) {
		t.Fatalf("raw set recorded %v", *writes)
	}
	if v, _ := CfgGet((*writes)[0].doc, "curator", "enabled"); v != false {
		t.Fatalf("raw set handed %v", (*writes)[0].doc)
	}
	if v, ok := CfgGet((*writes)[0].before, "curator", "enabled"); ok {
		t.Fatalf("file the raw set replaced already held %v", v)
	}
	landed, err := ReadUserConfigRaw(ctx, home.ConfigPath(h))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := CfgGet(landed, "curator", "enabled"); v != false {
		t.Fatalf("raw set wrote %v", landed)
	}
}

func TestRawWriteLandsWithoutARecorderAndReturnsTheRecordersError(t *testing.T) {
	captureFacade(t)
	h := t.TempDir()
	ctx := context.Background()
	facadeMu.Lock()
	prev := rawRecorder
	facadeMu.Unlock()
	t.Cleanup(func() { RegisterRawConfigWriteRecorder(prev) })
	RegisterRawConfigWriteRecorder(nil)
	if err := os.Remove(home.ConfigPath(h)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := SetConfigValue(ctx, h, "curator.enabled", "false", false, testCommandSources()); err != nil {
		t.Fatal(err)
	}
	landed, err := ReadUserConfigRaw(ctx, home.ConfigPath(h))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := CfgGet(landed, "curator", "enabled"); v != false {
		t.Fatalf("new file holds %v", landed)
	}
	refused := errors.New("disk refused the write")
	RegisterRawConfigWriteRecorder(func(context.Context, string, string, map[string]any, func(string, map[string]any) error) error {
		return refused
	})
	if err := SetConfigValue(ctx, h, "curator.enabled", "true", false, testCommandSources()); !errors.Is(err, refused) {
		t.Fatalf("recorder error surfaced as %v", err)
	}
	if landed, _ = ReadUserConfigRaw(ctx, home.ConfigPath(h)); !reflect.DeepEqual(landed, map[string]any{"curator": map[string]any{"enabled": false}}) {
		t.Fatalf("refused write changed the file to %v", landed)
	}
}
