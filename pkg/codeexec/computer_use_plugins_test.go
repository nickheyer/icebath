package codeexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cuFakePluginSource = "from tools.computer_use.backend import ComputerUseProvider\n\ndef register(ctx):\n    ctx.register_computer_use_provider(None)\n"

// Profile home holding the named user plugins and an optional computer_use.backend and isolation mode
func cuPluginHome(t *testing.T, dir string, plugins []string, backend, isolation string) *tools.Env {
	t.Helper()
	for _, p := range plugins {
		pd := filepath.Join(dir, "plugins", p)
		if err := os.MkdirAll(pd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pd, "plugin.yaml"), []byte("name: "+p+"\ndescription: "+p+" driver\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pd, "__init__.py"), []byte(cuFakePluginSource), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var cfg strings.Builder
	if backend != "" {
		cfg.WriteString("computer_use:\n  backend: " + backend + "\n")
	}
	if isolation != "" {
		cfg.WriteString("plugins:\n  isolation: " + isolation + "\n")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return &tools.Env{Home: dir, Config: config.Tree{}, Values: &sync.Map{}, Cwd: dir}
}

func TestUnknownBackendErrorsNamingConfiguredAndAvailable(t *testing.T) {
	env := cuPluginHome(t, filepath.Join(t.TempDir(), "home"), nil, "nope", "")
	_, err := getActiveProvider(env)
	var le *lookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a lookupError", err)
	}
	for _, want := range []string{"'nope'", "available: cua", "is not installed", "~/.icebath/plugins/<name>/", "`icebath tools`"} {
		if !strings.Contains(le.Error(), want) {
			t.Fatalf("%q missing %q", le.Error(), want)
		}
	}
}

func TestBuiltinCuaIsDefaultAndPluginNeverSelfActivates(t *testing.T) {
	for _, backend := range []string{"", "cua", "  "} {
		env := cuPluginHome(t, filepath.Join(t.TempDir(), "home"), []string{"cu-alpha"}, backend, "")
		if got := configuredBackendName(env); got != "cua" {
			t.Fatalf("backend %q resolved %q", backend, got)
		}
		p, err := getActiveProvider(env)
		if err != nil || p.Name() != "cua" || p.DisplayName() != "cua-driver (background)" {
			t.Fatalf("provider %v err %v", p, err)
		}
		b, err := p.CreateBackend("standard")
		if _, ok := b.(*CuaDriverBackend); !ok || err != nil {
			t.Fatalf("backend %T err %v", b, err)
		}
	}
}

func TestDiscoveryListsBundledThenUserWithoutLoading(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	env := cuPluginHome(t, dir, []string{"cu-beta", "cu-alpha", "cua"}, "cu-beta", "")
	if err := os.MkdirAll(filepath.Join(dir, "plugins", "not-cu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugins", "not-cu", "__init__.py"), []byte("def register(ctx): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := discoverComputerUseProviders(env)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"cua", cuaProviderDescription}, {"cu-alpha", "cu-alpha driver"}, {"cu-beta", "cu-beta driver"}}
	if len(found) != len(want) {
		t.Fatalf("found %v", found)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Fatalf("row %d = %v, want %v", i, found[i], want[i])
		}
	}
	d, ok, err := findComputerUseProviderDir(env, "cua")
	if err != nil || !ok || d.bundled == nil {
		t.Fatalf("cua resolved to %+v ok %v err %v, the bundled provider must win", d, ok, err)
	}
}

func TestEachProfileGetsItsOwnConfiguredBackend(t *testing.T) {
	root := t.TempDir()
	a := cuPluginHome(t, filepath.Join(root, "profiles", "a"), []string{"cu-alpha"}, "cu-alpha", "")
	b := cuPluginHome(t, filepath.Join(root, "profiles", "b"), []string{"cu-beta"}, "cu-beta", "")
	for _, c := range []struct {
		env  *tools.Env
		want string
	}{{a, "cu-alpha"}, {b, "cu-beta"}, {a, "cu-alpha"}} {
		if got := configuredBackendName(c.env); got != c.want {
			t.Fatalf("configured %q, want %q", got, c.want)
		}
		d, ok, err := findComputerUseProviderDir(c.env, c.want)
		if err != nil || !ok || !strings.HasPrefix(d.dir, c.env.Home) {
			t.Fatalf("%s resolved %+v ok %v err %v", c.want, d, ok, err)
		}
	}
}

func TestHostIsolationRefusesUserProvider(t *testing.T) {
	env := cuPluginHome(t, filepath.Join(t.TempDir(), "home"), []string{"cu-alpha"}, "cu-alpha", "host")
	_, err := getActiveProvider(env)
	var le *lookupError
	if !errors.As(err, &le) || !strings.Contains(le.Error(), "could not be loaded (see the log)") || !strings.Contains(le.Error(), "available: cua, cu-alpha") {
		t.Fatalf("err = %v", err)
	}
}
