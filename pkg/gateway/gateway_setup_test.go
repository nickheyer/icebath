package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Facade prompt double answering the queued yes or no replies in order
func answerYesNo(t *testing.T, answers ...bool) {
	t.Helper()
	saved := gatewayFacade
	t.Cleanup(func() { gatewayFacade = saved })
	next := 0
	gatewayFacade.promptYesNo = func(string, bool) bool {
		if next >= len(answers) {
			t.Fatal("prompt_yes_no asked more often than the case answers")
		}
		answer := answers[next]
		next++
		return answer
	}
}

// Setup output captured into a buffer for the duration of the test
func captureSetupOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	return &out
}

func TestSetupWizardSkipsServiceInstallForProfileServedByMultiplexer(t *testing.T) {
	servedRoot(t)
	var calls []string
	swapSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return true })
	answerYesNo(t, true, true, true)
	swapSetup(t, &setupInstallLinuxScope, func(context.Context, io.Writer, bool, bool) (string, bool, error) {
		calls = append(calls, "install")
		return "", true, nil
	})
	swapSetup(t, &setupServiceActionFn, func(context.Context, io.Writer, string, string, bool, bool) error {
		calls = append(calls, "start")
		return nil
	})
	out := captureSetupOutput(t)
	if err := wizardPostSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || !strings.Contains(out.String(), "already served by the default multiplexer") {
		t.Fatalf("calls %v\n%s", calls, out.String())
	}
}

func TestWizardInstallServiceAsksOnceAndNeverStartsAfterWindowsInstall(t *testing.T) {
	answerYesNo(t, true, true)
	swapSetup(t, &hostIsWSL, func() bool { return false })
	var installs []WindowsInstallOptions
	swapSetup(t, &setupWindowsInstall, func(_ context.Context, o WindowsInstallOptions) error {
		installs = append(installs, o)
		return nil
	})
	var starts [][]any
	swapSetup(t, &setupServiceActionFn, func(_ context.Context, _ io.Writer, action, label string, windows, system bool) error {
		starts = append(starts, []any{action, label, windows, system})
		return nil
	})
	captureSetupOutput(t)
	if err := wizardSetupInstallService(context.Background(), "windows"); err != nil {
		t.Fatal(err)
	}
	if want := []WindowsInstallOptions{{Force: false, StartNow: optBool(true), StartOnLogin: optBool(true)}}; !reflect.DeepEqual(installs, want) {
		t.Fatalf("installs %+v", installs)
	}
	if len(starts) != 0 {
		t.Fatalf("starts %v", starts)
	}
}

// launchd backend with every launchctl call recorded instead of run, like the launchd fixture
type launchdWizardState struct {
	plist                 string
	bootstraps, launchctl [][]string
	refreshes             int
	starts                [][]any
}

func launchdWizardHost(t *testing.T) *launchdWizardState {
	t.Helper()
	const label, domain = "ai.icebath.gateway", "gui/501"
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	state := &launchdWizardState{plist: filepath.Join(tmp, "LaunchAgents", label+".plist")}
	keepInstallHooks(t)
	swapSetup(t, &serviceGOOS, "darwin")
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return false })
	swapSetup(t, &setupIsMacOS, func() bool { return true })
	swapSetup(t, &hostIsTermux, func() bool { return false })
	swapSetup(t, &launchdPlistPathOf, func(context.Context) (string, error) { return state.plist, nil })
	swapSetup(t, &launchdLabelOf, func(context.Context) (string, error) { return label, nil })
	swapSetup(t, &launchdDomainOf, func(context.Context) (string, error) { return domain, nil })
	installHooks.refuseTemp = func(string, string) bool { return false }
	installHooks.clearMarker = func(context.Context) {}
	installHooks.supervising = func(context.Context, string) bool { return false }
	installHooks.supervisedPID = func(context.Context, string) (int, bool) { return 0, false }
	installHooks.bootstrap = func(_ context.Context, d, p, l string, _ time.Duration) error {
		state.bootstraps = append(state.bootstraps, []string{d, p, l})
		return nil
	}
	installHooks.refreshPlist = func(context.Context) (bool, error) { state.refreshes++; return true, nil }
	swapSetup(t, &setupServiceActionFn, func(_ context.Context, _ io.Writer, action, failed string, windows, system bool) error {
		state.starts = append(state.starts, []any{action, failed, windows, system})
		return nil
	})
	swapSetup(t, &launchctlRun, func(_ context.Context, _ time.Duration, _ bool, argv ...string) (CommandResult, error) {
		state.launchctl = append(state.launchctl, argv)
		return CommandResult{}, nil
	})
	return state
}

func TestWizardNoToStartNowInstallsWithoutLoading(t *testing.T) {
	state := launchdWizardHost(t)
	answerYesNo(t, false, true)
	swapSetup(t, &hostIsWSL, func() bool { return false })
	captureSetupOutput(t)
	if err := wizardSetupInstallService(context.Background(), "launchd"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state.plist); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if len(state.bootstraps) != 0 || len(state.starts) != 0 {
		t.Fatalf("bootstraps %v starts %v", state.bootstraps, state.starts)
	}
}

func TestGatewaySetupWithNoPlatformProgressSkipsTheServiceOffer(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_MANAGED", "")
	swapSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return false })
	swapSetup(t, &setupNeedsNoService, func(context.Context, io.Writer) (bool, error) {
		t.Fatal("post setup reached without platform progress")
		return false, nil
	})
	saved := gatewayFacade
	t.Cleanup(func() { gatewayFacade = saved })
	var menus [][]string
	gatewayFacade.promptChoice = func(question string, choices []string, def int) int {
		if question != "Select a platform to configure:" || def != len(choices)-1 || choices[def] != "Done" {
			t.Fatalf("menu %q %v %d", question, choices, def)
		}
		menus = append(menus, choices)
		return def
	}
	var out bytes.Buffer
	if err := GatewaySetup(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	order := []string{"☤ Gateway Setup", "Gateway service is not installed yet.",
		"You'll be offered to install it after configuring platforms.", "Messaging Platforms",
		"No platforms configured. Run 'icebath gateway setup' when ready."}
	at := 0
	for _, want := range order {
		i := strings.Index(text[at:], want)
		if i < 0 {
			t.Fatalf("missing %q after offset %d in\n%s", want, at, text)
		}
		at += i + len(want)
	}
	if len(menus) != 1 || !strings.HasSuffix(menus[0][0], "(not configured)") {
		t.Fatalf("menus %v", menus)
	}
}
