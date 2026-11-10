package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Fault and context switch counts of reaped children, any spawned and waited subprocess raising them
func childUsage(t *testing.T) int64 {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru); err != nil {
		t.Fatal(err)
	}
	return ru.Minflt + ru.Majflt + ru.Nvcsw + ru.Nivcsw
}

// Recorded pm ensure call
type ensureCall struct {
	name string
	opts pm.EnsureOptions
}

// Ensure options of an explicit install with every other knob unset
func explicitOnly(o pm.EnsureOptions) bool {
	return o.Explicit && o.BaseEnv == nil && !o.SkipVerify && o.Progress == nil && o.PauseEvent == nil && o.DownloadProgress == nil
}

func TestBrowserSetupRespectsProviderAndNativeOwner(t *testing.T) {
	for _, c := range []struct {
		provider                string
		dockerImage, playwright bool
	}{{"browserbase", false, false}, {"chromium", false, true}, {"chromium", false, false}, {"chromium", true, false}} {
		t.Run(c.provider, func(t *testing.T) {
			s, out := newTestToolsSetup(t, "")
			baked := filepath.Join(t.TempDir(), "chromium-executable-path")
			if c.dockerImage {
				if err := os.WriteFile(baked, []byte("/opt/icebath/chromium/chrome"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			found := []string{"", "/pm/chromium/chrome"}
			if c.dockerImage {
				found[1] = ""
			}
			installCalls, chromiumCalls := 0, 0
			var ensures []ensureCall
			swapHook(t, &ToolsConfigHooks.FindBrowserUseCLI, func(*Env) []string { installCalls++; return []string{"harness"} })
			swapHook(t, &ToolsConfigHooks.BakedChromiumFile, func() string { return baked })
			swapHook(t, &ToolsConfigHooks.ChromiumExecutable, func(*Env) string { v := found[chromiumCalls]; chromiumCalls++; return v })
			swapHook(t, &ToolsConfigHooks.ChromiumMissingHint, func(*Env) string {
				if postSetupFileExists(baked) {
					return "The image's Chromium is missing: pull the current Icebath image and recreate the " +
						"container (a volume mounted over the image's tool store hides it)."
				}
				return "Chromium is not installed: run `icebath pm install chromium`."
			})
			swapHook(t, &postSetupIsTermux, func() bool { return false })
			swapHook(t, &postSetupGOOS, "linux")
			swapHook(t, &postSetupModuleImportable, func(_ context.Context, name string) bool { return c.playwright })
			swapHook(t, &postSetupWhich, func(string) string { return "/usr/bin/apt-get" })
			swapHook(t, &postSetupEnsure, func(_ context.Context, name string, opts pm.EnsureOptions) error {
				ensures = append(ensures, ensureCall{name, opts})
				return nil
			})
			before := os.Environ()
			children := childUsage(t)
			if err := s.runPostSetup(c.provider); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(os.Environ(), before) {
				t.Fatal("post setup changed the process environment")
			}
			if installCalls != 1 {
				t.Fatalf("harness lookup ran %d times", installCalls)
			}
			if childUsage(t) != children {
				t.Fatal("post setup ran a subprocess")
			}
			if c.provider == "browserbase" && chromiumCalls != 0 {
				t.Fatalf("browserbase resolved chromium %d times", chromiumCalls)
			}
			if c.provider == "browserbase" || c.dockerImage {
				if len(ensures) != 0 {
					t.Fatalf("pm ensure called %v", ensures)
				}
			} else if len(ensures) != 1 || ensures[0].name != "chromium" || !explicitOnly(ensures[0].opts) {
				t.Fatalf("pm ensure calls %+v", ensures)
			}
			output := out.String()
			switch {
			case c.dockerImage:
				if !strings.Contains(output, "image") {
					t.Fatalf("output %q", output)
				}
			case c.provider == "chromium":
				if !strings.Contains(output, "/pm/chromium/chrome") || !strings.Contains(output, "system") || strings.Contains(output, "npx") {
					t.Fatalf("output %q", output)
				}
				if c.playwright {
					python, err := postSetupInterpreter(s.ctx())
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(output, python) || !strings.Contains(output, "-m playwright install-deps chromium") {
						t.Fatalf("output %q lacks %s", output, python)
					}
				} else if !strings.Contains(output, "https://playwright.dev/python/docs/browsers#install-system-dependencies") {
					t.Fatalf("output %q", output)
				}
			}
		})
	}
}
