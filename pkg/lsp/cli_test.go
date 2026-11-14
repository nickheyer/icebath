package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, cc CommandContext, args CommandArgs) (int, string, string) {
	var out, errOut strings.Builder
	code := RunCommand(context.Background(), cc, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCommandStatusRendersServiceAndRegistry(t *testing.T) {
	resetCaches(t)
	resetRegistry(t)
	rt, _, _ := testRuntime(t)
	t.Setenv("PATH", "")
	svc := testService(t, rt, Options{WaitTimeout: 5, DisabledServers: []string{"gopls"}})
	svc.markBroken(pairKey{"pyright", "/r"})
	cc := CommandContext{Runtime: rt, Config: func() map[string]any { return map[string]any{} },
		Service: func() (*Service, error) { return svc, nil }}
	code, out, _ := runCLI(t, cc, CommandArgs{})
	for _, want := range []string{"LSP Service\n===========\n  enabled:         True\n  wait_mode:       document\n  wait_timeout:    5.0s\n  install_strategy:manual\n  active clients:  none\n",
		"  broken pairs:    1\n    - ('pyright', '/r')\n", "  disabled in cfg: gopls\n", "Registered Servers\n==================\n",
		"  · pyright                  [missing    ] .py, .pyi\n      Python — Microsoft pyright\n",
		"  ? rust-analyzer            [manual-only] .rs\n      Rust — rust-analyzer  [trusted workspaces only]\n",
		"clangd                   [manual-only] .c, .cpp, .cc, .cxx, .h, … (+3)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q in\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatal(code)
	}
	code, out, _ = runCLI(t, cc, CommandArgs{Subcommand: "status", JSON: true})
	var parsed map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil {
		t.Fatalf("%d %s", code, out)
	}
	service := parsed["service"].(map[string]any)
	if service["enabled"] != true || service["wait_timeout"] != 5.0 || len(service["broken"].([]any)) != 1 ||
		!strings.HasPrefix(out, "{\n  \"service\": {\n    \"enabled\": true,\n") {
		t.Fatalf("%s", out)
	}
	off := CommandContext{Runtime: rt, Service: func() (*Service, error) { return nil, nil }}
	if _, out, _ = runCLI(t, off, CommandArgs{Subcommand: "status", JSON: true}); !strings.Contains(out, "\"service\": {\n    \"enabled\": false\n  }") {
		t.Fatal(out)
	}
}

func TestCommandListWhichInstallAndUnknown(t *testing.T) {
	rt, _, _ := testRuntime(t)
	t.Setenv("PATH", "")
	staged := writeFile(t, filepath.Join(rt.Home, "lsp", "bin", "gopls"), "")
	must(t, os.Chmod(staged, 0o755))
	cc := CommandContext{Runtime: rt}
	_, out, _ := runCLI(t, cc, CommandArgs{Subcommand: "list", InstalledOnly: true})
	if out != "gopls                    [installed  ] .go\n" {
		t.Fatalf("%q", out)
	}
	if code, out, _ := runCLI(t, cc, CommandArgs{Subcommand: "which", Server: "gopls"}); code != 0 || out != staged+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errOut := runCLI(t, cc, CommandArgs{Subcommand: "which", Server: "clangd"}); code != 1 || errOut != "clangd: not installed\n" {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, out, _ := runCLI(t, cc, CommandArgs{Subcommand: "install", Server: "gopls"}); code != 0 || out != "gopls already installed\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, errOut := runCLI(t, cc, CommandArgs{Subcommand: "install", Server: "clangd"}); code != 1 ||
		out != "installing clangd (pkg=clangd) ...\n" || errOut != "clangd: this server requires a manual install. See documentation.\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, cc, CommandArgs{Subcommand: "frob"}); code != 2 || errOut != "unknown lsp subcommand: frob\n" {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, out, _ := runCLI(t, cc, CommandArgs{Subcommand: "restart"}); code != 0 || out != "LSP service shut down. Next edit will respawn clients.\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCommandStatusWarnsAboutMissingShellcheck(t *testing.T) {
	rt, _, _ := testRuntime(t)
	t.Setenv("PATH", "")
	staged := writeFile(t, filepath.Join(rt.Home, "lsp", "bin", "bash-language-server"), "")
	must(t, os.Chmod(staged, 0o755))
	_, out, _ := runCLI(t, CommandContext{Runtime: rt, Service: func() (*Service, error) { return nil, nil }}, CommandArgs{})
	if !strings.Contains(out, "\nBackend warnings\n================\n  ! bash-language-server is installed but shellcheck is missing") ||
		!strings.Contains(out, "  enabled:         False\n\nBackend") {
		t.Fatal(out)
	}
}
