package whatsapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

// Fresh profile home with no WhatsApp variables in the environment
func isolatedWhatsAppHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "home", ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("ICEBATH_HOME", h)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "WHATSAPP_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	return h
}

// Value of a key in the profile .env, false when absent
func dotenvValue(h, key string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(h, ".env"))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`), true
		}
	}
	return "", false
}

// Wizard over scripted input with a package manager that has nothing installed and prepares nothing runnable
func scriptedWizard(t *testing.T, h, input string, bridge string) (*whatsAppWizard, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	wz := newWhatsAppWizard(context.Background(), h, out)
	wz.console = &cliui.Console{In: strings.NewReader(input), Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}
	wz.findNode = func(command string) (string, error) { return "", nil }
	wz.nodeEnv = func() map[string]string { return map[string]string{} }
	wz.ensure = func(name string) (map[string]string, error) { return map[string]string{}, nil }
	wz.installed = func(name string) (*pm.InstalledPackage, error) { return nil, nil }
	wz.run = func(argv []string, dir string, env map[string]string, captureStderr bool) (bridgeRun, error) {
		t.Fatalf("spawned %v", argv)
		return bridgeRun{}, nil
	}
	wz.bridgeDir = func() string { return bridge }
	return wz, out
}

// Bridge directory holding its script, with its dependencies installed when asked
func seededBridge(t *testing.T, withModules bool) string {
	t.Helper()
	bridge := filepath.Join(t.TempDir(), "bridge")
	if err := os.MkdirAll(bridge, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bridge, "bridge.js"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if withModules {
		if err := os.MkdirAll(filepath.Join(bridge, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bridge
}

func TestAbortedSetupDoesNotEnableWhatsapp(t *testing.T) {
	h := isolatedWhatsAppHome(t)
	wz, _ := scriptedWizard(t, h, "1\n", filepath.Join(t.TempDir(), "absent"))
	err := wz.setup()
	if err != nil && !promptCancelled(err) {
		t.Fatal(err)
	}
	if value, ok := dotenvValue(h, "WHATSAPP_ENABLED"); ok {
		t.Fatalf("setup aborted before pairing but WHATSAPP_ENABLED=%q", value)
	}
}

func TestExistingPairingSkipBranchEnablesWhatsapp(t *testing.T) {
	h := isolatedWhatsAppHome(t)
	session := filepath.Join(h, "whatsapp", "session")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHATSAPP_MODE", "bot")
	t.Setenv("WHATSAPP_ALLOWED_USERS", "15551234567")
	wz, _ := scriptedWizard(t, h, "n\nn\n", seededBridge(t, true))
	if err := wz.setup(); err != nil {
		t.Fatal(err)
	}
	if value, _ := dotenvValue(h, "WHATSAPP_ENABLED"); value != "true" {
		t.Fatalf("WHATSAPP_ENABLED %q", value)
	}
}

func TestCLIReportsPreparedBridgeBinaryMissing(t *testing.T) {
	for _, executable := range []string{"npm", "node"} {
		h := isolatedWhatsAppHome(t)
		var out *bytes.Buffer
		if executable == "npm" {
			var wz *whatsAppWizard
			wz, out = scriptedWizard(t, h, "", seededBridge(t, false))
			if ok, err := wz.whatsappInstallBridge(wz.bridgeDir()); err != nil || ok {
				t.Fatalf("CLI continued despite missing npm binary: %v %v", ok, err)
			}
		} else {
			t.Setenv("WHATSAPP_MODE", "bot")
			t.Setenv("WHATSAPP_ALLOWED_USERS", "15551234567")
			var wz *whatsAppWizard
			wz, out = scriptedWizard(t, h, "n\n", seededBridge(t, true))
			if err := wz.setup(); err != nil {
				t.Fatalf("setup %v", err)
			}
		}
		text := out.String()
		if !strings.Contains(text, executable) || !strings.Contains(text, "binary") {
			t.Fatalf("%s: CLI did not report the missing binary: %q", executable, text)
		}
	}
}

func TestReadonlyInstallMirrorsToHermesHome(t *testing.T) {
	root := t.TempDir()
	installRoot := filepath.Join(root, "install")
	installBridge := filepath.Join(installRoot, "scripts", "whatsapp-bridge")
	if err := os.MkdirAll(installBridge, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(installBridge, "bridge.js"), []byte("// bridge\n"), 0o644)
	os.WriteFile(filepath.Join(installBridge, "package.json"), []byte("{\"name\": \"whatsapp-bridge\"}\n"), 0o644)
	h := filepath.Join(root, "icebath_home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(pm.PatchRepoRoot(installRoot))
	if err := os.Chmod(installBridge, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(installBridge, 0o755)
		os.Chmod(filepath.Join(h, "scripts", "whatsapp-bridge"), 0o755)
	})
	resolved := ResolveWhatsAppBridgeDir(context.Background())
	expected := filepath.Join(h, "scripts", "whatsapp-bridge")
	if resolved != expected {
		t.Fatalf("resolved %s", resolved)
	}
	if raw, _ := os.ReadFile(filepath.Join(expected, "bridge.js")); string(raw) != "// bridge\n" {
		t.Fatalf("bridge.js %q", raw)
	}
	if _, err := os.Stat(filepath.Join(expected, "package.json")); err != nil {
		t.Fatal(err)
	}
}
