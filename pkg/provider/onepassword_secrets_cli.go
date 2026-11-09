package provider

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const opDocsURL = "https://developer.1password.com/docs/cli/get-started/"

// Version probe, binary lookup and cache clearer of the op CLI, the attributes hermes tests patch
var (
	opVersion               = cliVersion
	findOp                  = FindOp
	fetchOnePasswordSecrets = FetchOnePasswordSecrets
	clearOnePasswordCaches  = ClearOnePasswordCaches
)

// The onepassword section of the loaded config
func opCfg(h string) (map[string]any, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return nil, err
	}
	return sectionCfg(cfg, "onepassword"), nil
}

// The onepassword section created inside the config for writing
func opCfgForWrite(cfg map[string]any) map[string]any {
	return secretsSetdefaultMapping(secretsSetdefaultMapping(cfg, "secrets"), "onepassword")
}

// The env reference mapping of the section, empty unless it is a mapping
func references(opCfg map[string]any) map[string]any {
	if env, ok := opCfg["env"].(map[string]any); ok {
		return env
	}
	return map[string]any{}
}

// Attaches the onepassword subcommand tree to the parent parser
func registerOnePasswordCLI(parent *secretsParser) {
	registerSubcommands(parent, "secrets_op_command", []secretsSubcommand{
		{"setup", "Verify the op CLI, set account / token env var, and enable", opCmdSetup, []secretsArg{
			arg("--account", "1Password account shorthand or sign-in address (op --account)"),
			arg("--token-env", fmt.Sprintf("Env var holding a service-account token (default %s)", opDefaultTokenEnv)),
			arg("--token", "Service-account token to store in .env non-interactively"),
			arg("--binary-path", "Absolute path to the op binary (skips PATH lookup)"),
		}},
		{"status", "Show config + op binary + references", opCmdStatus, nil},
		{"token", "Rotate the service-account token: validate and store it in .env", opCmdToken, []secretsArg{
			arg("--token", "Provide the new token non-interactively (default: masked prompt)"),
			flag("--no-verify", "Store without probing 1Password first (not recommended)"),
		}},
		{"set", "Map an env var to an op:// reference", opCmdSet, []secretsArg{
			arg("env_var", "Environment variable name, e.g. OPENAI_API_KEY"),
			arg("reference", "1Password reference, e.g. op://Private/OpenAI/api key"),
		}},
		{"remove", "Remove an env-var → reference mapping", opCmdRemove, []secretsArg{
			arg("env_var", "Environment variable name to unmap"),
		}},
		{"sync", "Resolve references now and report what changed", opCmdSync, []secretsArg{
			flag("--apply", "Actually export resolved values into the current shell (default: dry-run)"),
		}},
		{"disable", "Turn off the 1Password integration", opCmdDisable, nil},
	})
}

func opCmdSetup(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	console.PrintPanel(pyrich.Panel{Body: "[bold]1Password secret source setup[/bold]\n\n" +
		"Icebath resolves [cyan]op://vault/item/field[/cyan] references through your\n" +
		"already-installed, already-authenticated 1Password CLI (`op`).\n\n" +
		fmt.Sprintf("Don't have it yet? Install + sign in: [cyan]%s[/cyan]", opDocsURL),
		BorderStyle: "cyan", Fit: true})
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	op := opCfgForWrite(cfg)
	console.Print()
	console.Print("[bold]Step 1[/bold]  Locate the op CLI")
	binaryPath := nsText(ns, "binary_path")
	if binaryPath == "" {
		binaryPath = cfgText(op, "binary_path", "")
	}
	binaryPath = pystr.Strip(binaryPath)
	binary := findOp(binaryPath)
	if binary == "" {
		if binaryPath != "" {
			console.Print(fmt.Sprintf("  [red]✗ %s is not an executable op binary.[/red]", binaryPath))
		} else {
			console.Print("  [red]✗ op not found on PATH.[/red]")
		}
		console.Print("  Install the 1Password CLI: " + opDocsURL)
		return 1, nil
	}
	console.Print(fmt.Sprintf("  [green]✓[/green] %s  (%s)", binary, opVersion(binary)))
	if binaryPath != "" {
		op["binary_path"] = binaryPath
	}
	if account := pystr.Strip(nsText(ns, "account")); account != "" {
		op["account"] = account
		console.Print(fmt.Sprintf("  Account: [cyan]%s[/cyan]", account))
	}
	console.Print()
	console.Print("[bold]Step 2[/bold]  Authentication")
	tokenEnv := nsText(ns, "token_env")
	if tokenEnv == "" {
		tokenEnv = cfgText(op, "service_account_token_env", "")
	}
	if tokenEnv == "" {
		tokenEnv = opDefaultTokenEnv
	}
	tokenEnv = pystr.Strip(tokenEnv)
	op["service_account_token_env"] = tokenEnv
	token := pystr.Strip(nsText(ns, "token"))
	switch {
	case token != "":
		if err := secretsSaveEnvValue(cli.home, tokenEnv, token); err != nil {
			return 1, err
		}
		os.Setenv(tokenEnv, token)
		console.Print(fmt.Sprintf("  [green]✓[/green] service-account token stored in %s as %s", secretsEnvPath(cli.home), tokenEnv))
	case os.Getenv(tokenEnv) != "":
		console.Print(fmt.Sprintf("  [green]✓[/green] using service-account token from %s", tokenEnv))
	default:
		if who, ok := opWhoami(cli, binary, cfgText(op, "account", ""), ""); ok {
			console.Print(fmt.Sprintf("  [green]✓[/green] using existing op session (%s)", who))
		} else {
			console.Print("  [yellow]No service-account token and no active op session " +
				"detected.[/yellow]\n" +
				"  Either run [cyan]op signin[/cyan] (desktop/interactive) or set a " +
				fmt.Sprintf("service-account token in %s, then re-run status.", tokenEnv))
		}
	}
	op["enabled"] = true
	for _, entry := range []struct {
		key   string
		value any
	}{{"env", map[string]any{}}, {"cache_ttl_seconds", 300}, {"override_existing", true}} {
		if _, ok := op[entry.key]; !ok {
			op[entry.key] = entry.value
		}
	}
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	console.Print()
	console.Print("[green]✓ 1Password secret source is enabled.[/green]")
	console.Print("  Map credentials:  [cyan]icebath secrets onepassword set OPENAI_API_KEY " +
		"\"op://Private/OpenAI/api key\"[/cyan]\n" +
		"  Preview:          [cyan]icebath secrets onepassword sync[/cyan]\n" +
		"  Status:           [cyan]icebath secrets onepassword status[/cyan]")
	return 0, nil
}

func opCmdStatus(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	op, err := opCfg(cli.home)
	if err != nil {
		return 1, err
	}
	enabled := pyTruthy(op["enabled"])
	account := cfgStr(op, "account")
	tokenEnv := cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	binaryPath := cfgStr(op, "binary_path")
	refs := references(op)
	tokenSet := os.Getenv(tokenEnv) != ""
	binary := findOp(binaryPath)
	accountCell := account
	if accountCell == "" {
		accountCell = "[dim]default[/dim]"
	}
	binaryCell := "[yellow]not found[/yellow]"
	if binary != "" {
		binaryCell = fmt.Sprintf("%s (%s)", binary, opVersion(binary))
	}
	printStatusPanel(console, "1Password secret source", [][2]string{
		{"Enabled", yn(enabled)},
		{"Account", accountCell},
		{"Token env var", tokenEnv},
		{"Token in env", yn(tokenSet)},
		{"Override existing", yn(cfgBool(op, "override_existing", true))},
		{"Cache TTL (s)", cfgText(op, "cache_ttl_seconds", 300)},
		{"op binary", binaryCell},
		{"References", fmt.Sprint(len(refs))},
	})
	if len(refs) > 0 {
		rows := make([][]string, 0, len(refs))
		for _, name := range sortedAnyKeys(refs) {
			rows = append(rows, []string{name, pyStr(refs[name])})
		}
		printTable(console, []pyrich.Column{{Header: "Env var", Style: "cyan"}, {Header: "Reference"}}, rows, nil, "")
	}
	if !enabled {
		console.Print("\n  Run [cyan]icebath secrets onepassword setup[/cyan] to enable.")
		return 0, nil
	}
	if binary != "" && !tokenSet {
		if who, ok := opWhoami(cli, binary, account, ""); ok {
			console.Print(fmt.Sprintf("\n  [green]Active op session:[/green] %s", who))
		} else {
			console.Print(fmt.Sprintf("\n  [yellow]No active op session and %s is unset — "+
				"Icebath will warn and skip 1Password on next startup.[/yellow]", tokenEnv))
		}
	}
	if len(refs) == 0 {
		console.Print("\n  [yellow]No references mapped yet.[/yellow]  Add one: " +
			"[cyan]icebath secrets onepassword set ENV_VAR \"op://…\"[/cyan]")
	}
	return 0, nil
}

func opCmdSet(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	envVar := nsText(ns, "env_var")
	valid, warnings := validateOpReferences(map[string]any{envVar: ns["reference"]})
	value, ok := valid[envVar]
	if !ok {
		for _, w := range warnings {
			console.Print(fmt.Sprintf("[red]%s[/red]", w))
		}
		return 1, nil
	}
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	op := opCfgForWrite(cfg)
	env, isMap := op["env"].(map[string]any)
	if !isMap {
		env = map[string]any{}
		op["env"] = env
	}
	env[envVar] = value
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	console.Print(fmt.Sprintf("[green]✓[/green] mapped [cyan]%s[/cyan] → %s", envVar, value))
	if !pyTruthy(op["enabled"]) {
		console.Print("  [yellow]Note: the integration is disabled — run " +
			"[cyan]icebath secrets onepassword setup[/cyan] to turn it on.[/yellow]")
	}
	return 0, nil
}

func opCmdRemove(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	envVar := nsText(ns, "env_var")
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	op := opCfgForWrite(cfg)
	envMap, isMap := op["env"].(map[string]any)
	if _, mapped := envMap[envVar]; !isMap || !mapped {
		console.Print(fmt.Sprintf("[yellow]%s is not mapped.[/yellow]", envVar))
		return 1, nil
	}
	delete(envMap, envVar)
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	console.Print(fmt.Sprintf("[green]✓[/green] removed mapping for [cyan]%s[/cyan]", envVar))
	return 0, nil
}

// Rotates the service account token, verifying with op whoami before persisting unless --no-verify
func opCmdToken(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	op, err := opCfg(cli.home)
	if err != nil {
		return 1, err
	}
	tokenEnv := cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	account := cfgStr(op, "account")
	binaryPath := cfgStr(op, "binary_path")
	verify := func(token string) bool {
		binary := findOp(binaryPath)
		if binary == "" {
			console.Print(fmt.Sprintf("[red]op CLI not found — install it (%s) or "+
				"re-run with --no-verify to store anyway.[/red]", opDocsURL))
			return false
		}
		console.Print("Verifying with `op whoami`…")
		who, ok := opWhoami(cli, binary, account, token)
		if !ok {
			console.Print("[red]✗ New token was rejected by op — nothing was changed.[/red]")
			return false
		}
		console.Print(fmt.Sprintf("[green]✓ Token accepted[/green] (%s).", who))
		return true
	}
	if nsFlag(ns, "no_verify") {
		verify = nil
	}
	disabledNote := ""
	if !pyTruthy(op["enabled"]) {
		disabledNote = "[yellow]Note: the 1Password integration is currently disabled — " +
			"run `icebath secrets onepassword setup` to turn it on.[/yellow]"
	}
	return rotateToken(console, nsText(ns, "token"), tokenEnv, rotateTokenOptions{
		Flag: "--token",
		Intro: "Create a new service-account token at " +
			"https://my.1password.com → Developer → Service Accounts.\n",
		Prompt:       fmt.Sprintf("Paste new token (%s): ", tokenEnv),
		Verify:       verify,
		Save:         func(name, value string) error { return secretsSaveEnvValue(cli.home, name, value) },
		EnvPath:      func() string { return secretsEnvPath(cli.home) },
		ClearCaches:  func() { clearOnePasswordCaches(cli.home) },
		DisabledNote: disabledNote,
	})
}

func opCmdSync(cli *secretsCLI, ns map[string]any) (int, error) {
	console := cli.console
	op, err := opCfg(cli.home)
	if err != nil {
		return 1, err
	}
	if !requireEnabled(console, op, "1Password", "onepassword") {
		return 1, nil
	}
	refs := references(op)
	if len(refs) == 0 {
		console.Print("[yellow]No op:// references configured.  Add one with " +
			"`icebath secrets onepassword set ENV_VAR \"op://…\"`.[/yellow]")
		return 0, nil
	}
	account := cfgStr(op, "account")
	tokenEnv := cfgText(op, "service_account_token_env", opDefaultTokenEnv)
	binaryPath := cfgStr(op, "binary_path")
	columns := []pyrich.Column{{Header: "Env var", Style: "cyan"}, {Header: "Action"}}
	if nsFlag(ns, "apply") {
		result := ApplyOnePasswordSecrets(OnePasswordApplyOptions{Enabled: true, Env: refs, Account: account,
			ServiceAccountTokenEnv: tokenEnv, BinaryPath: binaryPath, OverrideExisting: cfgBool(op, "override_existing", true),
			CacheTTL: 0, HomePath: cli.home})
		if result.Error != "" {
			console.Print(fmt.Sprintf("[red]%s[/red]", result.Error))
			return 1, nil
		}
		var rows [][]string
		for _, name := range sortedStrings(result.Applied) {
			rows = append(rows, []string{name, "[green]exported[/green]"})
		}
		for _, name := range sortedStrings(result.Skipped) {
			rows = append(rows, []string{name, "[dim]skipped (already set / token var)[/dim]"})
		}
		printTable(console, columns, rows, result.Warnings, "")
		console.Print(fmt.Sprintf("\n  [green]Exported %d secret(s) into current process.[/green]", len(result.Applied)))
		return 0, nil
	}
	secrets, warnings, err := FetchOnePasswordSecrets(OnePasswordFetchOptions{References: refs, Account: account,
		TokenEnv: tokenEnv, BinaryPath: binaryPath, UseCache: false, CacheTTL: 300, HomePath: cli.home})
	if err != nil {
		console.Print(fmt.Sprintf("[red]%s[/red]", pyrepr.ExceptionMessage(err)))
		return 1, nil
	}
	override := cfgBool(op, "override_existing", true)
	action := func(name string) string {
		if name == tokenEnv {
			return "[dim]skip (token var)[/dim]"
		}
		if _, resolved := secrets[name]; !resolved {
			return "[red]unresolved (see warnings)[/red]"
		}
		already := os.Getenv(name) != ""
		if already && !override {
			return "[dim]skip (already set)[/dim]"
		}
		if already {
			return "[green]would export[/green] (overrides)"
		}
		return "[green]would export[/green]"
	}
	var rows [][]string
	for _, name := range sortedAnyKeys(refs) {
		rows = append(rows, []string{name, action(name)})
	}
	printTable(console, columns, rows, warnings, "")
	console.Print("\n  This was a dry-run — references resolve automatically on the next " +
		"[cyan]icebath[/cyan] invocation.  Re-run with [cyan]--apply[/cyan] to export " +
		"into the current shell instead.")
	return 0, nil
}

func opCmdDisable(cli *secretsCLI, ns map[string]any) (int, error) {
	return disableSecretSource(cli, "onepassword",
		"[green]Disabled.[/green]  1Password references will NOT be resolved on the "+
			"next Icebath invocation.\n"+
			"  Your reference mappings are left in config.yaml — remove them with "+
			"[cyan]icebath secrets onepassword remove ENV_VAR[/cyan] if you no longer "+
			"need them.")
}

// Short identity of an authenticated op, a candidate token probed through the child's OP_SERVICE_ACCOUNT_TOKEN
func opWhoami(cli *secretsCLI, binary, account, tokenValue string) (string, bool) {
	cmd := []string{binary, "whoami"}
	if account != "" {
		cmd = append(cmd, "--account", account)
	}
	env, err := secretCLIEnv(cli.ctx)
	if err != nil {
		return "", false
	}
	if tokenValue != "" {
		env["OP_SERVICE_ACCOUNT_TOKEN"] = tokenValue
	}
	res, err := pysubprocess.RunWith(cli.ctx, cmd, pysubprocess.Options{Env: envList(env), Timeout: 10 * time.Second})
	if err != nil || res.Returncode != 0 {
		return "", false
	}
	identity := headRunes(strings.ReplaceAll(pystr.Strip(decodeCLIOutput(res.Stdout)), "\n", " "), 120)
	if identity == "" {
		return "authenticated", true
	}
	return identity, true
}
