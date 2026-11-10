package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/big"
	"os"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// SetupDocsBase is the documentation root the setup screens link to
const SetupDocsBase = "https://hermes-agent.nousresearch.com/docs"

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
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, tools.ErrKeyboardInterrupt) || errors.Is(err, context.Canceled)
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
	SetupInfo("Choose how to connect to your main chat model.", "   Guide: "+SetupDocsBase+"/integrations/providers", nil)
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
	SetupInfo("  Max iterations: 150", "  Compression threshold: 0.50", "  Run `icebath setup agent` later to customize.")
	return nil
}

// Python int of a prompted answer, false when it does not parse
func promptSetupInt(label string, current any) (*big.Int, bool) {
	return pyint.Parse(SetupPrompt(label, pyrepr.Str(current), false))
}

// Python float of a prompted answer, false when it does not parse
func promptSetupFloat(label string, current any) (float64, bool) {
	f, err := pyint.ParseFloat(SetupPrompt(label, pyrepr.Str(current), false))
	return f, err == nil
}

// Stores a prompted int under key only when it parses and accept holds for it
func promptIntSetting(section map[string]any, key, label string, current any, accept func(*big.Int) bool) {
	if value, ok := promptSetupInt(label, current); ok && accept(value) {
		section[key] = pyint.Native(value)
	}
}

var toolProgressHelp = []any{
	"Tool Progress Display", "Controls how much tool activity is shown (CLI and messaging).",
	"  off     — Silent, just the final response",
	"  new     — Show tool name only when it changes (less noise)",
	"  all     — Show every tool call with a short preview",
	"  verbose — Full args, results, and debug logs",
	"  log     — Silent in chat; write every tool call to ~/.icebath/logs/tool_calls.log (gateway only)",
}

// SetupAgentSettings prompts for max iterations, the tool progress mode and the compression threshold
func SetupAgentSettings(ctx context.Context, cfg config.Tree) error {
	h := home.Dir(ctx)
	PrintSetupHeader("Agent Settings", false)
	SetupInfo("   Guide: "+SetupDocsBase+"/user-guide/configuration", nil)
	current, ok := config.CfgGet(cfg, "agent", "max_turns")
	if !ok {
		current = 90
	}
	currentMax := pyrepr.Str(current)
	SetupInfo("Maximum tool-calling iterations per conversation.",
		"Higher = more complex tasks, but costs more tokens.",
		fmt.Sprintf("Press Enter to keep %s. Use 90 for most tasks or 150+ for open exploration.", currentMax))
	maxIter, parsed := promptSetupInt("Max iterations", currentMax)
	if !parsed {
		SetupPrintWarning("Invalid number, keeping current value")
	} else if maxIter.Sign() > 0 {
		agent, err := SetdefaultDict(cfg, "agent")
		if err != nil {
			return err
		}
		agent["max_turns"] = pyint.Native(maxIter)
		delete(cfg, "max_turns")
		if _, err := config.RemoveHomeEnvValue(ctx, h, "ICEBATH_MAX_ITERATIONS"); err != nil {
			return err
		}
		SetupPrintSuccess(fmt.Sprintf("Max iterations set to %s", maxIter.String()))
	}

	SetupInfo(append([]any{""}, toolProgressHelp...)...)
	currentMode, _ := config.CfgGet(cfg, "display", "tool_progress")
	question := "Tool progress mode (Enter keeps per-platform defaults)"
	if pyrepr.Truthy(currentMode) {
		question = "Tool progress mode"
	}
	mode := setupPromptValue(question, currentMode, false)
	if !pyrepr.Truthy(mode) && !pyrepr.Truthy(currentMode) {
		SetupPrintInfo("Keeping each platform's default tool progress")
	} else {
		text, isText := mode.(string)
		if !isText {
			return &SetupAttributeError{Msg: fmt.Sprintf("'%s' object has no attribute 'lower'", pyrepr.TypeName(mode))}
		}
		switch lower := pystr.Lower(text); lower {
		case "off", "new", "all", "verbose", "log":
			display, err := SetdefaultDict(cfg, "display")
			if err != nil {
				return err
			}
			display["tool_progress"] = lower
			if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
				return err
			}
			SetupPrintSuccess("Tool progress set to: " + lower)
		default:
			keep := "per-platform defaults"
			if pyrepr.Truthy(currentMode) {
				keep = pyrepr.Str(currentMode)
			}
			SetupPrintWarning(fmt.Sprintf("Unknown mode '%s', keeping '%s'", text, keep))
		}
	}

	PrintSetupHeader("Context Compression", false)
	SetupInfo("Automatically summarizes old messages when context gets too long.",
		"Higher threshold = compress later (use more context). Lower = compress sooner.")
	compression, err := SetdefaultDict(cfg, "compression")
	if err != nil {
		return err
	}
	compression["enabled"] = true
	currentThreshold, ok := config.CfgGet(cfg, "compression", "threshold")
	if !ok {
		currentThreshold = 0.50
	}
	if threshold, ok := promptSetupFloat("Compression threshold (0.5-0.95)", currentThreshold); ok && 0.5 <= threshold && threshold <= 0.95 {
		compression["threshold"] = threshold
	}
	shown, ok := compression["threshold"]
	if !ok {
		shown = 0.50
	}
	SetupPrintSuccess("Context compression threshold set to " + pyrepr.Str(shown))
	return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
}

// SetupToolsEnv is the tool environment the setup sections run their tool configuration in
func SetupToolsEnv(ctx context.Context) *tools.Env {
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// SetupTools runs the tools command flow on the setup console, first install skipping the platform menu
func SetupTools(ctx context.Context, cfg config.Tree, firstInstall bool) error {
	s := &tools.ToolsSetup{Ctx: ctx, Env: SetupToolsEnv(ctx), Con: setupUI()}
	return s.ToolsCommand(nil, firstInstall, cfg)
}

// Setup wizard console of the terminal backend section, the setup prompts and prints
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
