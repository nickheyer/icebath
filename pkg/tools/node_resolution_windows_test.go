package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/lsp"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestWindowsPathPrefersLaunchableCmdButNeverForPmTools(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(tmp, "absent-store"))
	t.Setenv("PATH", tmp)
	for _, tool := range []string{"npm", "pnpm"} {
		for _, name := range []string{tool, tool + ".ps1", tool + ".cmd"} {
			if err := os.WriteFile(filepath.Join(tmp, name), []byte("@exit /b 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	rt := LSPRuntime(&Env{Home: filepath.Join(tmp, "home")})
	if got, err := lsp.FindNodeExecutable(rt, "pnpm"); err != nil || got != filepath.Join(tmp, "pnpm.cmd") {
		t.Fatalf("pnpm = %q, %v", got, err)
	}
	if got, err := lsp.FindNodeExecutable(rt, "npm"); err != nil || got != "" {
		t.Fatalf("npm = %q, %v", got, err)
	}
}
