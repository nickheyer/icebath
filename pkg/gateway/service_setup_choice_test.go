package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Supervisor peer of the choice cases, a RAM backed home and the faked systemctl, loginctl and sudo
type choiceSupervisor struct {
	profile string
	calls   [][]string
	active  bool
	fail    bool
}

// Home under /dev/shm the temp home guard accepts, with the command boundary faked like the hermes supervisor fixture
func newChoiceSupervisor(t *testing.T) *choiceSupervisor {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "icebath-choice-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &choiceSupervisor{profile: filepath.Join(dir, "profile")}
	runtimeDir := filepath.Join(dir, "run")
	for _, d := range []string{s.profile, runtimeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("ICEBATH_HOME", s.profile)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	swapSetup(t, &serviceGOOS, "linux")
	swapSetup(t, &serviceLookPath, func(name string) (string, error) { return "/fake/bin/" + name, nil })
	var lifecycle bytes.Buffer
	swapSetup(t, &lifecycleOut, io.Writer(&lifecycle))
	swapSetup(t, &RunServiceCommand, func(_ context.Context, _ time.Duration, argv ...string) (CommandResult, error) {
		args := argv[1:]
		s.calls = append(s.calls, append([]string{filepath.Base(argv[0])}, args...))
		has := func(a string) bool { return slices.Contains(args, a) }
		if has("enable") && s.fail {
			return CommandResult{ReturnCode: 1}, nil
		}
		if has("start") {
			s.active = true
		}
		out := ""
		switch {
		case has("is-active"):
			out = "inactive\n"
			if s.active {
				out = "active\n"
			}
		case has("show-user"):
			out = "yes\n"
		case has("show"):
			out = "LoadState=not-found\n"
		case has("is-system-running"):
			out = "running\n"
		}
		return CommandResult{Stdout: out}, nil
	})
	return s
}

// Whether any recorded operation carries one of the forbidden verbs
func (s *choiceSupervisor) ran(verbs ...string) bool {
	for _, op := range s.calls {
		for _, v := range verbs {
			if slices.Contains(op, v) {
				return true
			}
		}
	}
	return false
}

func TestExplicitSetupConsentIsDurableOnlyAfterSuccess(t *testing.T) {
	for _, tc := range []struct{ answer, fail bool }{{false, false}, {true, false}, {true, true}} {
		t.Run(map[bool]string{true: "yes", false: "no"}[tc.answer]+map[bool]string{true: "-fail", false: ""}[tc.fail], func(t *testing.T) {
			s := newChoiceSupervisor(t)
			s.fail = tc.fail
			ctx := context.Background()
			if err := config.SaveConfig(ctx, s.profile, map[string]any{"gateway": map[string]any{"service_install_choice": nil}, "custom": "keep"},
				config.SaveOptions{}); err != nil {
				t.Fatal(err)
			}
			var prompts []string
			swapSetup(t, &setupPromptConsent, func(_ context.Context, question string, _ bool) bool {
				prompts = append(prompts, question)
				return tc.answer
			})
			swapSetup(t, &setupStdinIsTTY, func() bool { return true })
			cfg, err := loadSetupConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			result, err := EnsureGatewayService(ctx, &out, ServiceSetupOptions{Interactive: true, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			var expected any
			if !tc.fail {
				expected = "decline"
				if tc.answer {
					expected = "install"
				}
			}
			loaded, err := loadSetupConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded["gateway"].(map[string]any)["service_install_choice"]; got != expected {
				t.Fatalf("saved choice %v, want %v\n%s", got, expected, out.String())
			}
			if got := cfg["gateway"].(map[string]any)["service_install_choice"]; got != expected {
				t.Fatalf("working choice %v, want %v", got, expected)
			}
			if loaded["custom"] != "keep" || result != (tc.answer && !tc.fail) || len(prompts) != 1 {
				t.Fatalf("custom %v result %v prompts %v\n%s", loaded["custom"], result, prompts, out.String())
			}
			if !tc.fail {
				if _, err := EnsureGatewayService(ctx, &out, ServiceSetupOptions{Interactive: true}); err != nil {
					t.Fatal(err)
				}
				if len(prompts) != 1 {
					t.Fatalf("asked again %v", prompts)
				}
			}
			if !tc.answer {
				unit, err := GetSystemdUnitPath(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(unit); err == nil || s.ran("enable", "enable-linger", "start", "daemon-reload") {
					t.Fatalf("declined install touched the supervisor %v", s.calls)
				}
			}
		})
	}
}

// Unmanaged spawn boundary recording gateway run launches instead of starting them
func recordGatewaySpawns(t *testing.T) *[][]string {
	t.Helper()
	var spawned [][]string
	real := gatewayPopen
	swapSetup(t, &gatewayPopen, func(cmd *exec.Cmd) (SpawnedGateway, error) {
		if slices.Contains(cmd.Args, "gateway") && slices.Contains(cmd.Args, "run") {
			spawned = append(spawned, cmd.Args)
			return pendingSpawn{}, nil
		}
		return real(cmd)
	})
	return &spawned
}

func TestWizardServiceChoiceControlsInstallation(t *testing.T) {
	for _, tc := range []struct{ startNow, consent bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("start_now=%v-consent=%v", tc.startNow, tc.consent), func(t *testing.T) {
			s := newChoiceSupervisor(t)
			ctx := context.Background()
			if err := config.SaveConfig(ctx, s.profile, map[string]any{"gateway": map[string]any{"service_install_choice": nil}},
				config.SaveOptions{}); err != nil {
				t.Fatal(err)
			}
			swapSetup(t, &setupStdinIsTTY, func() bool { return true })
			answers := []bool{tc.startNow, tc.consent}
			swapSetup(t, &setupPromptConsent, func(context.Context, string, bool) bool {
				answer := answers[0]
				answers = answers[1:]
				return answer
			})
			swapSetup(t, &gatewayFacade.promptChoice, func(string, []string, int) int { return 0 })
			spawned := recordGatewaySpawns(t)
			var out bytes.Buffer
			if err := wizardInstallService(ctx, &out, "systemd"); err != nil {
				t.Fatal(err)
			}
			unit, err := GetSystemdUnitPath(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(unit); (err == nil) != tc.consent {
				t.Fatalf("unit exists %v, consent %v\n%s", err == nil, tc.consent, out.String())
			}
			if (len(*spawned) > 0) != (tc.startNow && !tc.consent) {
				t.Fatalf("spawned %v\n%s", *spawned, out.String())
			}
			loaded, err := loadSetupConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := map[bool]string{true: "install", false: "decline"}[tc.consent]
			if got := loaded["gateway"].(map[string]any)["service_install_choice"]; got != want {
				t.Fatalf("choice %v want %v", got, want)
			}
			if len(*spawned) > 0 && slices.Contains((*spawned)[0], "--replace") {
				t.Fatalf("replace in %v", (*spawned)[0])
			}
		})
	}
}

func TestWizardStartDoesNotSpawnOverReservedOwner(t *testing.T) {
	s := newChoiceSupervisor(t)
	ctx := context.Background()
	if err := os.Chmod(s.profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveConfig(ctx, s.profile, map[string]any{"gateway": map[string]any{"service_install_choice": "decline"}},
		config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	swapSetup(t, &setupStdinIsTTY, func() bool { return true })
	swapSetup(t, &setupPromptConsent, func(context.Context, string, bool) bool { return true })
	spawned := recordGatewaySpawns(t)
	owner := NewProfileOwnership()
	if err := owner.Reserve(ctx, []string{s.profile}, ""); err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var out bytes.Buffer
	if err := wizardInstallService(ctx, &out, "systemd"); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 0 {
		t.Fatalf("spawned %v", *spawned)
	}
}

func TestDockerAwareGatewaySetupWizardUserScopeInContainerSkipsInstall(t *testing.T) {
	swapSetup(t, &setupIsContainer, func() bool { return true })
	swapSetup(t, &setupPromptInstallScope, func(io.Writer) string { return "user" })
	swapSetup(t, &setupSystemdInstall, func(context.Context, io.Writer, bool, bool, string, bool, bool, bool) error {
		t.Fatal("must not install a user unit in a container")
		return nil
	})
	var out bytes.Buffer
	scope, did, err := installLinuxGatewayFromSetup(context.Background(), &out, false, true)
	if err != nil || scope != "user" || did || !strings.Contains(out.String(), "--system") {
		t.Fatalf("scope %q did %v err %v\n%s", scope, did, err, out.String())
	}
}

func TestWizardInstallServiceReportsInstallerExitLikeSystemExitStr(t *testing.T) {
	swapSetup(t, &setupStdinIsTTY, func() bool { return true })
	swapSetup(t, &setupPromptConsent, func(context.Context, string, bool) bool { return false })
	swapSetup(t, &setupLoadConfig, func(context.Context) (map[string]any, error) {
		return map[string]any{"gateway": map[string]any{"service_install_choice": "install"}}, nil
	})
	swapSetup(t, &setupLaunchdInstall, func(context.Context, io.Writer, bool, bool, bool) error { return &SystemExitError{Code: 1} })
	var out bytes.Buffer
	if err := wizardInstallService(context.Background(), &out, "launchd"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Install failed: 1\n") || strings.Contains(out.String(), "exit 1") {
		t.Fatalf("output %q", out.String())
	}
}
