package app

import (
	"bytes"
	"github.com/nickheyer/icebath/pkg/cliui"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fake agent install with source package and venv
func guiMakeAgent(t *testing.T, h string) string {
	t.Helper()
	root := filepath.Join(h, "icebath-agent")
	os.MkdirAll(filepath.Join(root, "hermes_cli"), 0o755)
	os.WriteFile(filepath.Join(root, "hermes_cli", "__init__.py"), nil, 0o644)
	os.MkdirAll(filepath.Join(root, "venv", "bin"), 0o755)
	return root
}

// Source built GUI artifacts a desktop build produces
func guiMakeBuild(t *testing.T, h string) {
	t.Helper()
	desktop := filepath.Join(h, "icebath-agent", "apps", "desktop")
	os.MkdirAll(filepath.Join(desktop, "dist"), 0o755)
	os.WriteFile(filepath.Join(desktop, "dist", "index.html"), []byte("<html>"), 0o644)
	os.MkdirAll(filepath.Join(desktop, "release", "linux-unpacked"), 0o755)
	os.MkdirAll(filepath.Join(desktop, "node_modules"), 0o755)
	os.MkdirAll(filepath.Join(h, "icebath-agent", "node_modules"), 0o755)
	os.WriteFile(filepath.Join(h, "desktop-build-stamp.json"), []byte("{}"), 0o644)
}

// User home and XDG roots under the test dir so no real install location is consulted
func guiIsolate(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("HOME", filepath.Join(tmp, "user"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("APPDATA", filepath.Join(tmp, "appdata"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "localappdata"))
	t.Setenv("ProgramFiles", "")
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "root"))
}

func TestGuiInstallSummaryShape(t *testing.T) {
	tmp := t.TempDir()
	guiIsolate(t, tmp)
	h := filepath.Join(tmp, ".icebath")
	guiMakeAgent(t, h)
	guiMakeBuild(t, h)
	summary := guiInstallSummary(h)
	if summary.Vals["agent_installed"] != true || summary.Vals["gui_installed"] != true {
		t.Fatalf("summary %v", summary.Vals)
	}
	artifacts, ok := summary.Vals["source_built_artifacts"].([]any)
	if !ok {
		t.Fatalf("artifacts %T", summary.Vals["source_built_artifacts"])
	}
	for _, p := range artifacts {
		if _, ok := p.(string); !ok {
			t.Fatalf("artifact %v", p)
		}
	}
	if summary.Vals["hermes_home"] != h || summary.Vals["platform"] != guiSysPlatform() {
		t.Fatalf("summary %v", summary.Vals)
	}
}

func TestUninstallRemovesLauncherEntryAndRefreshesCache(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux desktop entries")
	}
	tmp := t.TempDir()
	guiIsolate(t, tmp)
	entry := guiDesktopEntryPath()
	os.MkdirAll(filepath.Dir(entry), 0o755)
	os.WriteFile(entry, []byte("x"), 0o644)
	legacy := filepath.Join(filepath.Dir(entry), guiLegacyDesktopEntryName)
	os.WriteFile(legacy, []byte("x"), 0o644)
	bin := filepath.Join(tmp, "bin")
	os.MkdirAll(bin, 0o755)
	record := filepath.Join(tmp, "refreshed")
	os.WriteFile(filepath.Join(bin, "update-desktop-database"), []byte("#!/bin/sh\necho \"$1\" >> "+record+"\n"), 0o755)
	t.Setenv("PATH", bin)
	h := filepath.Join(tmp, ".icebath")
	root := guiMakeAgent(t, h)
	icon := filepath.Join(root, "apps", "desktop", "assets", "icon.png")
	os.MkdirAll(filepath.Dir(icon), 0o755)
	os.WriteFile(icon, []byte("\x89PNG"), 0o644)
	out := &bytes.Buffer{}
	removed := uninstallGUI(out, h, true)
	has := func(p string) bool {
		for _, r := range removed {
			if r == p {
				return true
			}
		}
		return false
	}
	if !has(entry) || guiExists(entry) || !has(legacy) || guiExists(legacy) {
		t.Fatalf("removed %v", removed)
	}
	if got, _ := os.ReadFile(record); strings.TrimSpace(string(got)) != filepath.Dir(entry) {
		t.Fatalf("refreshed %q", got)
	}
	if !guiExists(icon) {
		t.Fatal("checkout icon removed")
	}
	if st, err := os.Stat(filepath.Join(root, "hermes_cli")); err != nil || !st.IsDir() {
		t.Fatal("agent removed")
	}
}

func TestRemovePathHandlesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix symlink semantics")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "real")
	os.Mkdir(target, 0o755)
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if !guiRemovePath(&bytes.Buffer{}, link) {
		t.Fatal("not removed")
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("link remains")
	}
	if !guiExists(target) {
		t.Fatal("target removed")
	}
}

// Input that fails the test when a dry run asks for confirmation
type guiNoInput struct{ t *testing.T }

func (r guiNoInput) Read([]byte) (int, error) {
	r.t.Fatal("A dry-run must not request removal confirmation")
	return 0, nil
}

// Protected agent state and GUI state laid out under the test home, complete or the record alone
func guiRetentionInstall(t *testing.T, recordOnly bool) (protected, guiFiles map[string]string, record string) {
	t.Helper()
	tmp := t.TempDir()
	guiIsolate(t, tmp)
	h := filepath.Join(tmp, ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	// The running source tree is a git checkout as in hermes, so the steward refusal stays silent
	checkout := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	swap(t, &uninstallProjectRoot, func() string { return checkout })
	root := filepath.Join(h, "icebath-agent")
	protected = map[string]string{
		filepath.Join(root, "hermes_cli", "__init__.py"):                  "# agent source\n",
		filepath.Join(root, "venv", "runtime"):                            "agent runtime",
		filepath.Join(root, "node_modules", "shared-package", "index.js"): "shared workspace dependency",
		filepath.Join(h, "config.yaml"):                                   "model: fixture-model\n",
		filepath.Join(h, ".env"):                                          "FIXTURE_TOKEN=fake\n",
		filepath.Join(h, "sessions", "saved.json"):                        `{"saved": true}`,
	}
	record = desktopInstallRecordPath()
	guiFiles = map[string]string{
		filepath.Join(root, "apps", "desktop", "dist", "index.html"):             "renderer",
		filepath.Join(root, "apps", "desktop", "release", "artifact"):            "release",
		filepath.Join(root, "apps", "desktop", "node_modules", "private-module"): "desktop dependency",
		filepath.Join(h, "desktop-build-stamp.json"):                             `{"fixture": true}`,
		packagedGUIAppPaths()[0]:                                                 "desktop app",
		filepath.Join(desktopUserdataDir(), "connections.json"):                  `{"fixture": true}`,
		record: `{"fixture": true}`,
	}
	if recordOnly {
		guiFiles = map[string]string{record: guiFiles[record]}
	}
	for _, set := range []map[string]string{protected, guiFiles} {
		for path, data := range set {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return protected, guiFiles, record
}

func TestGuiDryRunNeverConfirmsOrRemovesInstalledState(t *testing.T) {
	for _, recordOnly := range []bool{false, true} {
		protected, guiFiles, record := guiRetentionInstall(t, recordOnly)
		for _, yes := range []bool{true, false} {
			out := &bytes.Buffer{}
			notTTY := func() bool { return false }
			restore := SetStreams(&cliui.Console{In: guiNoInput{t}, Out: out, StdinTTY: notTTY, StdoutTTY: notTTY}, &bytes.Buffer{})
			code, err := RunGUIUninstall(t.Context(), map[string]any{"dry_run": true, "yes": yes}, out)
			restore()
			if err != nil || code != 0 {
				t.Fatalf("exit %d %v", code, err)
			}
			for _, set := range []map[string]string{protected, guiFiles} {
				for path, data := range set {
					if b, err := os.ReadFile(path); err != nil || string(b) != data {
						t.Fatalf("%s changed", path)
					}
				}
			}
			text := out.String()
			if !strings.Contains(text, "Dry run") || !strings.Contains(text, record) || strings.Contains(text, "Uninstalled!") {
				t.Fatalf("output %q", text)
			}
		}
		if _, err := CmdUninstall(t.Context(), map[string]any{"dry_run": true}, io.Discard, func(string) { t.Fatal("agent dry run asked for a terminal") }); err != nil {
			t.Fatal(err)
		}
		for _, set := range []map[string]string{protected, guiFiles} {
			for path, data := range set {
				if b, err := os.ReadFile(path); err != nil || string(b) != data {
					t.Fatalf("%s changed by the agent dry run", path)
				}
			}
		}
	}
}

func TestGuiRemovalKeepsAgentDataAndSharedWorkspaceDependencies(t *testing.T) {
	for _, recordOnly := range []bool{false, true} {
		protected, guiFiles, _ := guiRetentionInstall(t, recordOnly)
		if code, err := RunGUIUninstall(t.Context(), map[string]any{"dry_run": false, "yes": true}, &bytes.Buffer{}); err != nil || code != 0 {
			t.Fatalf("exit %d %v", code, err)
		}
		for path, data := range protected {
			if b, err := os.ReadFile(path); err != nil || string(b) != data {
				t.Fatalf("%s changed", path)
			}
		}
		for path := range guiFiles {
			if guiExists(path) {
				t.Fatalf("%s kept", path)
			}
		}
	}
}
