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
	if role := os.Getenv("ICEBATH_WA_TEST_ROLE"); role != "" {
		os.Exit(runHelperRole(role))
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
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

// Wizard reading scripted answers, failing the case when it pairs
func scriptedWizard(t *testing.T, h, input string) (*whatsAppWizard, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	wz := newWhatsAppWizard(context.Background(), h, out)
	wz.console = &cliui.Console{In: strings.NewReader(input), Out: out, StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}
	wz.pair = func(session, mode string) int {
		t.Fatalf("paired %s in %s mode", session, mode)
		return 0
	}
	return wz, out
}

func TestAbortedSetupDoesNotEnableWhatsapp(t *testing.T) {
	h := isolatedWhatsAppHome(t)
	wz, _ := scriptedWizard(t, h, "1\n")
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
	wz, _ := scriptedWizard(t, h, "n\nn\n")
	if err := wz.setup(); err != nil {
		t.Fatal(err)
	}
	if value, _ := dotenvValue(h, "WHATSAPP_ENABLED"); value != "true" {
		t.Fatalf("WHATSAPP_ENABLED %q", value)
	}
}

func TestCLIReportsIncompletePairing(t *testing.T) {
	h := isolatedWhatsAppHome(t)
	t.Setenv("WHATSAPP_MODE", "bot")
	t.Setenv("WHATSAPP_ALLOWED_USERS", "15551234567")
	wz, out := scriptedWizard(t, h, "n\n")
	paired := ""
	wz.pair = func(session, mode string) int {
		paired = mode
		return 1
	}
	if err := wz.setup(); err != nil {
		t.Fatalf("setup %v", err)
	}
	if paired != "bot" || !strings.Contains(out.String(), "Pairing may not have completed. Run 'icebath whatsapp' to try again.") {
		t.Fatalf("paired %q: %q", paired, out.String())
	}
	if value, ok := dotenvValue(h, "WHATSAPP_ENABLED"); ok {
		t.Fatalf("incomplete pairing enabled WhatsApp: %q", value)
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
