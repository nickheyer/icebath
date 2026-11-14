package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func enabledDebugSession(t *testing.T) *DebugSession {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("TEST_DEBUG", "true")
	ds := NewDebugSession(nil, "test_tool", "TEST_DEBUG")
	ds.logDir = t.TempDir()
	return ds
}

func TestDebugSessionDisabledNotActiveByDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	os.Unsetenv("FAKE_DEBUG_VAR_XYZ")
	ds := NewDebugSession(nil, "test_tool", "FAKE_DEBUG_VAR_XYZ")
	if ds.Active() || ds.enabled {
		t.Fatal("debug session active without its env var")
	}
}

func TestDebugSessionEnabledActiveWhenEnvSet(t *testing.T) {
	ds := enabledDebugSession(t)
	if !ds.Active() || !ds.enabled {
		t.Fatal("debug session inactive with its env var set")
	}
}

func TestDebugSessionEnabledSaveEmptyLog(t *testing.T) {
	ds := enabledDebugSession(t)
	ds.Save(nil)
	files, err := filepath.Glob(filepath.Join(ds.logDir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v err %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		TotalCalls int   `json:"total_calls"`
		ToolCalls  []any `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.TotalCalls != 0 || data.ToolCalls == nil || len(data.ToolCalls) != 0 {
		t.Fatalf("payload %s", raw)
	}
}
