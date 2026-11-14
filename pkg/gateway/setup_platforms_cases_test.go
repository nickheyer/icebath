package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registers the IRC plugin entry as discovery would and returns its setup row, unregistering it after the test
func registerIRCPlatform(t *testing.T, setupFn func(context.Context) error) SetupPlatform {
	t.Helper()
	ctx := context.Background()
	entry := NewPlatformEntry("irc", "IRC", func(context.Context, *PlatformConfig) (any, error) { return nil, nil },
		func() bool { return os.Getenv("IRC_SERVER") != "" && os.Getenv("IRC_CHANNEL") != "" })
	entry.RequiredEnv = []string{"IRC_SERVER", "IRC_CHANNEL", "IRC_NICKNAME"}
	entry.InstallHint = "No extra packages needed (stdlib only)"
	entry.SetupFn = setupFn
	entry.PluginName = "irc_platform"
	entry.AllowedUsersEnv, entry.AllowAllEnv = "IRC_ALLOWED_USERS", "IRC_ALLOW_ALL_USERS"
	entry.MaxMessageLength, entry.PIISafe, entry.Emoji = 450, false, "💬"
	entry.AllowUpdateCommand, entry.PlatformHint = true, "You are chatting via IRC."
	if err := Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Registry.Unregister(ctx, "irc", "") })
	return SetupPlatform{Key: entry.Name, Label: entry.Label, Emoji: entry.Emoji, TokenVar: entry.RequiredEnv[0],
		InstallHint: entry.InstallHint, Entry: entry}
}

// Captured setup console over a temp home with the service manager probes faked off like the IRC cases patch them
func ircSetupHarness(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	savedSetup := WizardSetup
	t.Cleanup(func() { WizardSetup = savedSetup })
	WizardSetup.PromptYesNo = func(string, bool) bool { return false }
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { return false })
	swapSetup(t, &setupIsMacOS, func() bool { return false })
	swapSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) { return false, nil })
	swapSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { return false, nil })
	return &out
}

func TestIRCFreshInstallDiscoveryIrcAppearsInAllPlatforms(t *testing.T) {
	registerIRCPlatform(t, func(context.Context) error { return nil })
	for _, key := range []string{"IRC_SERVER", "IRC_CHANNEL", "IRC_NICKNAME"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	found := false
	for _, p := range AllPlatforms(context.Background()) {
		found = found || p.Key == "irc"
	}
	if !found {
		t.Fatal("irc missing from the setup platforms")
	}
}

func TestIRCFreshInstallDiscoveryIrcStatusConfiguredWhenEnvSet(t *testing.T) {
	plat := registerIRCPlatform(t, func(context.Context) error { return nil })
	t.Setenv("IRC_SERVER", "irc.libera.chat")
	t.Setenv("IRC_CHANNEL", "#hermes")
	t.Setenv("IRC_NICKNAME", "hermes-bot")
	status, err := PlatformStatus(context.Background(), plat)
	if err != nil || status != "configured" {
		t.Fatalf("status %q %v", status, err)
	}
}

func TestIRCInteractiveSetupConfigurePlatformDispatchesToIrcSetupFn(t *testing.T) {
	out := ircSetupHarness(t)
	var calls []string
	plat := registerIRCPlatform(t, func(context.Context) error {
		calls = append(calls, "setup_called")
		setupPrintln("IRC setup complete!")
		return nil
	})
	if err := configurePlatform(context.Background(), plat); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "setup_called" || !strings.Contains(out.String(), "IRC setup complete!") {
		t.Fatalf("calls %v output %q", calls, out.String())
	}
}

func TestIRCInteractiveSetupConfigurePlatformFallbackWhenNoSetupFn(t *testing.T) {
	out := ircSetupHarness(t)
	plat := registerIRCPlatform(t, nil)
	if err := configurePlatform(context.Background(), plat); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, "IRC") || !strings.Contains(text, "IRC_SERVER") {
		t.Fatalf("output %q", text)
	}
}

func TestIRCGatewaySetupFreshInstallSetupGatewayShowsIrcInPlatformMenu(t *testing.T) {
	ircSetupHarness(t)
	registerIRCPlatform(t, func(context.Context) error { return nil })
	for _, key := range []string{"IRC_SERVER", "IRC_CHANNEL", "IRC_NICKNAME"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	found := false
	for _, p := range AllPlatforms(context.Background()) {
		found = found || p.Key == "irc"
	}
	if !found {
		t.Fatal("IRC not in platforms")
	}
	type checklistCall struct {
		question string
		choices  []string
	}
	var checklistCalls []checklistCall
	WizardSetup.PromptChecklist = func(question string, choices []string, _ []int) []int {
		checklistCalls = append(checklistCalls, checklistCall{question, append([]string{}, choices...)})
		return nil
	}
	if err := SetupGateway(context.Background(), config.Tree{}); err != nil {
		t.Fatal(err)
	}
	var prompt *checklistCall
	for i := range checklistCalls {
		if strings.Contains(strings.ToLower(checklistCalls[i].question), "platform") {
			prompt = &checklistCalls[i]
			break
		}
	}
	if prompt == nil {
		t.Fatalf("no platform prompt found in %v", checklistCalls)
	}
	text := strings.Join(prompt.choices, "\n")
	if !strings.Contains(text, "IRC") || !strings.Contains(strings.ToLower(text), "not configured") {
		t.Fatalf("choices %q", text)
	}
}

func TestIRCGatewaySetupFreshInstallSetupGatewayIrcCountsAsMessagingPlatform(t *testing.T) {
	out := ircSetupHarness(t)
	registerIRCPlatform(t, func(context.Context) error { return nil })
	t.Setenv("IRC_SERVER", "irc.libera.chat")
	t.Setenv("IRC_CHANNEL", "#hermes")
	t.Setenv("IRC_NICKNAME", "hermes-bot")
	WizardSetup.PromptChoice = func(string, []string, int) int { return 0 }
	WizardSetup.PromptChecklist = func(_ string, items []string, _ []int) []int {
		var picked []int
		for i, item := range items {
			if strings.Contains(item, "IRC") {
				picked = append(picked, i)
			}
		}
		return picked
	}
	if err := SetupGateway(context.Background(), config.Tree{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Messaging platforms configured!") {
		t.Fatalf("output %q", out.String())
	}
}

// Unsets env vars for the test, restoring them after, since a real save_env_value also exports the value
func isolateEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// Scripted standard input for the real setup prompts, password prompts reading it the way getpass falls back
func withSetupStdin(t *testing.T, text string) *bytes.Buffer {
	t.Helper()
	isolateEnv(t, "MATTERMOST_URL", "MATTERMOST_TOKEN", "MATTERMOST_ALLOWED_USERS", "MATTERMOST_HOME_CHANNEL", "MATTERMOST_REPLY_MODE",
		"GATEWAY_ALLOW_ALL_USERS")
	def := cliui.Default()
	in := strings.NewReader(text)
	savedIn, savedTTY, savedSecret := def.In, def.StdinTTY, def.MaskedSecretPrompt
	def.In, def.StdinTTY = in, func() bool { return false }
	def.MaskedSecretPrompt = func(prompt string) (string, error) { return (&cliui.Console{In: in, Out: io.Discard}).Input(prompt) }
	t.Cleanup(func() { def.In, def.StdinTTY, def.MaskedSecretPrompt = savedIn, savedTTY, savedSecret })
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	return &out
}

func TestGatewaySetupPointsAtADocumentedUserId(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := withSetupStdin(t, "https://mm.example.com\ntok-abc\nuserid26\n")
	if err := setupStandardPlatform(context.Background(), setupPlatformsTable[0]); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "User Management") || !strings.Contains(text, "/api/v4/users/username/") ||
		strings.Contains(text, "click your avatar") || strings.Contains(text, "displayed there") {
		t.Fatalf("output %q", text)
	}
}

// The KEY=VALUE pairs of the home .env
func readHomeEnv(t *testing.T, h string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, ".env"))
	if err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
	}
	return out
}

func TestCliWizardWizardStopsAskingAboutHiddenKnobs(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	out := withSetupStdin(t, strings.Join([]string{"https://mm.example.com", "tok-abc", "user26charid"}, "\n")+"\n")
	if err := setupStandardPlatform(context.Background(), setupPlatformsTable[0]); err != nil {
		t.Fatal(err)
	}
	written := readHomeEnv(t, h)
	if written["MATTERMOST_URL"] != "https://mm.example.com" || written["MATTERMOST_TOKEN"] != "tok-abc" {
		t.Fatalf("env %v output %q", written, out.String())
	}
	if _, ok := written["MATTERMOST_HOME_CHANNEL"]; ok {
		t.Fatalf("home channel asked %v", written)
	}
	if _, ok := written["MATTERMOST_REPLY_MODE"]; ok {
		t.Fatalf("reply mode asked %v", written)
	}
	if text := out.String(); strings.Contains(text, "Home channel") || strings.Contains(text, "Reply mode") {
		t.Fatalf("output %q", text)
	}
}

func TestCliWizardRequiredTokenStillGatesSetup(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	withSetupStdin(t, "https://mm.example.com\n\n")
	if err := setupStandardPlatform(context.Background(), setupPlatformsTable[0]); err != nil {
		t.Fatal(err)
	}
	if _, ok := readHomeEnv(t, h)["MATTERMOST_TOKEN"]; ok {
		t.Fatal("token saved without an answer")
	}
}

func TestAllPlatformsHidesMatrixOnAWindowsHost(t *testing.T) {
	ctx := context.Background()
	entry := NewPlatformEntry("matrix", "Matrix", nil, func() bool { return false })
	if err := Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Registry.Unregister(ctx, "matrix", "") })
	saved := gatewayFacade.windowsHost
	t.Cleanup(func() { gatewayFacade.windowsHost = saved })
	for _, windows := range []bool{false, true} {
		gatewayFacade.windowsHost = func() bool { return windows }
		listed := false
		for _, p := range AllPlatforms(ctx) {
			listed = listed || p.Key == "matrix"
		}
		if listed == windows {
			t.Fatalf("windows %v listed matrix %v", windows, listed)
		}
	}
}
