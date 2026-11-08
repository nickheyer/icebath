package provider

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const bwDefaultTokenEnv = "BWS_ACCESS_TOKEN"

const notBSMTokenWarning = "[yellow]Warning: token doesn't start with '0.' — usually that means " +
	"you pasted something other than a BSM access token.[/yellow]"

const notBSMTokenWarningContinuing = "  [yellow]Warning: token doesn't start with '0.' — usually that means " +
	"you pasted something other than a BSM access token.  Continuing anyway.[/yellow]"

// Version probe, installer, project lister and cache clearer of the Bitwarden CLI, the attributes hermes tests patch
var (
	bwsVersion           = cliVersion
	installBws           = InstallBws
	listProjects         = listBitwardenProjects
	clearBitwardenCaches = ClearBitwardenCaches
)

// The Bitwarden backend as the handlers reach it, bound on first use like the lazy module import
type bitwardenBackend struct {
	find        func(installIfMissing bool) string
	install     func() (string, error)
	fetch       func(BitwardenFetchOptions) (map[string]string, []string, error)
	clearCaches func(homePath string)
}

// Backend bound to the current seams, tests patch the module attributes before each call
func loadBw() bitwardenBackend {
	return bitwardenBackend{find: findBws, install: installBws, fetch: fetchBitwardenSecrets, clearCaches: clearBitwardenCaches}
}

// Attaches the bitwarden subcommand tree to the parent parser
func registerBitwardenCLI(parent *secretsParser) {
	registerSubcommands(parent, "secrets_bw_command", []secretsSubcommand{
		{"setup", "Interactive wizard: install bws, store access token, pick project", bwCmdSetup, []secretsArg{
			arg("--project-id", "Pre-select a project UUID instead of prompting"),
			arg("--access-token", "Provide the access token non-interactively (will be stored in .env)"),
			arg("--server-url", "Bitwarden region / self-hosted endpoint. Examples: "+
				"https://vault.bitwarden.com (US, default), "+
				"https://vault.bitwarden.eu (EU), or your self-hosted URL. "+
				"Skips the interactive region prompt."),
		}},
		{"status", "Show config + binary + token validation status", bwCmdStatus, nil},
		{"token", "Rotate the access token: validate a new one and store it in .env", bwCmdToken, []secretsArg{
			arg("--access-token", "Provide the new token non-interactively (default: masked prompt)"),
			flag("--no-verify", "Store without probing Bitwarden first (not recommended)"),
		}},
		{"sync", "Fetch secrets now and report what changed", bwCmdSync, []secretsArg{
			flag("--apply", "Actually export the secrets into the current shell's env (default: dry-run)"),
		}},
		{"disable", "Turn off the Bitwarden integration", bwCmdDisable, nil},
		{"install", "Install the PM-pinned bws binary", bwCmdInstall, []secretsArg{
			flag("--force", "Verify and repair the managed copy"),
		}},
	})
}

func step(console *pyrich.Console, n int, title string) {
	console.Print()
	console.Print(fmt.Sprintf("[bold]Step %d[/bold]  %s", n, title))
}

// Step 1, the located or downloaded bws binary, empty after printing on failure
func setupBinary(bw bitwardenBackend, console *pyrich.Console) string {
	step(console, 1, "Install the bws CLI")
	binary := bw.find(false)
	if binary == "" {
		console.Print("  No bws on PATH — downloading…")
		installed, err := bw.install()
		if err != nil {
			console.Print(fmt.Sprintf("  [red]✗ Could not install bws: %s[/red]", pyrepr.ExceptionMessage(err)))
			console.Print("  Manual install: https://github.com/bitwarden/sdk-sm/releases")
			return ""
		}
		binary = installed
	}
	console.Print(fmt.Sprintf("  [green]✓[/green] %s  (%s)", binary, bwsVersion(binary)))
	return binary
}

// Setup flags a no TTY run must supply, BWS_SERVER_URL standing in for --server-url
func missingNoninteractiveFlags(ns map[string]any) []string {
	serverURL := pystr.Strip(nsText(ns, "server_url"))
	if serverURL == "" {
		serverURL = os.Getenv("BWS_SERVER_URL")
	}
	provided := [][2]string{
		{"--access-token", nsText(ns, "access_token")},
		{"--server-url", serverURL},
		{"--project-id", nsText(ns, "project_id")},
	}
	var missing []string
	for _, entry := range provided {
		if pystr.Strip(entry[1]) == "" {
			missing = append(missing, entry[0])
		}
	}
	return missing
}

// Step 2, the token from --access-token or a masked prompt persisted to .env, empty when aborted
func setupToken(cli *secretsCLI, ns map[string]any, tokenEnv string) (string, error) {
	step(cli.console, 2, "Provide your access token")
	token := pystr.Strip(nsText(ns, "access_token"))
	if token == "" {
		typed, err := maskedSecretPrompt(fmt.Sprintf("  Paste access token (%s): ", tokenEnv))
		if err != nil {
			return "", err
		}
		token = pystr.Strip(typed)
	}
	if token == "" {
		cli.console.Print("  [red]Empty token, aborting.[/red]")
		return "", nil
	}
	if !strings.HasPrefix(token, "0.") {
		cli.console.Print(notBSMTokenWarningContinuing)
	}
	if err := secretsSaveEnvValue(cli.home, tokenEnv, token); err != nil {
		return "", err
	}
	os.Setenv(tokenEnv, token)
	cli.console.Print(fmt.Sprintf("  [green]✓[/green] stored in %s as %s", secretsEnvPath(cli.home), tokenEnv))
	return token, nil
}

// Step 4, the project the user picks from the visible list, empty after printing when none is usable
func setupProject(cli *secretsCLI, binary, token, serverURL string) (string, error) {
	step(cli.console, 4, "Pick a project")
	projects, ok := listProjects(cli, binary, token, cli.console, serverURL)
	if !ok {
		return "", nil
	}
	if len(projects) == 0 {
		cli.console.Print("  [yellow]No projects visible to this machine account.[/yellow]")
		cli.console.Print("  In the Bitwarden web app, open the machine account → Projects tab " +
			"and grant it access to at least one project.")
		return "", nil
	}
	rows := make([][]string, 0, len(projects))
	for i, p := range projects {
		rows = append(rows, []string{fmt.Sprint(i + 1), projectField(p, "name"), projectField(p, "id")})
	}
	printTable(cli.console, []pyrich.Column{{Header: "#", Style: "cyan", Width: 4}, {Header: "Name"}, {Header: "ID", Style: "dim"}}, rows, nil, "")
	idx, err := promptIndex(cli.console, fmt.Sprintf("  Select project [1-%d]: ", len(projects)), len(projects), false, "")
	if err != nil {
		return "", err
	}
	return pyStr(projects[idx-1]["id"]), nil
}

// Project field as text, a question mark when missing
func projectField(project map[string]any, key string) string {
	v, ok := project[key]
	if !ok {
		return "?"
	}
	return pyStr(v)
}

func bwCmdSetup(cli *secretsCLI, ns map[string]any) (int, error) {
	backend := loadBw()
	console := cli.console
	console.PrintPanel(pyrich.Panel{Body: "[bold]Bitwarden Secrets Manager setup[/bold]\n\n" +
		"Need an access token? In the Bitwarden web app:\n" +
		"  Secrets Manager → Machine accounts → [your account] →\n" +
		"  Access tokens → Create access token\n\n" +
		"Copy the token (starts with [cyan]0.[/cyan]…) — it cannot be retrieved later.",
		BorderStyle: "cyan", Fit: true})
	binary := setupBinary(backend, console)
	if binary == "" {
		return 1, nil
	}
	if !stdinIsATTY() {
		if missing := missingNoninteractiveFlags(ns); len(missing) > 0 {
			console.Print("  [red]Non-interactive mode (no TTY) requires all setup flags.[/red]\n" +
				"  Missing: " + strings.Join(missing, ", ") + "\n\n" +
				"  Usage:\n" +
				"    icebath secrets bitwarden setup \\\n" +
				"      --access-token '0.xxx' \\\n" +
				"      --server-url 'https://vault.bitwarden.com' \\\n" +
				"      --project-id 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'")
			return 1, nil
		}
	}
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	secretsCfg := secretsSetdefaultMapping(secretsSetdefaultMapping(cfg, "secrets"), "bitwarden")
	tokenEnv := bwDefaultTokenEnv
	if v, ok := secretsCfg["access_token_env"]; ok {
		tokenEnv = pyStr(v)
	}
	token, err := setupToken(cli, ns, tokenEnv)
	if err != nil || token == "" {
		return 1, err
	}
	step(console, 3, "Pick a Bitwarden region")
	serverURL, chosen, err := resolveServerURL(cli, ns, secretsCfg)
	if err != nil || !chosen {
		return 1, err
	}
	if serverURL != "" {
		console.Print(fmt.Sprintf("  [green]✓[/green] using %s", serverURL))
	} else {
		console.Print("  [green]✓[/green] using bws default (US Cloud, https://vault.bitwarden.com)")
	}
	projectID := pystr.Strip(nsText(ns, "project_id"))
	projectGiven := projectID != ""
	if !projectGiven {
		projectID, err = setupProject(cli, binary, token, serverURL)
		if err != nil || projectID == "" {
			return 1, err
		}
	}
	testStep := 5
	if projectGiven {
		testStep = 4
	}
	step(console, testStep, "Test fetch")
	secrets, warnings, err := backend.fetch(BitwardenFetchOptions{AccessToken: token, ProjectID: projectID, Binary: binary,
		CacheTTL: 300, UseCache: false, ServerURL: serverURL, HomePath: cli.home})
	if err != nil {
		console.Print(fmt.Sprintf("  [red]✗ Fetch failed: %s[/red]", pyrepr.ExceptionMessage(err)))
		return 1, nil
	}
	if len(secrets) == 0 {
		console.Print("  [yellow]Fetch succeeded but the project has no secrets.[/yellow]")
	} else {
		rows := make([][]string, 0, len(secrets))
		for _, key := range sortedStringKeys(secrets) {
			rows = append(rows, []string{key, fetchStatus(key, tokenEnv)})
		}
		printTable(console, []pyrich.Column{{Header: "Name", Style: "cyan"}, {Header: "Status"}}, rows, nil, "")
	}
	for _, w := range warnings {
		console.Print("  [yellow]warning:[/yellow] " + w)
	}
	secretsCfg["enabled"], secretsCfg["project_id"], secretsCfg["server_url"] = true, projectID, serverURL
	for _, entry := range []struct {
		key   string
		value any
	}{{"access_token_env", tokenEnv}, {"cache_ttl_seconds", 300}, {"override_existing", true}, {"auto_install", true}} {
		if _, ok := secretsCfg[entry.key]; !ok {
			secretsCfg[entry.key] = entry.value
		}
	}
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	console.Print()
	console.Print("[green]✓ Bitwarden Secrets Manager is enabled.[/green]  " +
		"Secrets will be pulled at the start of every Icebath process.")
	console.Print("  Status:  [cyan]icebath secrets bitwarden status[/cyan]\n" +
		"  Refresh: [cyan]icebath secrets bitwarden sync[/cyan]\n" +
		"  Disable: [cyan]icebath secrets bitwarden disable[/cyan]")
	return 0, nil
}

func bwCfg(cfg map[string]any) map[string]any {
	return sectionCfg(cfg, "bitwarden")
}

func fetchStatus(key, tokenEnv string) string {
	if key == tokenEnv {
		return "[dim]bootstrap token — never overrides itself[/dim]"
	}
	if os.Getenv(key) != "" {
		return "[yellow]already set in env (will be overwritten)[/yellow]"
	}
	return "[green]new[/green]"
}

// Config value as text with a default for a missing key
func cfgText(cfg map[string]any, key string, def any) string {
	if v, ok := cfg[key]; ok {
		return pyStr(v)
	}
	return pyStr(def)
}

// Truthy config value with a default for a missing key
func cfgBool(cfg map[string]any, key string, def bool) bool {
	if v, ok := cfg[key]; ok {
		return pyTruthy(v)
	}
	return def
}

func bwCmdStatus(cli *secretsCLI, ns map[string]any) (int, error) {
	backend := loadBw()
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	bw := bwCfg(cfg)
	enabled := pyTruthy(bw["enabled"])
	tokenEnv := cfgText(bw, "access_token_env", bwDefaultTokenEnv)
	projectID := cfgText(bw, "project_id", "")
	serverURL := cfgStr(bw, "server_url")
	token := pystr.Strip(os.Getenv(tokenEnv))
	binary := backend.find(false)
	tokenValidation, validationMessages := tokenValidationStatus(cli, enabled, binary, token, serverURL)
	projectCell := projectID
	if projectCell == "" {
		projectCell = "[dim](unset)[/dim]"
	}
	serverCell := serverURL
	if serverCell == "" {
		serverCell = "[dim]default (US Cloud, https://vault.bitwarden.com)[/dim]"
	}
	binaryCell := "[yellow]not installed[/yellow]"
	if binary != "" {
		binaryCell = fmt.Sprintf("%s (%s)", binary, bwsVersion(binary))
	}
	printStatusPanel(console, "Bitwarden Secrets Manager", [][2]string{
		{"Enabled", yn(enabled)},
		{"Token env var", tokenEnv},
		{"Token in env", yn(token != "")},
		{"Token validation", tokenValidation},
		{"Project ID", projectCell},
		{"Server URL", serverCell},
		{"Override existing", yn(cfgBool(bw, "override_existing", false))},
		{"Cache TTL (s)", cfgText(bw, "cache_ttl_seconds", 300)},
		{"Auto-install", yn(cfgBool(bw, "auto_install", true))},
		{"bws binary", binaryCell},
	})
	for _, message := range validationMessages {
		console.Print(message)
	}
	if !enabled {
		console.Print("\n  Run [cyan]icebath secrets bitwarden setup[/cyan] to enable.")
		return 0, nil
	}
	if token == "" {
		console.Print(fmt.Sprintf("\n  [yellow]Enabled but %s is not set — Icebath will skip BSM "+
			"and warn on next startup.[/yellow]", tokenEnv))
	}
	if projectID == "" {
		console.Print("\n  [yellow]Enabled but no project_id — nothing to fetch.[/yellow]")
	}
	return 0, nil
}

// Rotates the BSM access token, probing Bitwarden with the new one before persisting unless --no-verify
func bwCmdToken(cli *secretsCLI, ns map[string]any) (int, error) {
	backend := loadBw()
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	bw := bwCfg(cfg)
	tokenEnv := cfgText(bw, "access_token_env", bwDefaultTokenEnv)
	serverURL := cfgStr(bw, "server_url")
	verify := func(token string) bool {
		if !strings.HasPrefix(token, "0.") {
			console.Print(notBSMTokenWarning)
		}
		if nsFlag(ns, "no_verify") {
			return true
		}
		binary := backend.find(true)
		if binary == "" {
			console.Print("[red]bws binary not available — cannot verify.  " +
				"Re-run with --no-verify to store anyway.[/red]")
			return false
		}
		console.Print("Verifying against Bitwarden…")
		projects, ok := listProjects(cli, binary, token, console, serverURL)
		if !ok {
			console.Print("[red]✗ New token was rejected — nothing was changed.[/red]")
			return false
		}
		plural := "s"
		if len(projects) == 1 {
			plural = ""
		}
		console.Print(fmt.Sprintf("[green]✓ Token accepted[/green] (%d project%s visible).", len(projects), plural))
		projectID := ""
		if v, ok := bw["project_id"]; ok && v != nil {
			projectID = pyStr(v)
		}
		if projectID != "" && len(projects) > 0 {
			visible := false
			for _, p := range projects {
				if pyStr(p["id"]) == projectID {
					visible = true
				}
			}
			if !visible {
				console.Print(fmt.Sprintf("[yellow]Warning: configured project %s is not visible "+
					"to this machine account.  Grant it access in the Bitwarden web "+
					"app or re-run `icebath secrets bitwarden setup` to pick a different project.[/yellow]", projectID))
			}
		}
		return true
	}
	disabledNote := ""
	if !pyTruthy(bw["enabled"]) {
		disabledNote = "[yellow]Note: the Bitwarden integration is currently disabled — " +
			"run `icebath secrets bitwarden setup` (or set " +
			"secrets.bitwarden.enabled: true) to turn it on.[/yellow]"
	}
	return rotateToken(console, nsText(ns, "access_token"), tokenEnv, rotateTokenOptions{
		Flag: "--access-token",
		Intro: "Create a new token in the Bitwarden web app:\n" +
			"  Secrets Manager → Machine accounts → [your account] → " +
			"Access tokens → Create access token\n",
		Prompt:       fmt.Sprintf("Paste new access token (%s): ", tokenEnv),
		Verify:       verify,
		Save:         func(name, value string) error { return secretsSaveEnvValue(cli.home, name, value) },
		EnvPath:      func() string { return secretsEnvPath(cli.home) },
		ClearCaches:  func() { backend.clearCaches(cli.home) },
		DisabledNote: disabledNote,
	})
}

func bwCmdSync(cli *secretsCLI, ns map[string]any) (int, error) {
	backend := loadBw()
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	bw := bwCfg(cfg)
	if !requireEnabled(console, bw, "Bitwarden", "bitwarden") {
		return 1, nil
	}
	tokenEnv := cfgText(bw, "access_token_env", bwDefaultTokenEnv)
	token := pystr.Strip(os.Getenv(tokenEnv))
	if token == "" {
		console.Print(fmt.Sprintf("[red]%s is not set.[/red]", tokenEnv))
		return 1, nil
	}
	projectID := cfgText(bw, "project_id", "")
	if projectID == "" {
		console.Print("[red]No project_id configured.[/red]")
		return 1, nil
	}
	secrets, warnings, err := backend.fetch(BitwardenFetchOptions{AccessToken: token, ProjectID: projectID,
		CacheTTL: 300, UseCache: false, ServerURL: cfgStr(bw, "server_url"), HomePath: cli.home})
	if err != nil {
		console.Print(fmt.Sprintf("[red]Fetch failed: %s[/red]", pyrepr.ExceptionMessage(err)))
		return 1, nil
	}
	if len(secrets) == 0 {
		console.Print("[yellow]No secrets in project.[/yellow]")
		return 0, nil
	}
	apply := nsFlag(ns, "apply")
	override := cfgBool(bw, "override_existing", false) || apply
	var rows [][]string
	applied := 0
	for _, key := range sortedStringKeys(secrets) {
		already := os.Getenv(key) != ""
		var action string
		switch {
		case key == tokenEnv:
			action = "[dim]skip (bootstrap token)[/dim]"
		case already && !override:
			action = "[dim]skip (already set)[/dim]"
		case apply:
			os.Setenv(key, secrets[key])
			applied++
			action = "[green]exported[/green]"
			if already {
				action += " (overrode)"
			}
		default:
			action = "[green]would export[/green]"
			if already {
				action += " (overrides)"
			}
		}
		rows = append(rows, []string{key, action})
	}
	printTable(console, []pyrich.Column{{Header: "Name", Style: "cyan"}, {Header: "Action"}}, rows, warnings, "")
	if !apply {
		console.Print("\n  This was a dry-run — secrets are picked up automatically on the " +
			"next [cyan]icebath[/cyan] invocation.  Re-run with [cyan]--apply[/cyan] " +
			"to export into the current shell instead.")
	} else {
		console.Print(fmt.Sprintf("\n  [green]Exported %d secret(s) into current process.[/green]", applied))
	}
	return 0, nil
}

func bwCmdDisable(cli *secretsCLI, ns map[string]any) (int, error) {
	return disableSecretSource(cli, "bitwarden",
		"[green]Disabled.[/green]  Bitwarden secrets will NOT be pulled on the next "+
			"Icebath invocation.\n"+
			"  Your access token is left in .env — remove it manually if you also want "+
			"to revoke the credential.")
}

func bwCmdInstall(cli *secretsCLI, ns map[string]any) (int, error) {
	path, err := loadBw().install()
	if err != nil {
		cli.console.Print(fmt.Sprintf("[red]Install failed: %s[/red]", pyrepr.ExceptionMessage(err)))
		return 1, nil
	}
	cli.console.Print(fmt.Sprintf("[green]✓[/green] %s  (%s)", path, bwsVersion(path)))
	return 0, nil
}

// Validation cell of the status panel with the probe's printed lines as follow up messages
func tokenValidationStatus(cli *secretsCLI, enabled bool, binary, token, serverURL string) (string, []string) {
	for _, skipped := range []struct {
		when   bool
		reason string
	}{{!enabled, "integration disabled"}, {token == "", "token missing"}, {binary == "", "bws not installed"}} {
		if skipped.when {
			return fmt.Sprintf("[dim]not checked[/dim] (%s)", skipped.reason), nil
		}
	}
	var messages []string
	if !strings.HasPrefix(token, "0.") {
		messages = append(messages, notBSMTokenWarningContinuing)
	}
	var recorded strings.Builder
	probeConsole := pyrich.NewConsole(&recorded)
	probeConsole.Width = 200
	if _, ok := listProjects(cli, binary, token, probeConsole, serverURL); !ok {
		if details := pystr.Strip(recorded.String()); details != "" {
			for _, line := range strings.Split(details, "\n") {
				messages = append(messages, strings.TrimRightFunc(line, pystr.IsSpace))
			}
		}
		return "[red]failed[/red]", messages
	}
	return "[green]passed[/green]", messages
}

// Substrings of the lowercased bws error and the follow up hint, first match wins
var projectListHints = []struct {
	needles []string
	hint    string
}{
	{[]string{"invalid_client", "400 bad request"},
		"  [yellow]'invalid_client' from the US identity endpoint usually " +
			"means the token is for a different Bitwarden region.  Re-run " +
			"[cyan]icebath secrets bitwarden setup[/cyan] and pick EU or " +
			"self-hosted at the region prompt, or set [cyan]secrets.bitwarden." +
			"server_url[/cyan] in config.yaml.[/yellow]"},
	{[]string{"authorization", "invalid"},
		"  [yellow]This usually means the access token is wrong or revoked. " +
			"Double-check it in the Bitwarden web app.[/yellow]"},
}

// Text truncated to the first n characters
func headRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// Calls bws project list and returns the parsed list, false after printing on failure
func listBitwardenProjects(cli *secretsCLI, binary, token string, console *pyrich.Console, serverURL string) ([]map[string]any, bool) {
	env, err := secretCLIEnv(cli.ctx)
	if err != nil {
		console.Print(fmt.Sprintf("  [red]Couldn't list projects: %s[/red]", pyrepr.ExceptionMessage(err)))
		return nil, false
	}
	env["BWS_ACCESS_TOKEN"] = token
	if serverURL != "" {
		env["BWS_SERVER_URL"] = serverURL
	}
	res, err := pysubprocess.RunWith(cli.ctx, []string{binary, "project", "list", "--output", "json"},
		pysubprocess.Options{Env: envList(env), Timeout: 15 * time.Second})
	if err != nil {
		console.Print(fmt.Sprintf("  [red]Couldn't list projects: %s[/red]", pyrepr.ExceptionMessage(err)))
		return nil, false
	}
	stdout := decodeCLIOutput(res.Stdout)
	if res.Returncode != 0 {
		output := decodeCLIOutput(res.Stderr)
		if output == "" {
			output = stdout
		}
		failure := headRunes(pystr.Strip(output), 300)
		console.Print(fmt.Sprintf("  [red]bws project list failed: %s[/red]", failure))
		lowered := strings.ToLower(failure)
		for _, entry := range projectListHints {
			matched := false
			for _, needle := range entry.needles {
				if strings.Contains(lowered, needle) {
					matched = true
				}
			}
			if matched {
				console.Print(entry.hint)
				break
			}
		}
		return nil, false
	}
	if stdout == "" {
		stdout = "[]"
	}
	data, err := pyjson.Loads(stdout)
	if err != nil {
		console.Print(fmt.Sprintf("  [red]bws returned non-JSON: %s[/red]", pyrepr.ExceptionMessage(err)))
		return nil, false
	}
	items, ok := data.([]any)
	if !ok {
		return []map[string]any{}, true
	}
	projects := []map[string]any{}
	for _, item := range items {
		if p, isDict := item.(map[string]any); isDict && pyTruthy(p["id"]) {
			projects = append(projects, p)
		}
	}
	return projects, true
}

// Canonical Bitwarden region endpoints, a new region here appears in the prompt
var regionPresets = [][2]string{
	{"US Cloud  (https://vault.bitwarden.com — bws default)", ""},
	{"EU Cloud  (https://vault.bitwarden.eu)", "https://vault.bitwarden.eu"},
}

// Server URL from the flag, BWS_SERVER_URL, the existing config or the region menu, false for an empty custom URL
func resolveServerURL(cli *secretsCLI, ns map[string]any, secretsCfg map[string]any) (string, bool, error) {
	console := cli.console
	if given := pystr.Strip(nsText(ns, "server_url")); given != "" {
		return given, true, nil
	}
	if envURL := pystr.Strip(os.Getenv("BWS_SERVER_URL")); envURL != "" {
		console.Print(fmt.Sprintf("  Detected [cyan]BWS_SERVER_URL[/cyan]=%s in your shell — using it.", envURL))
		return envURL, true, nil
	}
	existing := cfgStr(secretsCfg, "server_url")
	if existing != "" {
		console.Print(fmt.Sprintf("  Existing config: [cyan]%s[/cyan]. "+
			"Press Enter to keep, or pick a different option below.", existing))
	}
	table := &pyrich.Table{NoBox: true, PadX: 2, Columns: []pyrich.Column{{Header: "#", Style: "cyan", Width: 4}, {Header: "Region / endpoint"}}}
	for i, preset := range regionPresets {
		table.AddRow(fmt.Sprint(i+1), preset[0])
	}
	customIdx := len(regionPresets) + 1
	table.AddRow(fmt.Sprint(customIdx), "Self-hosted / custom URL")
	console.PrintTable(table)
	prompt := fmt.Sprintf("  Select region [1-%d]", customIdx)
	if existing != "" {
		prompt += " (Enter to keep current)"
	}
	idx, err := promptIndex(console, prompt+": ", customIdx, existing != "", "  [red]Enter a number.[/red]")
	if err != nil {
		return "", false, err
	}
	if idx == 0 {
		return existing, true, nil
	}
	if idx <= len(regionPresets) {
		return regionPresets[idx-1][1], true, nil
	}
	custom, err := console.Input("  Enter your Bitwarden server URL (e.g. https://vault.example.com): ")
	if err != nil {
		return "", false, err
	}
	custom = pystr.Strip(custom)
	if custom == "" {
		console.Print("  [red]Empty URL, aborting.[/red]")
		return "", false, nil
	}
	if !strings.HasPrefix(custom, "http://") && !strings.HasPrefix(custom, "https://") {
		console.Print("  [yellow]Warning: URL doesn't start with http:// or https:// — bws may reject it.[/yellow]")
	}
	return custom, true, nil
}
