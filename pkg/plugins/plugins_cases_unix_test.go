//go:build !windows

package plugins

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestForceReloadSymmetryShellHookReRegisterFailureDoesNotAbortReload(t *testing.T) {
	h := shellHookWorld(t)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	writeFile(t, filepath.Join(h, "config.yaml"), "hooks:\n  on_session_start:\n  - command: /bin/true\n")
	mustMkdir(t, ShellHookAllowlistPath(homeCtx(h))+".lock")
	logs := captureLogs(t)
	m := NewManager(h)
	m.DiscoverAndLoad(homeCtx(h), true)
	if !m.discovered {
		t.Fatal("a failing shell hook re-register aborted the reload")
	}
	if !strings.Contains(logs.String(), "force-reload shell-hook re-register skipped") {
		t.Fatalf("the re-register never failed: %s", logs.String())
	}
}

func TestForceReloadSymmetryForceReloadOfOneProfileDoesNotOrphanAnother(t *testing.T) {
	root := filepath.Dir(shellHookWorld(t))
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	homes := map[string]string{}
	managers := map[string]*Manager{}
	cfg := func() map[string]any { return hooksCfg("on_session_start", map[string]any{"command": "/bin/true"}) }
	for _, label := range []string{"profile-a", "profile-b"} {
		homes[label] = filepath.Join(root, label)
		writeFile(t, mustMkdir(t, homes[label])+"/config.yaml", "hooks:\n  on_session_start:\n  - command: /bin/true\n")
		ctx := home.WithOverride(context.Background(), homes[label])
		managers[label] = GetPluginManager(ctx)
		if _, err := RegisterShellHooksFromConfig(ctx, cfg(), true, false); err != nil {
			t.Fatal(err)
		}
	}
	count := func(label string) int { return len(managers[label].hookCallbacks("on_session_start")) }
	if count("profile-a") != 1 || count("profile-b") != 1 {
		t.Fatalf("hooks after register a=%d b=%d", count("profile-a"), count("profile-b"))
	}
	managers["profile-a"].DiscoverAndLoad(home.WithOverride(context.Background(), homes["profile-a"]), true)
	if count("profile-a") != 1 || count("profile-b") != 1 {
		t.Fatalf("hooks after force reload a=%d b=%d", count("profile-a"), count("profile-b"))
	}
	second, err := RegisterShellHooksFromConfig(home.WithOverride(context.Background(), homes["profile-b"]), cfg(), true, false)
	if err != nil || len(second) != 0 {
		t.Fatalf("second register %v %v", second, err)
	}
	if count("profile-b") != 1 {
		t.Fatalf("profile-b hooks %d", count("profile-b"))
	}
}
