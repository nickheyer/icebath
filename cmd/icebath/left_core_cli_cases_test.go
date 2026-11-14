package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Plugin manager stand in with every catalog lookup found and the given installer
func installLeftCoreCLIHooks(install func(ctx context.Context, name string, consent bool) (map[string]any, error)) {
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{
		CatalogEntry:    func(name string) (string, bool) { return name, true },
		Install:         install,
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
	})
}

// Temp .icebath home bound as the process home with the process argv set like the hermes test
func leftCoreCLIHome(t *testing.T, argv ...string) (string, context.Context) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	prevArgs := os.Args
	os.Args = append([]string{"icebath"}, argv...)
	t.Cleanup(func() { os.Args = prevArgs })
	installLeftCoreCLIHooks(func(context.Context, string, bool) (map[string]any, error) {
		return nil, errors.New("cli tests install no plugins")
	})
	return h, context.Background()
}

// Text written to the given process stream while fn runs
func captureStream(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	prev := *stream
	*stream = f
	defer func() { *stream = prev }()
	fn()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func leftCoreExit(t *testing.T, err error) *commands.ExitError {
	t.Helper()
	var exit *commands.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("error %T %v", err, err)
	}
	return exit
}

func TestOldMCPServeSpellingNamesThePluginCommand(t *testing.T) {
	h, ctx := leftCoreCLIHome(t, "mcp", "serve", "--tools")
	var err error
	out := captureStream(t, &os.Stdout, func() { err = newCLI().Execute([]string{"mcp", "serve", "--tools"}) })
	exit := leftCoreExit(t, err)
	if exit.Code != 2 || out != "" {
		t.Fatalf("code %d stdout %q", exit.Code, out)
	}
	if !strings.Contains(exit.Msg, "`icebath mcp serve` is now `icebath mcp-serve`") || !strings.Contains(exit.Msg, "plugins install mcp-serve") {
		t.Fatalf("stderr %q", exit.Msg)
	}
	if err := os.MkdirAll(filepath.Join(h, "plugins", "mcp-serve"), 0o755); err != nil {
		t.Fatal(err)
	}
	hint := provider.MovedCommandHint(ctx, "icebath mcp", "serve", "")
	if !strings.Contains(hint, "is now `icebath mcp-serve`") || strings.Contains(hint, "install") {
		t.Fatalf("installed hint %q", hint)
	}
}

func TestOldSpellingWithoutThePluginNamesTheInstall(t *testing.T) {
	for _, argv := range [][]string{{"import-agent", "claude-code", "--dry-run"}, {"claw", "migrate", "--dry-run"}} {
		_, ctx := leftCoreCLIHome(t, argv...)
		exit := leftCoreExit(t, newCLI().Execute(argv))
		if exit.Code != 2 {
			t.Fatalf("%v code %d", argv, exit.Code)
		}
		want := "`icebath " + argv[0] + "` moved out of core into the 'agent-importers' plugin, which is not loaded"
		if !strings.Contains(exit.Msg, want) || !strings.Contains(exit.Msg, "icebath plugins install agent-importers") {
			t.Fatalf("%v stderr %q", argv, exit.Msg)
		}
		if hint := provider.MovedCommandHint(ctx, "icebath auth", argv[0], ""); hint != "" {
			t.Fatalf("%v auth hint %q", argv, hint)
		}
	}
}
