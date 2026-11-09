package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Portal models offered when the live Qwen catalog is unavailable
var defaultQwenPortalModels = []string{"qwen3-coder-plus", "qwen3-coder"}

// CursesPromptChoice is the single select curses menu, -1 when cancelled, an error when curses is unavailable
func CursesPromptChoice(question string, choices []string, def int, description string) (int, error) {
	return flowRadiolistFn(question, plainPickerRows(choices), PickerRadiolistOptions{Selected: def, CancelReturns: -1, Description: description})
}

// Display name of a provider config, the id when none is set
func profileDisplayName(p Profile) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

// First n code points of s, all of s when shorter
func prefixRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// Numbered menu answer as a 0 based index, blank for an empty line, cancelled on Ctrl-C or EOF
func askIndex(out io.Writer, prompt string, count int, echoCancel bool) (int, bool, bool) {
	for {
		val, err := flowRawInputFn(out, prompt)
		if err != nil {
			if echoCancel {
				fmt.Fprintln(out)
			}
			return 0, false, true
		}
		val = strings.TrimSpace(val)
		if val == "" {
			return 0, true, false
		}
		n, perr := strconv.Atoi(strings.ReplaceAll(strings.TrimPrefix(val, "+"), "_", ""))
		if perr != nil {
			fmt.Fprintln(out, "Please enter a number")
			continue
		}
		if n-1 >= 0 && n-1 < count {
			return n - 1, false, false
		}
		fmt.Fprintf(out, "Please enter 1-%d\n", count)
	}
}

// Provider menu index with arrow keys, a numbered fallback without curses, false when the user cancels
func promptProviderChoice(out io.Writer, choices []string, def int, title string) (int, bool) {
	if idx, err := flowCursesChoiceFn(title, choices, def, ""); err == nil && idx >= 0 {
		fmt.Fprintln(out)
		return idx, true
	}
	printNumbered(out, title, choices, def)
	fmt.Fprintln(out)
	idx, blank, cancelled := askIndex(out, fmt.Sprintf("Choice [1-%d] (%d): ", len(choices), def+1), len(choices), true)
	if blank {
		return def, true
	}
	if cancelled {
		return 0, false
	}
	return idx, true
}

// China for the api.stepfun.com host, international otherwise
func inferStepfunRegion(baseURL string) string {
	if strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "api.stepfun.com") {
		return "china"
	}
	return "international"
}

// Step Plan base URL of a region
func stepfunBaseURLForRegion(region string) string {
	if region == "china" {
		return StepfunStepPlanCNBaseURL
	}
	return StepFunIntlBaseURL
}

// PromptAPIKey is the api key entry with keep, replace and clear when a key exists, abort true when the caller must return
func PromptAPIKey(env PoolEnv, out io.Writer, pconfig Profile, existingKey, providerID, existingSource string) (string, bool, error) {
	h := envHome(env)
	name := profileDisplayName(pconfig)
	keyEnv := ""
	if len(pconfig.KeyEnv) > 0 {
		keyEnv = pconfig.KeyEnv[0]
	}
	promptNewKey := func(allowLMStudioDefault bool) string {
		lmstudioDefault := providerID == "lmstudio" && allowLMStudioDefault
		prompt := fmt.Sprintf("%s (or Enter to cancel): ", keyEnv)
		if lmstudioDefault {
			prompt = fmt.Sprintf("%s (Enter for no-auth default '%s'): ", keyEnv, LMStudioNoAuthPlaceholder)
		}
		entered, ok := flowAsk(out, prompt, flowAskOptions{Secret: true, CancelMsg: optStr("")})
		if !ok {
			return ""
		}
		if entered == "" && lmstudioDefault {
			return LMStudioNoAuthPlaceholder
		}
		return entered
	}
	if existingKey == "" {
		fmt.Fprintf(out, "No %s API key configured.\n", name)
		if keyEnv == "" {
			return "", true, nil
		}
		newKey := promptNewKey(true)
		if newKey == "" {
			fmt.Fprintln(out, "Cancelled.")
			return "", true, nil
		}
		if err := config.SaveEnvValue(h, keyEnv, newKey); err != nil {
			return "", false, err
		}
		flowSay(out, "API key saved.", "")
		return newKey, false, nil
	}
	sourceSuffix := ""
	if keyEnv != "" {
		sourceSuffix = FormatSecretSourceSuffix(h, keyEnv)
	}
	fmt.Fprintf(out, "  %s API key: %s... ✓%s\n", name, prefixRunes(existingKey, 8), sourceSuffix)
	if keyEnv == "" {
		fmt.Fprintln(out)
		return existingKey, false, nil
	}
	poolBacked := strings.HasPrefix(existingSource, "credential_pool:")
	menu := "  [K]eep / [R]eplace / [C]lear (default K): "
	if poolBacked {
		menu = "  [K]eep / [R]eplace (default K): "
	}
	choice, ok := flowAsk(out, menu, flowAskOptions{Raw: true, CancelMsg: optStr("")})
	if !ok {
		choice = "k"
	}
	choice = strings.ToLower(choice)
	switch {
	case strings.HasPrefix(choice, "r"):
		newKey := promptNewKey(false)
		if newKey == "" {
			flowSay(out, "  No change.", "")
			return existingKey, false, nil
		}
		if err := config.SaveEnvValue(h, keyEnv, newKey); err != nil {
			return "", false, err
		}
		flowSay(out, "  API key updated.", "")
		return newKey, false, nil
	case strings.HasPrefix(choice, "c") && !poolBacked:
		if err := config.SaveEnvValue(h, keyEnv, ""); err != nil {
			return "", false, err
		}
		fmt.Fprintf(out, "  API key cleared.  Re-run `icebath setup` to configure %s again.\n", name)
		return "", true, nil
	}
	fmt.Fprintln(out)
	return existingKey, false, nil
}

// Runs the Claude OAuth setup token flow, true when credentials were saved
func runAnthropicOAuthFlow(ctx context.Context, env PoolEnv, out io.Writer, save func(name, value string) error) (bool, error) {
	h := envHome(env)
	activateClaudeCode := func() (bool, error) {
		var creds map[string]any
		func() {
			defer func() { _ = recover() }()
			creds = ReadClaudeCodeCredentials(env)
		}()
		if len(creds) > 0 && (IsClaudeCodeTokenValid(creds) || truthy(creds["refreshToken"])) {
			if err := config.UseAnthropicClaudeCodeCredentials(ctx, h, save); err != nil {
				return false, err
			}
			fmt.Fprintln(out, "  ✓ Claude Code credentials linked.")
			fmt.Fprintf(out, "    Hermes will use Claude's credential store directly instead of copying a setup-token into %s/.env.\n", home.Display(h))
			return true, nil
		}
		return false, nil
	}
	pasteToken := func(prompt string) (bool, bool, error) {
		token, ok := flowAsk(out, prompt, flowAskOptions{Secret: true, CancelMsg: optStr("")})
		if !ok {
			return false, true, nil
		}
		if token == "" {
			return false, false, nil
		}
		if err := config.SaveAnthropicOAuthToken(ctx, h, token, save); err != nil {
			return false, false, err
		}
		fmt.Fprintln(out, "  ✓ Setup-token saved.")
		return true, false, nil
	}
	flowSay(out, "", "  Running 'claude setup-token' — follow the prompts below.",
		"  A browser window will open for you to authorize access.", "")
	var token string
	var err error
	if findClaudeCodeCLI("claude") == "" {
		err = exec.ErrNotFound
	} else {
		token, err = RunOAuthSetupToken(env)
	}
	if err != nil && (errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)) {
		flowSay(out, "", "  The 'claude' CLI is required for OAuth login.", "", "  To install and authenticate:", "",
			"    1. Install Claude Code:  npm install -g @anthropic-ai/claude-code",
			"    2. Run:                  claude setup-token",
			"    3. Follow the browser prompts to authorize",
			"    4. Re-run:               icebath model", "",
			"  Or paste an existing setup-token now (sk-ant-oat-...):", "")
		saved, cancelled, perr := pasteToken("  Setup-token (or Enter to cancel): ")
		if perr != nil {
			return false, perr
		}
		if cancelled {
			return false, nil
		}
		if saved {
			return true, nil
		}
		fmt.Fprintln(out, "  Cancelled — install Claude Code and try again.")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if token != "" {
		linked, lerr := activateClaudeCode()
		if lerr != nil {
			return false, lerr
		}
		if linked {
			return true, nil
		}
		if err := config.SaveAnthropicOAuthToken(ctx, h, token, save); err != nil {
			return false, err
		}
		fmt.Fprintln(out, "  ✓ OAuth credentials saved.")
		return true, nil
	}
	flowSay(out, "", "  If the setup-token was displayed above, paste it here:", "")
	saved, cancelled, perr := pasteToken("  Paste setup-token (or Enter to cancel): ")
	if perr != nil {
		return false, perr
	}
	if cancelled {
		return false, nil
	}
	if saved {
		return true, nil
	}
	fmt.Fprintln(out, "  ⚠ Could not detect saved credentials.")
	return false, nil
}
