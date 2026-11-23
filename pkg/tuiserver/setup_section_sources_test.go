package tuiserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Isolated home holding the given config.yaml and .env text
func setupSectionHome(t *testing.T, cfgText, envText string) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeTestFile(t, filepath.Join(h, "config.yaml"), cfgText)
	writeTestFile(t, filepath.Join(h, ".env"), envText)
	config.ResetCache()
	dropCfgCache()
	t.Cleanup(func() {
		config.ResetCache()
		dropCfgCache()
	})
	return h
}

// Response of one request routed through its contract, params and result checked
func contractCall(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	return handleRequest(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
}

func TestSetupActionStartBindsTheParametersOfTheWalk(t *testing.T) {
	setupSectionHome(t, "model:\n  default: m\n", "")
	got := make(chan map[string]string, 1)
	setupParamActions["test.echo"] = func(_ context.Context, _ string, params map[string]string, out io.Writer) bool {
		io.WriteString(out, "echo "+params["host"]+"\n")
		got <- params
		return true
	}
	t.Cleanup(func() { delete(setupParamActions, "test.echo") })
	action, known := setupActionFor("test.echo", map[string]string{"host": "box"})
	if !known || action == nil {
		t.Fatal("param action not found")
	}
	job := &setupJob{}
	job.run(context.Background(), "", action)
	if params := <-got; params["host"] != "box" {
		t.Fatalf("params %v", params)
	}
	if lines, _, done, outcome := job.since(0); !done || !outcome || !reflect.DeepEqual(lines, []any{"echo box"}) {
		t.Fatalf("job %v %v %v", lines, done, outcome)
	}
	if _, known := setupActionFor("xai.oauth.login", nil); !known {
		t.Fatal("a plain action lost its lookup")
	}
	if _, known := setupActionFor("no.such.action", nil); known {
		t.Fatal("unknown action found")
	}
}

func TestSpeechEngineInstallReportsTheSyncOutcome(t *testing.T) {
	prevSync := syncSpeechEngine
	t.Cleanup(func() { syncSpeechEngine = prevSync })
	var synced []string
	syncSpeechEngine = func(_ context.Context, extra string, _ io.Writer) error {
		synced = append(synced, extra)
		return nil
	}
	job := &setupJob{}
	action, _ := setupActionFor("tts.install.kittentts", nil)
	job.run(context.Background(), "", action)
	lines, _, done, outcome := job.since(0)
	if !done || !outcome || !reflect.DeepEqual(synced, []string{"kittentts"}) ||
		!reflect.DeepEqual(lines, []any{"Installing kittentts Python package (~25-80MB model downloaded on first use)...", "kittentts installed and ready to speak."}) {
		t.Fatalf("job %v %v %v synced %v", lines, done, outcome, synced)
	}
	syncSpeechEngine = func(context.Context, string, io.Writer) error {
		return &os.PathError{Op: "open", Path: "/venv", Err: os.ErrPermission}
	}
	job = &setupJob{}
	action, _ = setupActionFor("tts.install.neutts", nil)
	job.run(context.Background(), "", action)
	lines, _, _, outcome = job.since(0)
	if outcome || len(lines) != 4 || !strings.HasPrefix(lines[2].(string), "Failed to install neutts: ") || lines[3] != "Retry with /setup voice" {
		t.Fatalf("job %v %v", lines, outcome)
	}
}
