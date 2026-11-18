package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Program on the PATH of this process, empty when absent
func hostedProgram(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

// Command that installs an npm package for every user, nil without npm on the PATH
func npmInstallCommand(pkg string) []string {
	if npm := hostedProgram("npm"); npm != "" {
		return []string{npm, "install", "-g", pkg}
	}
	return nil
}

// Command that installs Claude Code with npm or the native installer, nil when neither runs here
func claudeCodeInstallCommand() []string {
	if argv := npmInstallCommand("@anthropic-ai/claude-code"); argv != nil {
		return argv
	}
	if curl := hostedProgram("curl"); curl != "" {
		return []string{"sh", "-c", curl + " -fsSL https://claude.ai/install.sh | bash"}
	}
	return nil
}

// Flows the TUI runs where the wizard flow would send the user to another program, keyed by picker row
func hostedFlowOverrides(ask SetupAsker) map[string]func(r *ModelFlowRun) error {
	return map[string]func(r *ModelFlowRun) error{
		"anthropic":   func(r *ModelFlowRun) error { return hostedAnthropicFlow(r, ask) },
		"qwen-oauth":  func(r *ModelFlowRun) error { return hostedQwenFlow(r, ask) },
		"vertex":      func(r *ModelFlowRun) error { return hostedVertexFlow(r, ask) },
		"copilot-acp": func(r *ModelFlowRun) error { return hostedCopilotACPFlow(r, ask) },
		"openrouter":  func(r *ModelFlowRun) error { return hostedOpenRouterFlow(r, ask) },
	}
}

// Installs the hosted flows and the Entra probe that offers a sign in, the returned func restoring the wizard ones
func installHostedOverrides(ask SetupAsker) func() {
	saved := map[string]func(r *ModelFlowRun) error{}
	for key, flow := range hostedFlowOverrides(ask) {
		saved[key] = providerModelFlows[key]
		providerModelFlows[key] = flow
	}
	probe := azureDescribeCredentialFn
	azureDescribeCredentialFn = hostedEntraProbe(probe, ask)
	return func() {
		for key, flow := range saved {
			providerModelFlows[key] = flow
		}
		azureDescribeCredentialFn = probe
	}
}

// Prints a sign in failure with the TUI retry command, nothing for a cancel
func reportHostedSignIn(out io.Writer, err error, host string) {
	if isSignInCancelled(err) {
		fmt.Fprintln(out, "Sign-in was cancelled.")
		return
	}
	var exit *AuthExitError
	if errors.As(err, &exit) && strings.TrimSpace(exit.Message) != "" {
		fmt.Fprintf(out, "Sign-in did not complete: %s\n", strings.TrimSpace(exit.Message))
		return
	}
	for _, line := range SignInFailureLines(err, host, "/setup model") {
		fmt.Fprintln(out, line)
	}
}

// Anthropic flow of the TUI, the wizard flow with Claude Code installed, else a browser sign in or an install
func hostedAnthropicFlow(r *ModelFlowRun, ask SetupAsker) error {
	if findClaudeCodeCLI("claude") != "" {
		return modelFlowAnthropic(r)
	}
	line := anthropicHeldCredentialLine(r)
	if line == "" && len(LoadPool(r.Env, "anthropic").Entries()) > 0 {
		line = "  Anthropic credentials: ✓ (credential pool)"
	}
	if line != "" {
		flowSay(r.Out, line, "")
		switch promptAuthCredentialsChoice(r.Out, "Anthropic credentials:") {
		case "cancel":
			return nil
		case "use":
			return anthropicPickModel(r)
		}
	}
	h := r.home()
	save := func(name, value string) error { return config.SaveEnvValue(h, name, value) }
	install := claudeCodeInstallCommand()
	rows := []string{"Claude Pro/Max subscription (sign in in the browser)"}
	if install != nil {
		rows = append(rows, "Claude Pro/Max subscription (install Claude Code, then claude setup-token)")
	}
	rows = append(rows, "Anthropic API key (pay-per-token)", "Cancel")
	switch picked := askHostedChoice(ask, "Choose authentication method", rows, 0, "", nil, -1); {
	case picked < 0 || rows[picked] == "Cancel":
		fmt.Fprintln(r.Out, "  No change.")
		return nil
	case strings.Contains(rows[picked], "browser"):
		c := &authCLI{ctx: r.ctx(), env: r.Env, ns: map[string]any{"provider": "anthropic", "auth_type": AuthTypeOAuth}, out: r.Out, errw: r.Out}
		if err := c.add(); err != nil {
			reportHostedSignIn(r.Out, err, "claude.ai")
			return nil
		}
	case strings.Contains(rows[picked], "install"):
		ran, err := askHostedCommand(ask, install)
		if err != nil || !ran || findClaudeCodeCLI("claude") == "" {
			fmt.Fprintln(r.Out, "  Claude Code did not install, so its sign in cannot run.")
			return err
		}
		if ok, err := runAnthropicOAuthFlow(r.ctx(), r.Env, r.Out, save); err != nil || !ok {
			return err
		}
	default:
		if ok, err := anthropicAPIKeyEntry(r); err != nil || !ok {
			return err
		}
	}
	return anthropicPickModel(r)
}

// Qwen OAuth flow of the TUI, a device code sign in where the wizard names the Qwen CLI
func hostedQwenFlow(r *ModelFlowRun, ask SetupAsker) error {
	status := getQwenAuthStatus(r.Env)
	if truthy(status["logged_in"]) {
		return modelFlowQwenOAuth(r)
	}
	lines := []string{"Not logged into Qwen OAuth."}
	if truthy(status["error"]) && !strings.Contains(anyString(status["error"]), "not found") {
		lines = append(lines, fmt.Sprintf("Error: %s", anyString(status["error"])))
	}
	flowSay(r.Out, lines...)
	rows := []string{"Sign in with your Qwen account in the browser", "Cancel"}
	if askHostedChoice(ask, "Qwen sign in", rows, 0, "", nil, -1) != 0 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	if err := qwenDeviceCodeLogin(r.Env, r.Out, true); err != nil {
		reportHostedSignIn(r.Out, err, "chat.qwen.ai")
		return nil
	}
	return modelFlowQwenOAuth(r)
}

// Vertex flow of the TUI, a gcloud sign in or a service account file before the wizard flow
func hostedVertexFlow(r *ModelFlowRun, ask SetupAsker) error {
	held := strings.TrimSpace(r.envValue("VERTEX_CREDENTIALS_PATH")) != "" || strings.TrimSpace(r.envValue("GOOGLE_APPLICATION_CREDENTIALS")) != ""
	if !held {
		_, err := os.Stat(gcpCloudSDKADCPath())
		held = err == nil
	}
	if held {
		return modelFlowVertex(r)
	}
	gcloud := hostedProgram("gcloud")
	rows := []string{"Use a service account JSON file"}
	if gcloud != "" {
		rows = append([]string{"Sign in with gcloud (application default credentials)"}, rows...)
	}
	rows = append(rows, "Keep the application default credentials as they are", "Cancel")
	flowSay(r.Out, "  Vertex uses OAuth2 credentials from gcloud or a service account, not a static API key.", "")
	switch picked := askHostedChoice(ask, "Vertex credentials", rows, 0, "", nil, -1); {
	case picked < 0 || rows[picked] == "Cancel":
		fmt.Fprintln(r.Out, "  No change.")
		return nil
	case strings.HasPrefix(rows[picked], "Sign in"):
		if ran, err := askHostedCommand(ask, []string{gcloud, "auth", "application-default", "login"}); err != nil || !ran {
			fmt.Fprintln(r.Out, "  gcloud sign in did not finish.")
			return err
		}
	case strings.HasPrefix(rows[picked], "Use a service account"):
		path, ok := flowAsk(r.Out, "  Service account JSON path: ", flowAskOptions{CancelMsg: optStr("")})
		if !ok || path == "" {
			fmt.Fprintln(r.Out, "  No change.")
			return nil
		}
		if _, _, err := gcpLoadCredentialsFromFile(r.ctx(), path, []string{gcpCloudPlatformScope}); err != nil {
			fmt.Fprintf(r.Out, "  Cannot use %s: %v\n", path, err)
			return nil
		}
		if err := r.saveEnv("VERTEX_CREDENTIALS_PATH", path); err != nil {
			return err
		}
	}
	return modelFlowVertex(r)
}

// Copilot ACP flow of the TUI, a Copilot CLI install offered when none is found, then the wizard flow
func hostedCopilotACPFlow(r *ModelFlowRun, ask SetupAsker) error {
	install := npmInstallCommand("@github/copilot")
	if _, err := copilotACPCLIStatus(r.ctx()); err == nil || install == nil {
		return modelFlowCopilotACP(r)
	}
	flowSay(r.Out, "  GitHub Copilot ACP delegates Icebath turns to `copilot --acp`, and no Copilot CLI is on the PATH.", "")
	if askHostedChoice(ask, "Copilot CLI", []string{"Install the GitHub Copilot CLI with npm", "Cancel"}, 0, "", nil, -1) != 0 {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	if ran, err := askHostedCommand(ask, install); err != nil || !ran {
		fmt.Fprintln(r.Out, "  The Copilot CLI did not install.")
		return err
	}
	return modelFlowCopilotACP(r)
}

// OpenRouter flow of the TUI, a browser sign in that mints the key beside pasting one
func hostedOpenRouterFlow(r *ModelFlowRun, ask SetupAsker) error {
	pconfig := Profile{Name: "openrouter", DisplayName: "OpenRouter", AuthType: AuthTypeAPIKey, KeyEnv: []string{"OPENROUTER_API_KEY"}}
	if existing, _ := existingAPIKeyForModelFlow(r.Env, "openrouter", pconfig); existing != "" {
		return modelFlowOpenrouter(r)
	}
	rows := []string{"Sign in with OpenRouter in the browser", "Paste an API key", "Cancel"}
	switch askHostedChoice(ask, "OpenRouter", rows, 0, "", nil, -1) {
	case 1:
		return modelFlowOpenrouter(r)
	case 0:
	default:
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	creds, err := OpenrouterPKCELogin(r.Env, r.Out, true, 300*time.Second)
	if err != nil {
		reportHostedSignIn(r.Out, err, "openrouter.ai")
		return nil
	}
	key := anyString(creds["api_key"])
	if err := r.saveEnv("OPENROUTER_API_KEY", key); err != nil {
		return err
	}
	fmt.Fprintln(r.Out, "OpenRouter API key saved.")
	return openrouterPickModel(r, key)
}

// Entra probe that offers an Azure CLI sign in or a service principal when nothing answers
func hostedEntraProbe(probe func(ctx context.Context, cfg *EntraIdentityConfig, scope string, timeout time.Duration) map[string]any,
	ask SetupAsker) func(ctx context.Context, cfg *EntraIdentityConfig, scope string, timeout time.Duration) map[string]any {
	return func(ctx context.Context, cfg *EntraIdentityConfig, scope string, timeout time.Duration) map[string]any {
		info := probe(ctx, cfg, scope, timeout)
		if truthy(info["ok"]) {
			return info
		}
		az := hostedProgram("az")
		rows := []string{"Enter a service principal (tenant, client ID, client secret)", "Continue without a credential"}
		if az != "" {
			rows = append([]string{"Sign in with the Azure CLI (az login)"}, rows...)
		}
		switch picked := askHostedChoice(ask, "Microsoft Entra ID credential", rows, 0, "", nil, -1); {
		case picked < 0 || strings.HasPrefix(rows[picked], "Continue"):
			return info
		case strings.HasPrefix(rows[picked], "Sign in"):
			ran, err := askHostedCommand(ask, []string{az, "login"})
			if err != nil {
				fmt.Fprintf(SetupOutput(), "  az login could not start: %v\n", err)
				return info
			}
			if !ran {
				fmt.Fprintln(SetupOutput(), "  az login did not finish.")
				return info
			}
			ResetEntraCredentialCache()
		default:
			h := home.Dir(ctx)
			for _, field := range [][2]string{{"AZURE_TENANT_ID", "Tenant ID"}, {"AZURE_CLIENT_ID", "Client ID"}, {"AZURE_CLIENT_SECRET", "Client secret"}} {
				value, ok := flowAsk(SetupOutput(), "  "+field[1]+": ", flowAskOptions{Secret: field[0] == "AZURE_CLIENT_SECRET", CancelMsg: optStr("")})
				if !ok || value == "" {
					return info
				}
				if err := config.SaveEnvValueContext(ctx, h, field[0], value); err != nil {
					fmt.Fprintf(SetupOutput(), "  Could not save %s: %v\n", field[0], err)
					return info
				}
				if err := os.Setenv(field[0], value); err != nil {
					fmt.Fprintf(SetupOutput(), "  Could not set %s: %v\n", field[0], err)
					return info
				}
			}
			ResetEntraCredentialCache()
		}
		return probe(ctx, cfg, scope, timeout)
	}
}
