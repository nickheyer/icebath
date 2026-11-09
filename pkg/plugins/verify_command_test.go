package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func verifyArgs(path string, overrides map[string]any) map[string]any {
	ns := map[string]any{"path": path, "detect_only": false, "save": false, "skip_start": false, "phase": nil,
		"port": nil, "timeout": 60.0, "ready_timeout": 5.0, "json": false}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

func runVerifyForTest(t *testing.T, ns map[string]any) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunVerifyCommand(context.Background(), ns, &out, &errOut)
	return code, out.String(), errOut.String()
}

func loadsObject(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("json %v in %q", err, text)
	}
	return v.(*pyjson.Object)
}

func TestDetectOnlyJson(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"detect_only": true, "json": true}))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	payload := loadsObject(t, out)
	source, _ := payload.Get("source")
	recipe, _ := payload.Get("recipe")
	kind, _ := recipe.(*pyjson.Object).Get("kind")
	build, _ := recipe.(*pyjson.Object).Get("build")
	if source != "detected" || kind != "go" || len(build.([]any)) != 1 || build.([]any)[0] != "go build ./..." {
		t.Fatalf("payload %s", out)
	}
}

func TestNoRecipeFound(t *testing.T) {
	code, _, errOut := runVerifyForTest(t, verifyArgs(t.TempDir(), map[string]any{"detect_only": true}))
	if code != 1 || !strings.Contains(errOut, "No recognizable project") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

func TestNoRecipeFoundJsonReportsTheRoot(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"detect_only": true, "json": true}))
	payload := loadsObject(t, out)
	ok, _ := payload.Get("ok")
	reason, _ := payload.Get("error")
	if code != 1 || ok != false || reason != "no-recipe" {
		t.Fatalf("code %d payload %s", code, out)
	}
}

func TestSaveWritesManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"detect_only": true, "save": true, "json": true}))
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".icebath", "environment.json"))
	if err != nil {
		t.Fatal(err)
	}
	payload := loadsObject(t, string(raw))
	version, _ := payload.Get("version")
	recipe, _ := payload.Get("recipe")
	kind, _ := recipe.(*pyjson.Object).Get("kind")
	if version != int64(1) || kind != "go" {
		t.Fatalf("manifest %s", raw)
	}
}

func writeVerifyManifest(t *testing.T, dir, test string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".icebath"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".icebath", "environment.json"), []byte(`{"recipe": {"name": "Fake", "test": ["`+test+`"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunPhasesJson(t *testing.T) {
	dir := t.TempDir()
	writeVerifyManifest(t, dir, "echo ok")
	code, out, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"json": true, "skip_start": true}))
	if code != 0 {
		t.Fatalf("code %d out %s", code, out)
	}
	payload := loadsObject(t, out)
	ok, _ := payload.Get("ok")
	source, _ := payload.Get("source")
	phases, _ := payload.Get("phases")
	command, _ := phases.([]any)[0].(*pyjson.Object).Get("command")
	if ok != true || source != "manifest" || command != "echo ok" {
		t.Fatalf("payload %s", out)
	}
}

func TestFailingPhaseExitCode(t *testing.T) {
	dir := t.TempDir()
	writeVerifyManifest(t, dir, "false")
	code, out, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"json": true}))
	payload := loadsObject(t, out)
	ok, _ := payload.Get("ok")
	if code != 1 || ok != false {
		t.Fatalf("code %d payload %s", code, out)
	}
}

func TestBadPath(t *testing.T) {
	code, _, errOut := runVerifyForTest(t, verifyArgs(filepath.Join(t.TempDir(), "nope"), nil))
	if code != 2 || !strings.HasPrefix(errOut, "error: not a directory: ") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

func TestHumanReportListsPhasesAndFailingTails(t *testing.T) {
	dir := t.TempDir()
	writeVerifyManifest(t, dir, "sh -c 'echo boom; exit 3'")
	code, out, _ := runVerifyForTest(t, verifyArgs(dir, map[string]any{"skip_start": true}))
	if code != 1 {
		t.Fatalf("code %d out %s", code, out)
	}
	for _, want := range []string{"Recipe: Fake (unknown) — source: manifest", "  test  FAIL   ", "Result: FAILED", "--- output tail: test (sh -c 'echo boom; exit 3') ---", "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from %s", want, out)
		}
	}
}
