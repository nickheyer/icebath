package gatewaycli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExitDiagStatesGoRuntimeVersion(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_GATEWAY_EXIT_DIAG", "1")
	makeExitDiag(context.Background())("runner.returned", [2]any{"code", 0})
	data, err := os.ReadFile(filepath.Join(h, "logs", "gateway-exit-diag.log"))
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatalf("%v in %q", err, data)
	}
	if want := strings.TrimPrefix(strings.Fields(runtime.Version())[0], "go"); line["go"] != want {
		t.Fatalf("go = %v want %s in %s", line["go"], want, data)
	}
	if _, has := line["python"]; has {
		t.Fatalf("diag line of the Go binary states a python runtime: %s", data)
	}
	if line["tag"] != "runner.returned" || line["code"] != float64(0) {
		t.Fatalf("line %s", data)
	}
	if strings.Index(string(data), `"go"`) > strings.Index(string(data), `"platform"`) {
		t.Fatalf("go precedes platform in %s", data)
	}
}
