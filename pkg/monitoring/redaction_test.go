package monitoring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestSecretKeyAlwaysStripped(t *testing.T) {
	fakeKey := "sk-ant-api03-" + strings.Repeat("A", 24)
	if out, ok := RedactForExport("calling with key " + fakeKey + " and moving on"); !ok || strings.Contains(out, fakeKey) {
		t.Fatalf("key survived %q", out)
	}
}

func TestBearerHeaderStripped(t *testing.T) {
	if out, ok := RedactForExport("Authorization: Bearer abc.def-ghi_jkl"); !ok || strings.Contains(out, "abc.def-ghi_jkl") {
		t.Fatalf("bearer survived %q", out)
	}
}

func TestStructurePreserved(t *testing.T) {
	out, ok := RedactForExport("platform.slack entered fatal after auth_failed")
	if !ok || !strings.Contains(out, "platform.slack") || !strings.Contains(out, "auth_failed") {
		t.Fatalf("structure lost %q", out)
	}
}

func TestFailsClosedWhenRedactorUnavailable(t *testing.T) {
	prev := egressSecretRedactor
	t.Cleanup(func() { egressSecretRedactor = prev })
	egressSecretRedactor = func(string) string { panic("redactor unavailable") }
	if out, _ := RedactForExport("secret sauce sk-live-key"); out != "[redaction-unavailable]" {
		t.Fatalf("raw text escaped %q", out)
	}
}

func TestPIIShapesAndBounds(t *testing.T) {
	out, _ := RedactForExport("mail a.b@example.org id 123e4567-e89b-12d3-a456-426614174000 call +1 415-555-0100 ok")
	if out != "mail [email] id [id] call [phone] ok" {
		t.Fatalf("pii %q", out)
	}
	if _, ok := RedactForExport(nil); ok {
		t.Fatal("None produced text")
	}
	if got := RedactBounded(nil); got != "[redacted]" {
		t.Fatalf("empty %q", got)
	}
	if got := RedactBoundedWith("abcdef", 3, "", ""); got != "abc" {
		t.Fatalf("bounded %q", got)
	}
	if got := RedactBoundedWith("abcdef", -2, "", ""); got != "abcd" {
		t.Fatalf("negative bound %q", got)
	}
	prev := egressSecretRedactor
	t.Cleanup(func() { egressSecretRedactor = prev })
	egressSecretRedactor = func(string) string { return "" }
	if got := RedactBoundedWith("x", 10, "[gone]", ""); got != "[gone]" {
		t.Fatalf("empty marker %q", got)
	}
}

func TestInstallIDPersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	load := func() map[string]any {
		cfg, err := loadConfigForTest(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	first := EnsureInstallID(t.Context(), load())
	if first == "" || first == "unknown" {
		t.Fatalf("minted %q", first)
	}
	if second := EnsureInstallID(t.Context(), load()); second != first {
		t.Fatalf("second load minted %q, want %q", second, first)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if !strings.Contains(string(raw), first) {
		t.Fatalf("config.yaml lacks the id %s", raw)
	}
	cfg := map[string]any{"monitoring": map[string]any{"install_id": "  "}}
	if got := EnsureInstallID(t.Context(), cfg); got == "" || cfg["monitoring"].(map[string]any)["install_id"] != got {
		t.Fatalf("blank id not replaced in memory %v", cfg)
	}
}

func loadConfigForTest(dir string) (map[string]any, error) {
	tree, err := config.Load(dir, nil)
	return map[string]any(tree), err
}
