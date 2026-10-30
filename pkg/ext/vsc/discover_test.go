package vsc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Writes an advertisement file for a window into the configuration directory under test
func advertise(t *testing.T, name string, w Window) string {
	t.Helper()
	dir, err := windowsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWindows(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	if ws, err := Windows(); err != nil || len(ws) != 0 {
		t.Fatalf("no directory should list nothing, got %v %v", ws, err)
	}
	now := time.Now()
	advertise(t, "1.json", Window{Pid: 1, Socket: "/run/a.sock", Workspaces: []string{"/home/x/code/app"}, Created: now.Add(-time.Hour)})
	advertise(t, "2.json", Window{Pid: 2, Socket: "/run/b.sock", Workspaces: []string{"/home/x/other"}, Created: now})
	advertise(t, "3.json", Window{Pid: 3, Workspaces: []string{"/home/x/code"}, Created: now})
	dir, _ := windowsDir()
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	ws, err := Windows()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Pid != 2 || ws[1].Pid != 1 {
		t.Fatalf("windows %+v", ws)
	}
	picked := ordered(ws, "/home/x/code/app/pkg")
	if picked[0].Pid != 1 {
		t.Fatalf("workspace holding the directory should come first, got %+v", picked)
	}
	picked = ordered(ws, "/home/x/elsewhere")
	if picked[0].Pid != 2 {
		t.Fatalf("newest should come first without a match, got %+v", picked)
	}
	ws[0].forget()
	if again, _ := Windows(); len(again) != 1 || again[0].Pid != 1 {
		t.Fatalf("forget left %+v", again)
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		path, root string
		want       bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x/y", "/a", false},
	} {
		if got := within(c.path, c.root); got != c.want {
			t.Fatalf("within(%s, %s) = %v", c.path, c.root, got)
		}
	}
}

func TestInsideVSCode(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	if InsideVSCode() {
		t.Fatal("no TERM_PROGRAM taken as VS Code")
	}
	t.Setenv("TERM_PROGRAM", "vscode")
	if !InsideVSCode() {
		t.Fatal("vscode not recognised")
	}
}

func TestRegistered(t *testing.T) {
	root := t.TempDir()
	ext := filepath.Join(root, "heyerapp.icebath-0.1.0")
	if err := os.MkdirAll(ext, 0o755); err != nil {
		t.Fatal(err)
	}
	w := Window{ExtensionPath: ext}
	if !w.registered("HeyerApp.icebath") {
		t.Fatal("no registry file should count as registered")
	}
	registry := filepath.Join(root, "extensions.json")
	os.WriteFile(registry, []byte(`[{"identifier":{"id":"golang.go"}}]`), 0o600)
	if w.registered("HeyerApp.icebath") {
		t.Fatal("registry without the extension counted as registered")
	}
	os.WriteFile(registry, []byte(`[{"identifier":{"id":"golang.go"}},{"identifier":{"id":"heyerapp.icebath"}}]`), 0o600)
	if !w.registered("HeyerApp.icebath") {
		t.Fatal("registry with the extension in another case not recognised")
	}
	if !(Window{}).registered("HeyerApp.icebath") {
		t.Fatal("window without an extension path should count as registered")
	}
}
