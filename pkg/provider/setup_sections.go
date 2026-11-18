package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Help command the provider setup screen points to
const SetupProviderHelp = "icebath model --help"

var setupLog = logging.GetLogger("hermes_cli.setup")

// Seam over the shared provider picker, the hermes_cli.main.select_provider_and_model the setup tests patch
var setupSelectProviderAndModel = SelectProviderAndModel

// SetupAttributeError is the AttributeError a setup section raises on a config value of the wrong type
type SetupAttributeError struct{ Msg string }

func (e *SetupAttributeError) Error() string { return "AttributeError: " + e.Msg }

// SetupTypeError is the TypeError a setup section raises assigning into a config value that is not a mapping
type SetupTypeError struct{ Msg string }

func (e *SetupTypeError) Error() string { return "TypeError: " + e.Msg }

// SetdefaultDict is cfg.setdefault(key, {}) followed by an item assignment, a TypeError when the value is not a mapping
func SetdefaultDict(cfg map[string]any, key string) (map[string]any, error) {
	v, ok := cfg[key]
	if !ok {
		section := map[string]any{}
		cfg[key] = section
		return section, nil
	}
	switch m := v.(type) {
	case map[string]any:
		return m, nil
	case config.Tree:
		return m, nil
	}
	return nil, &SetupTypeError{Msg: fmt.Sprintf("'%s' object does not support item assignment", pyrepr.TypeName(v))}
}

// SetupSystemExit is the exit code and message of a Python SystemExit leaving a setup flow, false for any other error
func SetupSystemExit(err error) (code int, message string, ok bool) {
	var setupExit SetupExit
	var authExit *AuthExitError
	var nousExit *NousLoginExitError
	var oauthExit *OAuthLoginExitError
	var moaExit *MoAExitError
	switch {
	case errors.As(err, &setupExit):
		return setupExit.Code, "", true
	case errors.As(err, &authExit):
		return authExit.Code, authExit.Message, true
	case errors.As(err, &nousExit):
		return nousExit.Code, "", true
	case errors.As(err, &oauthExit):
		return oauthExit.Code, "", true
	case errors.As(err, &moaExit):
		return 1, moaExit.Message, true
	}
	return 0, "", false
}

// Reports a KeyboardInterrupt leaving a prompt or a tool wait
func isSetupKeyboardInterrupt(err error) bool {
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, tools.ErrKeyboardInterrupt) || errors.Is(err, context.Canceled) ||
		errors.Is(err, errFlowCancelled)
}

// Runs the provider picker under the setup surface, a sys.exit or Ctrl-C answering skipped instead of failing
func runSetupProviderPicker(ctx context.Context) (skipped bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(SetupExit); ok {
				skipped, err = true, nil
				return
			}
			panic(r)
		}
	}()
	err = observability.ProviderSetupSurface(ctx, "cli_setup", func(ctx context.Context) error {
		return setupSelectProviderAndModel(ctx, nil)
	})
	if _, _, exit := SetupSystemExit(err); err != nil && (exit || isSetupKeyboardInterrupt(err)) {
		return true, nil
	}
	return false, err
}

// SetupModelProvider runs the shared provider and model picker, then reloads and resaves the config it wrote
func SetupModelProvider(ctx context.Context, cfg config.Tree) error {
	PrintSetupHeader("Inference Provider", false)
	SetupInfo("Choose how to connect to your main chat model.", "   Guide: "+SetupProviderHelp, nil)
	skipped, err := runSetupProviderPicker(ctx)
	switch {
	case skipped:
		SetupInfo(nil, "Provider setup skipped.")
	case err != nil:
		setupLog.Debug(fmt.Sprintf("select_provider_and_model error during setup: %v", err))
		lines := ProviderSetupFailureLines(err, "icebath model")
		SetupPrintWarning(lines[0])
		for _, line := range lines[1:] {
			SetupPrintInfo(line)
		}
	}
	return ReloadSetupConfig(ctx, cfg, true)
}

// ReloadSetupConfig replaces cfg in place with the config on disk, saving it back when save is set
func ReloadSetupConfig(ctx context.Context, cfg config.Tree, save bool) error {
	h := home.Dir(ctx)
	fresh, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return err
	}
	clear(cfg)
	maps.Copy(cfg, fresh)
	if !save {
		return nil
	}
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// ApplyDefaultAgentSettings writes the recommended iteration and compression defaults without prompting
func ApplyDefaultAgentSettings(ctx context.Context, cfg config.Tree) error {
	h := home.Dir(ctx)
	agent, err := SetdefaultDict(cfg, "agent")
	if err != nil {
		return err
	}
	agent["max_turns"] = 150
	if _, err := config.RemoveHomeEnvValue(ctx, h, "ICEBATH_MAX_ITERATIONS"); err != nil {
		return err
	}
	compression, err := SetdefaultDict(cfg, "compression")
	if err != nil {
		return err
	}
	compression["enabled"] = true
	compression["threshold"] = 0.50
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	SetupPrintSuccess("Applied recommended defaults:")
	SetupInfo("  Max iterations: 150", "  Compression threshold: 0.50", "  Type `/setup agent` later to customize.")
	return nil
}

// SetupToolsEnv is the tool environment the setup sections run their tool configuration in
func SetupToolsEnv(ctx context.Context) *tools.Env {
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// Setup console of the terminal backend section, the setup prompts and prints
type setupTerminalUI struct{}

func (setupTerminalUI) Prompt(question, def string, password bool) string {
	return SetupPrompt(question, def, password)
}
func (setupTerminalUI) PromptYesNo(question string, def bool) bool {
	return PromptYesNo(question, def, "")
}
func (setupTerminalUI) PromptChoice(question string, choices []string, def int) int {
	return PromptChoice(question, choices, def, "")
}
func (setupTerminalUI) PrintHeader(title string) { PrintSetupHeader(title, false) }
func (setupTerminalUI) PrintInfo(text string)    { SetupPrintInfo(text) }
func (setupTerminalUI) PrintSuccess(text string) { SetupPrintSuccess(text) }
func (setupTerminalUI) PrintWarning(text string) { SetupPrintWarning(text) }
func (setupTerminalUI) Println()                 { setupPrintln("") }

// Installs the setup prompts as the console the terminal backend section of pkg/tools draws on
func init() { tools.SetupTerminalConsole = setupTerminalUI{} }

// Value of an env name for the setup sections, empty when unset, a failed scoped read returned as an error
func setupEnvValue(ctx context.Context, key string) (string, error) {
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
	return v, err
}

// Reports any of a registry row's api key env names set, the ambient Claude Code token excepted
func setupProviderHasKey(ctx context.Context, pconfig *Profile) (bool, error) {
	for _, name := range pconfig.KeyEnv {
		if name == "CLAUDE_CODE_OAUTH_TOKEN" {
			continue
		}
		v, err := setupEnvValue(ctx, name)
		if err != nil || v != "" {
			return err == nil, err
		}
	}
	return false, nil
}

// ModelSectionHasCredentials reports usable inference credentials from the auth store, the registry keys or the OpenRouter env vars
func ModelSectionHasCredentials(ctx context.Context, cfg map[string]any) (bool, error) {
	if active, err := NewStore(home.Dir(ctx)).ActiveProvider(); err == nil && active != "" {
		return true, nil
	}
	items := providerRegistry().items(ctx)
	anyOpenRouterKey := false
	for _, name := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY"} {
		v, err := setupEnvValue(ctx, name)
		if err != nil {
			return false, err
		}
		if v != "" {
			anyOpenRouterKey = true
			break
		}
	}
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		providerID := ""
		if raw := modelCfg["provider"]; pyrepr.Truthy(raw) {
			text, isText := raw.(string)
			if !isText {
				return false, &SetupAttributeError{Msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))}
			}
			providerID = pystr.Lower(pystr.Strip(text))
		}
		for _, it := range items {
			if it.Key != providerID {
				continue
			}
			has, err := setupProviderHasKey(ctx, it.Row)
			if err != nil || has {
				return has, err
			}
			break
		}
		if providerID == "openrouter" && anyOpenRouterKey {
			return true, nil
		}
	}
	if anyOpenRouterKey {
		return true, nil
	}
	for _, it := range items {
		has, err := setupProviderHasKey(ctx, it.Row)
		if err != nil {
			return false, err
		}
		if !has {
			continue
		}
		if profile := getProviderProfile(ctx, it.Key); profile == nil || !profile.AmbientCredentials {
			return true, nil
		}
	}
	return false, nil
}
