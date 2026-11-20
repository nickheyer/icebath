package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestFirstOpenUsesLongerFloor(t *testing.T) {
	env := &tools.Env{Config: config.Tree{"browser": map[string]any{"command_timeout": 30}}}
	if got := launchTimeout(env, nil); got != minFirstLaunchTimeout {
		t.Fatalf("timeout %v", got)
	}
}

func TestRespectsConfigAboveFloor(t *testing.T) {
	env := &tools.Env{Config: config.Tree{"browser": map[string]any{"command_timeout": 180}}}
	if got := launchTimeout(env, nil); got != 180*time.Second {
		t.Fatalf("timeout %v", got)
	}
}

func TestApparmorUsernsTriggersBypass(t *testing.T) {
	pin(t, &launcherInDocker, func() bool { return false })
	pin(t, &launcherGeteuid, func() int { return 1000 })
	sysctl := filepath.Join(t.TempDir(), "apparmor_restrict_unprivileged_userns")
	os.WriteFile(sysctl, []byte("1\n"), 0o644)
	pin(t, &apparmorUsernsSysctl, sysctl)
	if !hostNeedsChromiumSandboxBypass() {
		t.Fatal("the AppArmor userns denial did not trigger the bypass")
	}
}

// browser_store, a pm whose chromium appears once published and a host that never downloads on its own
type pmStore struct {
	t         *testing.T
	env       *tools.Env
	binary    string
	published string
	ensures   int
	fail      bool
	lazy      bool
}

func browserStore(t *testing.T) *pmStore {
	env := testEnv(t)
	s := &pmStore{t: t, env: env, binary: filepath.Join(env.Home, "tools", "chromium", "browser", "chrome")}
	t.Setenv("PATH", "")
	pin(t, &runtimePMExecutable, func() string { return s.published })
	pin(t, &runtimePMHasNoChromium, func() bool { return false })
	pin(t, &runtimeIsTermux, func() bool { return false })
	pin(t, &launcherIsTermux, func() bool { return false })
	pin(t, &launcherInDocker, func() bool { return false })
	pin(t, &launcherLazyInstallsAllowed, func() bool { return s.lazy })
	pin(t, &launcherEnsureChromium, func() error {
		s.ensures++
		if s.fail {
			return errors.New("chromium: download failed")
		}
		s.publish()
		return nil
	})
	launcherMu.Lock()
	autoinstallAttempted = false
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); autoinstallAttempted = false; launcherMu.Unlock() })
	return s
}

func (s *pmStore) publish() string {
	os.MkdirAll(filepath.Dir(s.binary), 0o755)
	os.WriteFile(s.binary, []byte("browser-fixture"), 0o755)
	s.published = s.binary
	return s.binary
}

func (s *pmStore) pinExecutable(path string) {
	s.env.Config = config.Tree{"browser": map[string]any{"executable_path": path}}
}

func TestUnrecordedPlaywrightCacheIsNotAPMBrowser(t *testing.T) {
	s := browserStore(t)
	os.MkdirAll(filepath.Join(s.env.Home, ".cache", "ms-playwright", "chromium-1234"), 0o755)
	if ok, _ := chromiumReady(s.env); ok {
		t.Fatal("a playwright cache counted as a pm browser")
	}
}

func TestLatePMInstallIsVisibleWithoutCacheReset(t *testing.T) {
	s := browserStore(t)
	if ok, _ := chromiumReady(s.env); ok {
		t.Fatal("ready before the install")
	}
	binary := s.publish()
	if ok, hint := chromiumReady(s.env); !ok || hint != "" || launcherResolveExecutable(s.env) != binary {
		t.Fatalf("ready %v %q", ok, hint)
	}
}

func TestExecutionAcquiresMissingBrowserThroughPM(t *testing.T) {
	s := browserStore(t)
	s.lazy = true
	if ok, hint := chromiumReady(s.env); !ok || hint != "" {
		t.Fatalf("ready %v %q", ok, hint)
	}
	if launcherResolveExecutable(s.env) != s.binary || s.ensures != 1 {
		t.Fatalf("resolved %q after %d ensures", launcherResolveExecutable(s.env), s.ensures)
	}
}

func TestTermuxOwnershipPolicyNeverProvisions(t *testing.T) {
	s := browserStore(t)
	s.lazy = true
	pin(t, &launcherIsTermux, func() bool { return true })
	pin(t, &runtimeIsTermux, func() bool { return true })
	if maybeAutoinstallChromium(s.env) {
		t.Fatal("Termux browser installation is externally owned")
	}
	if ok, _ := chromiumReady(s.env); ok || s.ensures != 0 {
		t.Fatalf("ready %v after %d ensures", ok, s.ensures)
	}
}

func TestAMissingPinnedBrowserIsNamedInTheHint(t *testing.T) {
	s := browserStore(t)
	s.publish()
	missing := filepath.Join(t.TempDir(), "moved away", "chrome")
	s.pinExecutable(missing)
	ok, hint := chromiumReady(s.env)
	if ok || !strings.Contains(hint, "browser.executable_path") || !strings.Contains(hint, missing) || strings.Contains(hint, "icebath pm install chromium") {
		t.Fatalf("ready %v %q", ok, hint)
	}
}

func TestExternalChromiumOverrideSurvivesPMComposition(t *testing.T) {
	s := browserStore(t)
	s.publish()
	override := filepath.Join(t.TempDir(), "user browser")
	os.WriteFile(override, []byte("external"), 0o644)
	s.pinExecutable(override)
	if ok, hint := chromiumReady(s.env); !ok || hint != "" || launcherResolveExecutable(s.env) != override {
		t.Fatalf("ready %v %q resolved %q", ok, hint, launcherResolveExecutable(s.env))
	}
}

func TestChromiumAutoinstallIsOneShot(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "succeeds", true: "fails"}[fails], func(t *testing.T) {
			s := browserStore(t)
			s.lazy, s.fail = true, fails
			for range 2 {
				if ok, _ := chromiumReady(s.env); ok == fails {
					t.Fatalf("ready %v", ok)
				}
			}
			if s.ensures != 1 {
				t.Fatalf("ensures %d", s.ensures)
			}
		})
	}
}

func TestChromiumAcquisitionPolicy(t *testing.T) {
	for _, policy := range []string{"docker", "disabled", "termux", "override"} {
		t.Run(policy, func(t *testing.T) {
			s := browserStore(t)
			s.lazy = policy != "disabled"
			pin(t, &launcherInDocker, func() bool { return policy == "docker" })
			pin(t, &launcherIsTermux, func() bool { return policy == "termux" })
			if policy == "override" {
				override := filepath.Join(t.TempDir(), "external-chrome")
				os.WriteFile(override, nil, 0o644)
				s.pinExecutable(override)
				if got := launcherResolveExecutable(s.env); got != override {
					t.Fatalf("resolved %q", got)
				}
				os.Remove(override)
				if got := launcherResolveExecutable(s.env); got != "" {
					t.Fatalf("resolved %q", got)
				}
			}
			if maybeAutoinstallChromium(s.env) || s.ensures != 0 {
				t.Fatal("forbidden acquisition")
			}
		})
	}
}

func TestBrowserReadinessIgnoresAmbientChromium(t *testing.T) {
	s := browserStore(t)
	os.Mkdir(filepath.Join(s.env.Home, "chromium_headless_shell-1234"), 0o755)
	t.Setenv("PLAYWRIGHT_BROWSERS_PATH", s.env.Home)
	os.WriteFile(filepath.Join(s.env.Home, "chromium"), nil, 0o755)
	t.Setenv("PATH", s.env.Home)
	if ok, _ := chromiumReady(s.env); ok {
		t.Fatal("an ambient chromium counted")
	}
	s.publish()
	if ok, _ := chromiumReady(s.env); !ok {
		t.Fatal("the pm chromium did not count")
	}
}
