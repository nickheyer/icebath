package vsc

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

// Zip holding a package manifest with the version, the way vsce lays an extension out
func fakeVSIX(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("extension/package.json")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(`{"name":"icebath","version":"` + version + `"}`))
	zw.Create("extension/dist/extension.js")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVsixVersion(t *testing.T) {
	v, err := vsixVersion(fakeVSIX(t, "0.3.1"))
	if err != nil || v != "0.3.1" {
		t.Fatalf("version %q %v", v, err)
	}
	if _, err := vsixVersion([]byte("not a zip")); err == nil {
		t.Fatal("garbage read as a vsix")
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.2.0", "0.1.9", true}, {"0.1.0", "0.1.0", false}, {"1.0", "0.9.9", true}, {"0.1.0", "0.1.0.1", false}, {"0.1.0", "", true}, {"", "0.1.0", false}} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Fatalf("newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestCliNames(t *testing.T) {
	if names := cliNames("/usr/lib/code/extensions/git/dist/askpass-main.js"); names[0] != "code" {
		t.Fatalf("plain code first, got %v", names)
	}
	if names := cliNames("/opt/Cursor/resources/app/extensions/git/dist/askpass-main.js"); names[0] != "cursor" {
		t.Fatalf("cursor first, got %v", names)
	}
	if names := cliNames("/Applications/VSCodium.app/Contents/Resources/app/extensions/git/dist/askpass-main.js"); names[0] != "codium" {
		t.Fatalf("codium first, got %v", names)
	}
	if names := cliNames("/usr/share/code-insiders/resources/app/extensions/git/dist/askpass-main.js"); names[0] != "code-insiders" {
		t.Fatalf("insiders first, got %v", names)
	}
}

func TestEditorCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix layout")
	}
	root := t.TempDir()
	app := filepath.Join(root, "resources", "app")
	for _, d := range []string{filepath.Join(app, "extensions", "git", "dist"), filepath.Join(app, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(app, "bin", "code")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("VSCODE_GIT_ASKPASS_NODE", "")
	t.Setenv("VSCODE_GIT_ASKPASS_MAIN", filepath.Join(app, "extensions", "git", "dist", "askpass-main.js"))
	if got, ok := editorCLI(); !ok || got != cli {
		t.Fatalf("beside the git helper: %q %v", got, ok)
	}
	t.Setenv("VSCODE_GIT_ASKPASS_MAIN", filepath.Join(t.TempDir(), "editor", "extensions", "git", "dist", "askpass-main.js"))
	if got, ok := editorCLI(); ok {
		t.Fatalf("nothing on the PATH should find nothing, got %q", got)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "code-oss"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got, ok := editorCLI(); !ok || got != filepath.Join(bin, "code-oss") {
		t.Fatalf("on the PATH: %q %v", got, ok)
	}
}

func TestBridgeTarget(t *testing.T) {
	t.Setenv(EnvVar, "")
	t.Setenv("TERM_PROGRAM", "vscode")
	if target, ok := bridgeTarget(Default()); ok {
		t.Fatalf("a test process is no terminal, got %q", target)
	}
	t.Setenv(EnvVar, "/run/x.sock")
	if target, ok := bridgeTarget(Default()); !ok || target != "/run/x.sock" {
		t.Fatalf("variable should win, got %q %v", target, ok)
	}
}

func TestBundledVSIX(t *testing.T) {
	data, version, ok := bundledVSIX()
	if _, err := os.Stat(filepath.Join("bundle", "icebath.vsix")); err != nil {
		if ok {
			t.Fatal("no vsix beside the bundle file yet one was embedded")
		}
		return
	}
	if !ok || len(data) == 0 {
		t.Fatal("vsix beside the bundle file was not embedded")
	}
	pkg, err := os.ReadFile(filepath.Join("..", "..", "..", "vscode", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pkg, []byte(`"version": "`+version+`"`)) {
		t.Fatalf("embedded vsix is %s, vscode/package.json says otherwise", version)
	}
}

func TestInstallExtensionLive(t *testing.T) {
	if os.Getenv("ICEBATH_LIVE_INSTALL") == "" {
		t.Skip("set ICEBATH_LIVE_INSTALL=1 to install the bundled extension into the editor of this terminal")
	}
	data, _, ok := bundledVSIX()
	if !ok {
		t.Fatal("this build carries no VS Code extension, build it with make vsix")
	}
	cli, err := installExtension(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed with %s", cli)
}

func TestInstalledVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, dir)
	if v, err := InstalledVersion(); err != nil || v != "" {
		t.Fatalf("fresh machine reports %q %v", v, err)
	}
	if err := markInstalled("0.1.2"); err != nil {
		t.Fatal(err)
	}
	if v, err := InstalledVersion(); err != nil || v != "0.1.2" {
		t.Fatalf("after marking %q %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ext", "vsc.installed")); err != nil {
		t.Fatal(err)
	}
	if err := markInstalled("0.2.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := InstalledVersion(); v != "0.2.0" {
		t.Fatalf("second mark left %q", v)
	}
}

func TestInstallCommand(t *testing.T) {
	c := installCommand()
	if c.ID != "vsc.install" || c.Slash != "vsc-install" || c.Title == "" || c.Run == nil {
		t.Fatalf("command %+v", c)
	}
}
