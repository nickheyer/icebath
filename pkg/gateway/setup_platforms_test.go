package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// One recorded hermes run of a platform setup function with its scripted answers and observed effects
type platformScreen struct {
	Name       string            `json:"name"`
	Call       string            `json:"call"`
	Args       []any             `json:"args"`
	Registry   []screenEntry     `json:"registry"`
	Configured map[string]bool   `json:"configured"`
	Env        map[string]string `json:"env"`
	Service    map[string]any    `json:"service"`
	HermesHome string            `json:"hermes_home"`
	Auto       *struct {
		Token       string `json:"token"`
		OwnerUserID *int64 `json:"owner_user_id"`
	} `json:"auto"`
	Platform map[string]string `json:"platform"`
	Answers  struct {
		Prompt    []string `json:"prompt"`
		YesNo     []bool   `json:"yes_no"`
		Choice    []int    `json:"choice"`
		Checklist [][]int  `json:"checklist"`
	} `json:"answers"`
	Want struct {
		Stdout string            `json:"stdout"`
		Calls  []any             `json:"calls"`
		Env    map[string]string `json:"env"`
		Result any               `json:"result"`
		Error  *string           `json:"error"`
	} `json:"want"`
}

// Registry plugin entry of a recorded scenario
type screenEntry struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Emoji       string   `json:"emoji"`
	RequiredEnv []string `json:"required_env"`
	InstallHint string   `json:"install_hint"`
	SetupFn     bool     `json:"setup_fn"`
}

// Hermes spellings of the recording mapped to icebath
var platformScreenRenames = strings.NewReplacer("hermes ", "icebath ", "Hermes ", "Icebath ", "~/.hermes", "~/.icebath",
	"(e.g. hermes)", "(e.g. icebath)", "'hermes ", "'icebath ")

var hermesServiceName = regexp.MustCompile(`hermes-gateway-[0-9a-f]{8}`)

// Built in forms of the recording, the mattermost table row and three synthetic forms
func screenForms() map[string]SetupPlatform {
	return map[string]SetupPlatform{
		"mattermost": setupPlatformsTable[0],
		"telegram_form": {Key: "telegram", Label: "Telegram", Emoji: "📱", TokenVar: "TELEGRAM_BOT_TOKEN", Vars: []SetupVar{
			{Name: "TELEGRAM_BOT_TOKEN", Prompt: "Bot token", Password: true, Help: "Paste the bot token."},
			{Name: "TELEGRAM_ALLOWED_USERS", Prompt: "Allowed user IDs (comma-separated)", IsAllowlist: true, Help: "Numeric user IDs."},
			{Name: "TELEGRAM_HOME_CHANNEL", Prompt: "Home channel", Help: "Hidden knob."}}},
		"discord_form": {Key: "discord", Label: "Discord", Emoji: "🎮", TokenVar: "DISCORD_BOT_TOKEN", Vars: []SetupVar{
			{Name: "DISCORD_BOT_TOKEN", Prompt: "Bot token", Password: true, Help: "Discord bot token."},
			{Name: "DISCORD_ALLOWED_USERS", Prompt: "Allowed user IDs", IsAllowlist: true, Help: "Discord user IDs."}}},
		"email_form": {Key: "email", Label: "Email", Emoji: "📧", TokenVar: "EMAIL_ADDRESS", Vars: []SetupVar{
			{Name: "EMAIL_ADDRESS", Prompt: "Address", Help: "Mailbox address."},
			{Name: "EMAIL_ALLOWED_USERS", Prompt: "Allowed senders", IsAllowlist: true, Help: "Sender addresses."}}},
	}
}

// Swaps a package seam for one test
func swapSetup[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Temp home and the captured setup console of one platform setup run
func platformSetupHome(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	if err := os.MkdirAll(filepath.Join(tmp, ".icebath"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	swapSetup(t, &Registry, NewPlatformRegistry())
	return context.Background(), &out
}

// Installs fakes answering from a recorded scenario and recording every call the hermes fakes recorded
func installPlatformScreenFakes(t *testing.T, ctx context.Context, sc *platformScreen) (*[]any, map[string]string) {
	t.Helper()
	calls := &[]any{}
	record := func(entry ...any) { *calls = append(*calls, entry) }
	env := map[string]string{}
	for k, v := range sc.Env {
		env[k] = v
	}
	prompts, yesNo, choices, checklists := sc.Answers.Prompt, sc.Answers.YesNo, sc.Answers.Choice, sc.Answers.Checklist
	nextPrompt := func() string {
		if len(prompts) == 0 {
			t.Fatal("unexpected prompt")
		}
		v := prompts[0]
		prompts = prompts[1:]
		return v
	}
	nextYesNo := func() bool {
		if len(yesNo) == 0 {
			t.Fatal("unexpected yes or no")
		}
		v := yesNo[0]
		yesNo = yesNo[1:]
		return v
	}
	savedSetup, savedFacade := WizardSetup, gatewayFacade
	t.Cleanup(func() { WizardSetup, gatewayFacade = savedSetup, savedFacade })
	WizardSetup.Prompt = func(q, def string, password bool) string {
		record("setup.prompt", q, orNone(def), password)
		return nextPrompt()
	}
	WizardSetup.PromptYesNo = func(q string, def bool) bool {
		record("setup.prompt_yes_no", q, def)
		return nextYesNo()
	}
	WizardSetup.PromptChecklist = func(title string, items []string, pre []int) []int {
		record("setup.prompt_checklist", title, append([]string{}, items...), append([]int{}, pre...))
		v := checklists[0]
		checklists = checklists[1:]
		return v
	}
	WizardSetup.GetEnvValue = func(_ context.Context, key string) (string, error) {
		record("setup.get_env_value", key)
		return env[key], nil
	}
	WizardSetup.SaveEnvValue = func(_ context.Context, key, value string) error {
		record("setup.save_env_value", key, value)
		env[key] = value
		return nil
	}
	homeDir := filepath.Join(os.Getenv("ICEBATH_HOME"))
	if sc.HermesHome != "" {
		homeDir = filepath.Join(homeDir, filepath.FromSlash(sc.HermesHome))
	}
	WizardSetup.HermesHome = func(context.Context) string { return homeDir }
	gatewayFacade.prompt = func(q, def string, password bool) string {
		record("gateway.prompt", q, orNone(def), password)
		return nextPrompt()
	}
	gatewayFacade.promptYesNo = func(q string, def bool) bool {
		record("gateway.prompt_yes_no", q, def)
		return nextYesNo()
	}
	gatewayFacade.promptChoice = func(q string, items []string, def int) int {
		record("gateway.prompt_choice", q, append([]string{}, items...), def)
		v := choices[0]
		choices = choices[1:]
		return v
	}
	gatewayFacade.getEnvValue = func(_ context.Context, key string) (string, error) {
		record("gateway.get_env_value", key)
		return env[key], nil
	}
	gatewayFacade.saveEnvValue = func(_ context.Context, key, value string) error {
		record("gateway.save_env_value", key, value)
		env[key] = value
		return nil
	}
	gatewayFacade.writePlatformConfigField = func(_ context.Context, platformKey, fieldKey string, value any, raw bool) error {
		record("gateway.write_platform_config_field", platformKey, fieldKey, value, raw)
		return nil
	}
	swapSetup(t, &telegramAutoSetupFn, func(_ context.Context, profileName *string) (*TelegramBotSetupResult, error) {
		var name any
		if profileName != nil {
			name = *profileName
		}
		record("auto_setup_telegram_bot_result", name)
		if sc.Auto == nil {
			return nil, nil
		}
		owner := ""
		if sc.Auto.OwnerUserID != nil {
			owner = fmt.Sprint(*sc.Auto.OwnerUserID)
		}
		return &TelegramBotSetupResult{Token: sc.Auto.Token, OwnerUserID: owner}, nil
	})
	flag := func(name string) bool {
		v, _ := sc.Service[name].(bool)
		return v
	}
	swapSetup(t, &setupIsContainer, func() bool { record("service.is_container"); return flag("container") })
	swapSetup(t, &setupSupportsSystemd, func(context.Context) bool { record("service.supports_systemd_services"); return flag("systemd") })
	swapSetup(t, &setupIsMacOS, func() bool { record("service.is_macos"); return false })
	swapSetup(t, &setupIsWindows, func() bool { record("service.is_windows"); return false })
	swapSetup(t, &setupServiceRunning, func(context.Context) (bool, error) { record("service.is_service_running"); return flag("running"), nil })
	swapSetup(t, &setupServiceInstalled, func(context.Context) (bool, error) {
		record("service.is_service_installed")
		return flag("installed"), nil
	})
	swapSetup(t, &setupNeedsNoService, func(context.Context, io.Writer) (bool, error) {
		record("service.served_profile_needs_no_service")
		return false, nil
	})
	swapSetup(t, &setupConflictingUnits, func(context.Context) (bool, error) {
		record("service.has_conflicting_systemd_units")
		return false, nil
	})
	swapSetup(t, &setupScopeNeedsRoot, func(context.Context, bool) (bool, error) {
		record("service.system_scope_would_need_root")
		return flag("needs_root"), nil
	})
	swapSetup(t, &setupSystemdStart, func(context.Context, bool) error { record("service.systemd_start"); return nil })
	restartErr := func(name string) error {
		record("service." + name)
		switch sc.Service["restart_error"] {
		case nil:
			return nil
		case "user_systemd":
			return &UserSystemdUnavailableError{Msg: "user bus missing\n  run: loginctl enable-linger me"}
		case "needs_root":
			return &SystemScopeRequiresRootError{Msg: "System gateway restart requires root. Re-run with sudo.", Action: "restart"}
		default:
			return errors.New(sc.Service["restart_error"].(string))
		}
	}
	swapSetup(t, &setupSystemdRestart, func(context.Context, bool) error { return restartErr("systemd_restart") })
	swapSetup(t, &setupLaunchdRestart, func(context.Context) error { return restartErr("launchd_restart") })
	swapSetup(t, &setupWindowsRestart, func(context.Context) error { return restartErr("windows_restart") })
	platform := "Linux"
	if p, ok := sc.Service["platform"].(string); ok {
		platform = p
	}
	swapSetup(t, &setupPlatformSystem, func() string { return platform })
	swapSetup(t, &setupStdinIsTTY, func() bool { return false })
	for _, r := range sc.Registry {
		r := r
		entry := NewPlatformEntry(r.Name, r.Label, nil, func() bool {
			record(r.Name + ".check_fn")
			return sc.Configured[r.Name]
		})
		entry.RequiredEnv, entry.InstallHint, entry.Emoji, entry.PluginName = append([]string{}, r.RequiredEnv...), r.InstallHint, r.Emoji, r.Name+"_platform"
		if r.SetupFn {
			entry.SetupFn = func(context.Context) error {
				record(r.Name + ".setup_fn")
				setupPrintln(r.Name + " setup complete!")
				return nil
			}
		}
		if err := Registry.Register(ctx, entry, ""); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if len(prompts)+len(yesNo)+len(choices)+len(checklists) > 0 {
			t.Errorf("unused answers %v %v %v %v", prompts, yesNo, choices, checklists)
		}
	})
	return calls, env
}

// Text or nil, the None a hermes fake records for an omitted default
func orNone(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// JSON round trip so recorded and live values compare as decoded JSON
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Applies the icebath renames to every string of a decoded JSON value
func renamedJSON(v any) any {
	switch x := v.(type) {
	case string:
		return platformScreenRenames.Replace(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = renamedJSON(item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range x {
			out[k] = renamedJSON(item)
		}
		return out
	}
	return v
}

// Runs the recorded entry point of a scenario
func runPlatformScreen(ctx context.Context, sc *platformScreen) (any, error) {
	platform := func() SetupPlatform {
		if form, ok := sc.Platform["form"]; ok {
			return screenForms()[form]
		}
		for _, p := range AllPlatforms(ctx) {
			if p.Key == sc.Platform["registry"] {
				return p
			}
		}
		return SetupPlatform{}
	}
	args := func() []string {
		out := make([]string, 0, len(sc.Args))
		for _, a := range sc.Args {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	switch sc.Call {
	case "setup_gateway":
		return nil, SetupGateway(ctx, config.Tree{})
	case "restart":
		return nil, restartRunningGateway(ctx, sc.Args[0].(bool), sc.Args[1].(bool))
	case "setup_telegram":
		return nil, setupTelegram(ctx)
	case "setup_webhooks":
		return nil, setupWebhooks(ctx)
	case "warn_missing":
		return nil, warnMissingHomeChannels(ctx)
	case "configure":
		return nil, configurePlatform(ctx, platform())
	case "standard":
		return nil, setupStandardPlatform(ctx, platform())
	case "declines":
		a := args()
		return PlatformSetup.DeclinesReconfigure(ctx, a[0], a[1], a[2:]...)
	case "save_prompted":
		kw, _ := sc.Args[2].(map[string]any)
		o := SavePromptedOptions{}
		o.Password, _ = kw["password"].(bool)
		o.SuccessMsg, _ = kw["success_msg"].(string)
		o.SkipMsg, _ = kw["skip_msg"].(string)
		return PlatformSetup.SavePrompted(ctx, sc.Args[0].(string), sc.Args[1].(string), o)
	}
	return nil, fmt.Errorf("unknown call %s", sc.Call)
}

func TestSetupPlatformsScreensMatchHermes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "setup_platforms_screens.json"))
	if err != nil {
		t.Fatal(err)
	}
	var screens []platformScreen
	if err := json.Unmarshal(raw, &screens); err != nil {
		t.Fatal(err)
	}
	if len(screens) != 58 {
		t.Fatalf("%d recorded screens", len(screens))
	}
	for i := range screens {
		sc := &screens[i]
		t.Run(sc.Name, func(t *testing.T) {
			ctx, out := platformSetupHome(t)
			calls, env := installPlatformScreenFakes(t, ctx, sc)
			result, err := runPlatformScreen(ctx, sc)
			if sc.Want.Error != nil {
				if err == nil || err.Error() != *sc.Want.Error {
					t.Fatalf("error %v, want %s", err, *sc.Want.Error)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			name, err := GetServiceName(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := hermesServiceName.ReplaceAllString(platformScreenRenames.Replace(sc.Want.Stdout), name)
			if out.String() != want {
				t.Fatalf("stdout\n%q\nwant\n%q", out.String(), want)
			}
			if got, want := asJSON(t, *calls), renamedJSON(asJSON(t, sc.Want.Calls)); !reflect.DeepEqual(got, want) {
				t.Fatalf("calls\n%v\nwant\n%v", got, want)
			}
			if !reflect.DeepEqual(asJSON(t, env), asJSON(t, sc.Want.Env)) {
				t.Fatalf("env %v, want %v", env, sc.Want.Env)
			}
			if !reflect.DeepEqual(asJSON(t, result), asJSON(t, sc.Want.Result)) {
				t.Fatalf("result %v, want %v", result, sc.Want.Result)
			}
		})
	}
}
