package startupfast

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Writes text to path under dir, creating the parents
func writeTree(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Fresh home and a fresh tree that heads the import path for one test
func freshRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pm.PatchRepoRoot(root))
	pm.EnsureProjectRootOnPath()
	return root
}

func TestReadInstallMethodStamp(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", "  "+h+"  ")
	if got := readInstallMethod(); got != "" {
		t.Fatalf("missing stamp %q", got)
	}
	writeTree(t, filepath.Join(h, ".install_method"), []byte("\ufeff  Git\r\n"))
	if got := readInstallMethod(); got != "git" {
		t.Fatalf("stamp %q", got)
	}
	writeTree(t, filepath.Join(h, ".install_method"), []byte("g\xffit\n"))
	if got := readInstallMethod(); got != "" {
		t.Fatalf("undecodable stamp %q", got)
	}
}

func TestActiveProfileMayOverrideHome(t *testing.T) {
	h := t.TempDir()
	if activeProfileMayOverrideHome(h) {
		t.Fatal("no active_profile file")
	}
	for text, want := range map[string]bool{"default\n": false, "  \n": false, "\ufeffwork\r\n": true, "w\xffork": false} {
		writeTree(t, filepath.Join(h, "active_profile"), []byte(text))
		if got := activeProfileMayOverrideHome(h); got != want {
			t.Fatalf("%q gave %v", text, got)
		}
	}
}

func TestContainerModeMayBeActive(t *testing.T) {
	inside := isContainerStartupEnvironment()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_DEV", "")
	if ContainerModeMayBeActive() {
		t.Fatal("plain home")
	}
	writeTree(t, filepath.Join(h, "active_profile"), []byte("work\n"))
	if got := ContainerModeMayBeActive(); got == inside {
		t.Fatalf("active profile gave %v", got)
	}
	profile := filepath.Join(h, "profiles", "work")
	writeTree(t, filepath.Join(profile, "active_profile"), []byte("work\n"))
	t.Setenv("ICEBATH_HOME", profile+string(filepath.Separator))
	if ContainerModeMayBeActive() {
		t.Fatal("a profile home never redirects again")
	}
	writeTree(t, filepath.Join(profile, ".container-mode"), nil)
	if got := ContainerModeMayBeActive(); got == inside {
		t.Fatalf("container mode file gave %v", got)
	}
	t.Setenv("ICEBATH_DEV", "1")
	if ContainerModeMayBeActive() {
		t.Fatal("dev mode")
	}
}

func TestUpdateStatusLine(t *testing.T) {
	status := func(pairs ...any) *pyjson.Object {
		o := pyjson.NewObject()
		for i := 0; i < len(pairs); i += 2 {
			o.Set(pairs[i].(string), pairs[i+1])
		}
		return o
	}
	ok := func() (string, error) { return "icebath update", nil }
	broken := func() (string, error) { return "", errors.New("no config") }
	cases := []struct {
		name    string
		status  *pyjson.Object
		command func() (string, error)
		want    string
	}{
		{"rollback first", status("rollback", true, "behind", int64(3), "sourceVersion", "1.2.3"), ok, "Newer than stable release v1.2.3 — 'icebath update' moves back to it"},
		{"rollback none version", status("rollback", true), ok, "Newer than stable release vNone — 'icebath update' moves back to it"},
		{"rollback command fails", status("rollback", true), broken, ""},
		{"no count", status("behind", int64(-1)), ok, "Update available — run 'icebath update'"},
		{"no count float", status("behind", -1.0), ok, "Update available — run 'icebath update'"},
		{"several", status("behind", big.NewInt(3)), ok, "Update available: 3 commits behind — run 'icebath update'"},
		{"one", status("behind", int64(1)), ok, "Update available: 1 commit behind — run 'icebath update'"},
		{"true counts as one", status("behind", true), ok, "Update available: True commit behind — run 'icebath update'"},
		{"behind command fails", status("behind", int64(2)), broken, ""},
		{"up to date", status("behind", int64(0)), broken, "Up to date"},
		{"false is zero", status("behind", false), ok, "Up to date"},
		{"none", status(), ok, ""},
		{"negative", status("behind", int64(-5)), ok, ""},
		{"text raises", status("behind", "3"), ok, ""},
		{"empty text", status("behind", ""), ok, ""},
	}
	for _, tc := range cases {
		line, printed := updateStatusLine(tc.status, tc.command)
		if line != tc.want || printed != (tc.want != "") {
			t.Fatalf("%s gave %q %v", tc.name, line, printed)
		}
	}
}

func TestPrintFastVersionInfoFields(t *testing.T) {
	freshRoot(t)
	var out bytes.Buffer
	PrintFastVersionInfo(context.Background(), &out, false)
	text := out.String()
	goLine := "\nGo: " + strings.TrimPrefix(strings.Fields(runtime.Version())[0], "go") + "\n"
	for _, field := range []string{"Icebath Agent v", "Install directory: ", goLine} {
		if !strings.Contains(text, field) {
			t.Fatalf("missing %q in\n%s", field, text)
		}
	}
	for _, field := range []string{"Python", "OpenAI SDK", "SDK:"} {
		if strings.Contains(text, field) {
			t.Fatalf("Go binary states %q in\n%s", field, text)
		}
	}
}
