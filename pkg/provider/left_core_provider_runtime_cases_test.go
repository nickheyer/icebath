package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

const leftCoreAgentStartEnv = "ICEBATH_TEST_LEFT_CORE_AGENT_START"

// Runs a fresh process's agent start, printing every delivered notice, when the child env flag is set
func dispatchLeftCoreAgentStartEntry() {
	if os.Getenv(leftCoreAgentStartEnv) != "1" {
		return
	}
	RecoverLeftCoreAtStartup(context.Background(), func(message string) { fmt.Println("NOTICE", message) }, false)
	os.Exit(0)
}

// What a fresh process's agent start delivers for the bound process home
func agentStartInAnotherProcess(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), leftCoreAgentStartEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(out))
	}
	var lines []string
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if strings.HasPrefix(line, "NOTICE") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "")
}

// Fresh attempt memo, no terminal front end, and none of the copilot env names in the process environment
func leftCoreRuntimeFresh(t *testing.T) {
	t.Helper()
	leftCoreRowsFresh(t)
	prev := leftCoreTerminalFrontEnd.Load()
	leftCoreTerminalFrontEnd.Store(false)
	t.Cleanup(func() { leftCoreTerminalFrontEnd.Store(prev) })
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
}

// Home with the config bound as the process home, the lazy install policy left to its config
func copilotHome(t *testing.T, cfg string) (string, context.Context) {
	t.Helper()
	h := leftCoreHome(t, t.TempDir(), "home", "", cfg)
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	return h, context.Background()
}

// Catalog installs recorded as name and consent pairs, every one failing at the catalog repo
func failingInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(_ context.Context, name string, consent bool) (map[string]any, error) {
			*calls = append(*calls, fmt.Sprintf("%s:%v", name, consent))
			return map[string]any{"ok": false, "error": "fatal: unable to access the catalog repo"}, nil
		}
	})
	return calls
}

// Catalog installs that leave the plugin dir in the process home, recorded by name
func okInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(ctx context.Context, name string, _ bool) (map[string]any, error) {
			*calls = append(*calls, name)
			mkPluginDir(t, home.Dir(ctx), name)
			return map[string]any{"ok": true}, nil
		}
	})
	return calls
}

// Resolves the runtime provider expecting the unknown provider auth error
func resolveExpectingAuthError(t *testing.T, ctx context.Context, h, requested string) {
	t.Helper()
	_, err := ResolveRuntimeProvider(ctx, h, requested, "", "", "")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("resolve error %T %v", err, err)
	}
}

// Text written to the process stderr while fn runs
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	fn()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRuntimeResolutionInstallsOncePerProcessAndHonoursTheBackoff(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	installs := failingInstalls(t)
	for i := 0; i < 3; i++ {
		resolveExpectingAuthError(t, ctx, h, "")
	}
	if strings.Join(*installs, ",") != "github-copilot:true" {
		t.Fatalf("installs %v", *installs)
	}
	clearLeftCoreAttempted()
	resolveExpectingAuthError(t, ctx, h, "")
	if len(*installs) != 1 {
		t.Fatalf("a second process inside the backoff installed, %v", *installs)
	}
}

func TestRuntimeResolutionWithoutLazyInstallsFetchesNothing(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\nsecurity:\n  allow_lazy_installs: false\n")
	installs := failingInstalls(t)
	resolveExpectingAuthError(t, ctx, h, "")
	if len(*installs) != 0 {
		t.Fatalf("installs %v", *installs)
	}
}

func TestAFailedAttemptIsNotRecheckedOnEveryResolution(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	failingInstalls(t)
	rows := LeftCore()
	calls := 0
	for i := range rows {
		if rows[i].Plugin == "github-copilot" {
			rows[i].InUse = func(h string, processEnv bool) bool {
				calls++
				return copilotInUse(h, processEnv)
			}
		}
	}
	t.Cleanup(SwapLeftCoreTableForTests(rows))
	recoverLeftCoreProvider(ctx, h, "copilot")
	first := calls
	for i := 0; i < 5; i++ {
		recoverLeftCoreProvider(ctx, h, "copilot")
	}
	if calls != first {
		t.Fatalf("predicate ran %d times after the attempt, %d before", calls-first, first)
	}
}

func TestAnAutoRouteFollowingTheStoredActiveProviderInstallsIt(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: auto\n")
	installs := failingInstalls(t)
	writeLeftCoreFile(t, filepath.Join(h, "auth.json"), `{"active_provider": "copilot"}`)
	t.Setenv("COPILOT_GITHUB_TOKEN", "gho_fixture")
	ResolveRuntimeProvider(ctx, h, "", "", "", "")
	if strings.Join(*installs, ",") != "github-copilot:true" {
		t.Fatalf("installs %v", *installs)
	}
}

func TestARouteConfiguredOutsideTheMainModelInstallsWhenResolved(t *testing.T) {
	for _, cfg := range []string{
		"model:\n  provider: nous\ncron:\n  model_provider: copilot\n",
		"model:\n  provider: nous\ndelegation:\n  backends:\n    model_map:\n      options:\n" +
			"        - name: fast\n          provider: copilot\n          model: gpt-5.4\n",
	} {
		leftCoreRuntimeFresh(t)
		h, ctx := copilotHome(t, cfg)
		installs := failingInstalls(t)
		resolveExpectingAuthError(t, ctx, h, "copilot")
		if strings.Join(*installs, ",") != "github-copilot:true" {
			t.Fatalf("%q installs %v", cfg, *installs)
		}
	}
}

func TestRepeatedStartsKeepOneCopyOfTheSameNotice(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\nsecurity:\n  allow_lazy_installs: false\n")
	failingInstalls(t)
	for i := 0; i < 3; i++ {
		clearLeftCoreAttempted()
		RecoverLeftCoreAtStartup(ctx, nil, false)
	}
	data, err := os.ReadFile(leftCoreNoticeFile(h))
	if err != nil || len(strings.Split(strings.TrimRight(string(data), "\n"), "\n")) != 1 {
		t.Fatalf("kept %q %v", data, err)
	}
	clearLeftCoreAttempted()
	var said []string
	RecoverLeftCoreAtStartup(ctx, func(m string) { said = append(said, m) }, false)
	if len(said) != 1 || !strings.Contains(said[0], "allow_lazy_installs is off") {
		t.Fatalf("said %q", said)
	}
}

func TestAResolutionOutcomeReachesTheFirstAgentOfAnyProcessOnce(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	installs := okInstalls(t)
	prev := interactiveTerminal
	interactiveTerminal = func() bool { return false }
	t.Cleanup(func() { interactiveTerminal = prev })
	errText := captureStderr(t, func() { recoverLeftCoreProvider(ctx, h, "copilot") })
	if strings.Join(*installs, ",") != "github-copilot" || errText != "" {
		t.Fatalf("installs %v stderr %q", *installs, errText)
	}
	first, second := agentStartInAnotherProcess(t), agentStartInAnotherProcess(t)
	if strings.Count(first, "GitHub Copilot moved out of core") != 1 || !strings.Contains(first, "run `icebath auth add copilot`") {
		t.Fatalf("first %q", first)
	}
	if strings.Contains(second, "moved out of core") {
		t.Fatalf("second %q", second)
	}
}

func TestATTYWithoutADeclaredFrontEndKeepsTheOutcomeForTheChatAgent(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	installs := okInstalls(t)
	prev := interactiveTerminal
	interactiveTerminal = func() bool { return true }
	t.Cleanup(func() { interactiveTerminal = prev })
	errText := captureStderr(t, func() { recoverLeftCoreProvider(ctx, h, "copilot") })
	if strings.Join(*installs, ",") != "github-copilot" || errText != "" {
		t.Fatalf("installs %v stderr %q", *installs, errText)
	}
	chat := agentStartInAnotherProcess(t)
	if strings.Count(chat, "GitHub Copilot moved out of core") != 1 || !strings.Contains(chat, "icebath auth add copilot") {
		t.Fatalf("chat %q", chat)
	}
}

func TestAPipedCLIFrontEndPrintsTheOutcomeAndWhatAnotherProcessKept(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	okInstalls(t)
	prev := interactiveTerminal
	interactiveTerminal = func() bool { return false }
	t.Cleanup(func() { interactiveTerminal = prev })
	queueLeftCoreNotice(h, "kept by the gateway's start")
	ServeNoticesOnStderr()
	errText := captureStderr(t, func() {
		recoverLeftCoreProvider(ctx, h, "copilot")
		recoverLeftCoreProvider(ctx, h, "copilot")
	})
	if strings.Count(errText, "kept by the gateway's start") != 1 || strings.Count(errText, "GitHub Copilot moved out of core") != 1 {
		t.Fatalf("stderr %q", errText)
	}
	if out := agentStartInAnotherProcess(t); strings.Contains(out, "moved out of core") {
		t.Fatalf("another process %q", out)
	}
}

func TestAProviderInstalledDuringResolutionResolvesInThatSameCall(t *testing.T) {
	leftCoreRuntimeFresh(t)
	h, ctx := copilotHome(t, "model:\n  provider: copilot\n")
	t.Setenv("COPILOT_GITHUB_TOKEN", "stand-in-token")
	snapshotProviderRegistry(t)
	fakeExternalProviderPlugins(t, map[string]func(homeKey string) ([]Profile, error){
		"github-copilot": func(string) ([]Profile, error) {
			return []Profile{{Name: "copilot", DisplayName: "Stand-in", AuthType: "api_key", KeyEnv: []string{"COPILOT_GITHUB_TOKEN"},
				BaseURL: "https://copilot.stand-in.test/v1"}}, nil
		},
	})
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(context.Context, string, bool) (map[string]any, error) {
			plugin := filepath.Join(h, "plugins", "github-copilot")
			writeLeftCoreFile(t, filepath.Join(plugin, "plugin.yaml"), "name: github-copilot\nkind: model-provider\n")
			writeLeftCoreFile(t, filepath.Join(plugin, "__init__.py"), "from providers import register_provider\n")
			return map[string]any{"ok": true}, nil
		}
	})
	runtime, err := ResolveRuntimeProvider(ctx, h, "", "", "", "")
	if err != nil || runtime.Provider != "copilot" || runtime.BaseURL != "https://copilot.stand-in.test/v1" {
		t.Fatalf("runtime %+v %v", runtime, err)
	}
}
