package memory

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const setupCancelled = -1

var (
	setupReadManifest        = plugins.ReadManifestForInstall
	setupSyncVenv            = pm.SyncVenv
	setupRunningFromSelected = pm.RunningFromSelectedEnvironment
)

type setupProvider struct {
	Name, Hint string
	Provider   Provider
}

type envWrite struct{ Key, Value string }

type postSetupProvider interface {
	PostSetup(ctx context.Context, icebathHome string, cfg map[string]any) error
}

type statusConfigProvider interface {
	StatusConfig(providerConfig any) (any, error)
}

// MemorySetup runs icebath memory setup and status for one home on one console
type MemorySetup struct {
	Env     *tools.Env
	Console *cliui.Console
	Stderr  io.Writer
	stdin   *bufio.Reader

	availableProviders  func(ctx context.Context) []setupProvider
	cursesSelect        func(ctx context.Context, title string, items [][2]string, def, cancelReturns int) (int, error)
	installDependencies func(ctx context.Context, name string) error
	loadConfig          func(ctx context.Context) (map[string]any, error)
	saveConfig          func(ctx context.Context, cfg map[string]any) error
	prompt              func(label, def string, secret bool) (string, error)
}

// Setup runner bound to the env home with the real picker, prompts, installer and config writer
func NewMemorySetup(env *tools.Env, console *cliui.Console, stderr io.Writer) *MemorySetup {
	m := &MemorySetup{Env: env, Console: console, Stderr: stderr}
	m.availableProviders = m.getAvailableProviders
	m.cursesSelect = m.curses
	m.installDependencies = m.installDeps
	m.loadConfig = func(context.Context) (map[string]any, error) {
		tree, err := config.Load(envHome(env), configLookup(env))
		if err != nil {
			return nil, err
		}
		return map[string]any(tree.Clone()), nil
	}
	m.saveConfig = func(ctx context.Context, cfg map[string]any) error {
		return config.SaveConfig(ctx, envHome(env), cfg, config.SaveOptions{})
	}
	m.prompt = m.readPrompt
	return m
}

func (m *MemorySetup) printf(format string, a ...any) { fmt.Fprintf(m.Console.Out, format, a...) }

// Arrow key single select over label and description rows, clearing the screen afterwards
func (m *MemorySetup) curses(ctx context.Context, title string, items [][2]string, def, cancelReturns int) (int, error) {
	rows := make([]cliui.RadioItem, len(items))
	for i, it := range items {
		text := it[0]
		if it[1] != "" {
			text = it[0] + " - " + it[1]
		}
		rows[i] = cliui.PlainItem(text)
	}
	res, err := m.Console.CursesRadiolist(ctx, title, rows, def, cliui.RadiolistOptions{CancelReturns: &cancelReturns})
	m.clearInteractiveTransition()
	return res, err
}

func (m *MemorySetup) printCancelledSetup() { m.printf("\n  Cancelled. No changes saved.\n\n") }

// Clears stale picker output before the next setup screen when stdout is a terminal
func (m *MemorySetup) clearInteractiveTransition() {
	if m.Console.StdoutTTY == nil || !m.Console.StdoutTTY() {
		return
	}
	io.WriteString(m.Console.Out, "\033[2J\033[H")
}

// Prompted value with an optional default and masked secret entry, the default when left blank
func (m *MemorySetup) readPrompt(label, def string, secret bool) (string, error) {
	suffix := ""
	if def != "" {
		suffix = " [" + def + "]"
	}
	var val string
	if secret {
		v, err := m.Console.MaskedSecretPrompt("  " + label + suffix + ": ")
		if err != nil {
			return "", err
		}
		val = v
	} else {
		m.printf("  %s%s: ", label, suffix)
		if m.stdin == nil {
			m.stdin = bufio.NewReader(m.Console.In)
		}
		line, err := m.stdin.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		val = strings.TrimSpace(line)
	}
	if val == "" {
		return def, nil
	}
	return val, nil
}

// Manifest of the provider plugin and the extras and candidate members its preparation needs
func MemoryProviderDependencyInputs(ctx context.Context, env *tools.Env, name string) (map[string]any, *pm.SyncVenvOptions, error) {
	reg := FindProvider(ctx, env, name)
	if reg == nil || reg.Dir == "" {
		return map[string]any{}, nil, nil
	}
	meta, err := setupReadManifest(reg.Dir)
	member := false
	if err == nil {
		var decl *pm.PythonDeclaration
		if decl, err = pm.ReadPythonDeclaration(reg.Dir); err == nil {
			member, err = decl.InstallsPackages()
		}
	}
	if err != nil {
		return nil, nil, pm.NewInstallError(name, err.Error(), "")
	}
	var extras []string
	if extra, ok := meta["extra"].(string); ok && extra != "" {
		extras = []string{extra}
	}
	if len(extras) == 0 && !member {
		return meta, nil, nil
	}
	opts := &pm.SyncVenvOptions{Extras: extras, Explicit: true}
	if member {
		opts.Plugins = &pm.Candidates{Dirs: []string{reg.Dir}}
	}
	return meta, opts, nil
}

// Syncs the provider's dependency union, installed or restart_required, empty when nothing needed preparing
func PrepareMemoryProviderDependencies(ctx context.Context, env *tools.Env, name string) (map[string]any, string, error) {
	meta, opts, err := MemoryProviderDependencyInputs(ctx, env, name)
	if err != nil || opts == nil {
		return meta, "", err
	}
	if err := setupSyncVenv(ctx, *opts); err != nil {
		return nil, "", err
	}
	running, _, err := setupRunningFromSelected(ctx, pm.RepoRoot())
	if err != nil {
		return nil, "", err
	}
	if running {
		return meta, "installed", nil
	}
	return meta, "restart_required", nil
}

// Prepares dependencies and prints install commands for missing external tools
func (m *MemorySetup) installDeps(ctx context.Context, name string) error {
	meta, status, err := PrepareMemoryProviderDependencies(ctx, m.Env, name)
	if err != nil {
		return err
	}
	if status != "" {
		m.printf("  \u2713 Dependencies prepared for %s\n", name)
		if status == "restart_required" {
			m.printf("  Restart Icebath to use the prepared dependencies.\n")
		}
	}
	deps, _ := meta["external_dependencies"].([]any)
	for _, raw := range deps {
		dep, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		check, _ := dep["check"].(string)
		install, _ := dep["install"].(string)
		if check == "" || externalCheckPasses(ctx, check) {
			continue
		}
		if install != "" {
			depName, _ := dep["name"].(string)
			m.printf("\n  \u26a0 '%s' not found. Install with:\n", depName)
			m.printf("    %s\n", install)
		}
	}
	return nil
}

// Reports whether a shell split check command exits zero within five seconds
func externalCheckPasses(ctx context.Context, check string) bool {
	argv, err := pyshlex.SplitPOSIX(check, false)
	if err != nil || len(argv) == 0 {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, argv[0], argv[1:]...).Run() == nil
}

// Discovered providers that load, each with its setup hint
func (m *MemorySetup) getAvailableProviders(ctx context.Context) []setupProvider {
	var out []setupProvider
	for _, d := range DiscoverMemoryProviders(ctx, m.Env) {
		p := LoadMemoryProvider(ctx, m.Env, d.Name)
		if p == nil {
			continue
		}
		schema := p.ConfigSchema()
		secrets, plain := false, false
		for _, f := range schema {
			if pyTruthy(f["secret"]) {
				secrets = true
			} else {
				plain = true
			}
		}
		hint := "local"
		switch {
		case secrets && plain:
			hint = "API key / local"
		case secrets:
			hint = "requires API key"
		case len(schema) == 0:
			hint = "no setup needed"
		}
		out = append(out, setupProvider{Name: d.Name, Hint: hint, Provider: p})
	}
	return out
}

func findSetupProvider(providers []setupProvider, name string) *setupProvider {
	for i := range providers {
		if providers[i].Name == name {
			return &providers[i]
		}
	}
	return nil
}

// Install command of a catalog memory provider that resolves nowhere, empty otherwise
func (m *MemorySetup) catalogInstallHint(ctx context.Context, name string) string {
	if name == "" || FindProvider(ctx, m.Env, name) != nil {
		return ""
	}
	return CatalogInstallHint(ctx, name, "memory")
}

// Normalizes the memory block and reports whether the provider's own post setup took over
func (m *MemorySetup) postSetupHook(ctx context.Context, p Provider, cfg map[string]any) (bool, error) {
	if _, ok := cfg["memory"].(map[string]any); !ok {
		cfg["memory"] = map[string]any{}
	}
	if ps, ok := p.(postSetupProvider); ok {
		return true, ps.PostSetup(ctx, envHome(m.Env), cfg)
	}
	return false, nil
}

// Setup for one named provider without the picker
func (m *MemorySetup) CmdSetupProvider(ctx context.Context, name string) error {
	match := findSetupProvider(m.availableProviders(ctx), name)
	if match == nil {
		m.printf("\n  Memory provider '%s' not found.\n", name)
		if install := m.catalogInstallHint(ctx, name); install != "" {
			m.printf("  It is a catalog plugin that is not installed. Install it with: %s\n\n", install)
		} else {
			m.printf("  Run 'icebath memory setup' to see available providers.\n\n")
		}
		return nil
	}
	m.clearInteractiveTransition()
	if err := m.installDependencies(ctx, match.Name); err != nil {
		return err
	}
	cfg, err := m.loadConfig(ctx)
	if err != nil {
		return err
	}
	if took, err := m.postSetupHook(ctx, match.Provider, cfg); took || err != nil {
		return err
	}
	cfg["memory"].(map[string]any)["provider"] = match.Name
	if err := m.saveConfig(ctx, cfg); err != nil {
		return err
	}
	m.printf("\n  Memory provider: %s\n", match.Name)
	m.printf("  Activation saved to config.yaml\n\n")
	return nil
}

// Walks the schema prompting per field, false when the user cancelled
func (m *MemorySetup) promptSchemaFields(ctx context.Context, name string, schema []map[string]any, providerConfig map[string]any, envWrites *[]envWrite) (bool, error) {
	m.printf("\n  Configuring %s:\n\n", name)
	hasWrite := func(k string) bool {
		for _, w := range *envWrites {
			if w.Key == k {
				return true
			}
		}
		return false
	}
	for _, field := range schema {
		key := pyStr(field["key"])
		desc, ok := field["description"]
		if !ok {
			desc = key
		}
		def := field["default"]
		if df, ok := field["default_from"].(map[string]any); ok && len(df) > 0 {
			ref := providerConfig[pyStr(dictGet(df, "field", ""))]
			if refMap, ok := dictGet(df, "map", map[string]any{}).(map[string]any); ok && pyTruthy(ref) {
				if s, isStr := ref.(string); isStr {
					if v, in := refMap[s]; in {
						def = v
					}
				}
			}
		}
		secret := pyTruthy(field["secret"])
		choices, _ := field["choices"].([]any)
		envVar, _ := field["env_var"].(string)
		url, _ := field["url"].(string)
		if when, ok := field["when"].(map[string]any); ok && len(when) > 0 && !whenMatches(providerConfig, when) {
			continue
		}
		switch {
		case len(choices) > 0 && !secret:
			current, has := providerConfig[key]
			if !has {
				current = def
			}
			idx := 0
			if pyTruthy(current) {
				for i, c := range choices {
					if pyEqual(c, current) {
						idx = i
						break
					}
				}
			}
			items := make([][2]string, len(choices))
			for i, c := range choices {
				items[i] = [2]string{pyStr(c), ""}
			}
			sel, err := m.cursesSelect(ctx, "  "+pyStr(desc), items, idx, setupCancelled)
			if err != nil {
				return false, err
			}
			if sel == setupCancelled {
				m.printCancelledSetup()
				return false, nil
			}
			providerConfig[key] = choices[sel]
		case secret:
			existing := ""
			if envVar != "" {
				existing = os.Getenv(envVar)
			}
			var val string
			var err error
			if existing != "" {
				masked := "set"
				if r := []rune(existing); len(r) > 4 {
					masked = "..." + string(r[len(r)-4:])
				}
				val, err = m.prompt(fmt.Sprintf("%s (current: %s, blank to keep)", pyStr(desc), masked), "", true)
			} else {
				if url != "" {
					m.printf("  Get yours at %s\n", url)
				}
				val, err = m.prompt(pyStr(desc), "", true)
			}
			if err != nil {
				return false, err
			}
			if val != "" && envVar != "" {
				*envWrites = append(*envWrites, envWrite{envVar, val})
			}
		default:
			effective := providerConfig[key]
			if !pyTruthy(effective) {
				effective = def
			}
			defText := ""
			if pyTruthy(effective) {
				defText = pyStr(effective)
			}
			val, err := m.prompt(pyStr(desc), defText, false)
			if err != nil {
				return false, err
			}
			if val != "" {
				providerConfig[key] = val
				if envVar != "" && !hasWrite(envVar) {
					*envWrites = append(*envWrites, envWrite{envVar, val})
				}
			}
		}
	}
	return true, nil
}

func dictGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func whenMatches(cfg, when map[string]any) bool {
	for k, v := range when {
		if !pyEqual(cfg[k], v) {
			return false
		}
	}
	return true
}

func pyEqual(a, b any) bool { return pyrepr.Repr(a) == pyrepr.Repr(b) }

// Interactive provider picker and schema walk that saves activation, provider config and secrets
func (m *MemorySetup) CmdSetup(ctx context.Context) error {
	providers := m.availableProviders(ctx)
	if len(providers) == 0 {
		m.printf("\n  No memory provider plugins detected.\n")
		m.printf("  Install a plugin to %s and try again.\n\n", home.Display(home.PluginsDir(envHome(m.Env))))
		return nil
	}
	items := make([][2]string, 0, len(providers)+1)
	for _, p := range providers {
		items = append(items, [2]string{p.Name, "\u2014 " + p.Hint})
	}
	items = append(items, [2]string{"Built-in only", "\u2014 MEMORY.md / USER.md (default)"})
	selected, err := m.cursesSelect(ctx, "Memory provider setup", items, len(items)-1, setupCancelled)
	if err != nil {
		return err
	}
	if selected == setupCancelled {
		m.printCancelledSetup()
		return nil
	}
	cfg, err := m.loadConfig(ctx)
	if err != nil {
		return err
	}
	if _, ok := cfg["memory"].(map[string]any); !ok {
		cfg["memory"] = map[string]any{}
	}
	if selected >= len(providers) {
		cfg["memory"].(map[string]any)["provider"] = ""
		if err := m.saveConfig(ctx, cfg); err != nil {
			return err
		}
		m.printf("\n  \u2713 Memory provider: built-in only\n")
		m.printf("  Saved to config.yaml\n\n")
		return nil
	}
	chosen := providers[selected]
	m.clearInteractiveTransition()
	if err := m.installDependencies(ctx, chosen.Name); err != nil {
		return err
	}
	if took, err := m.postSetupHook(ctx, chosen.Provider, cfg); took || err != nil {
		return err
	}
	memCfg := cfg["memory"].(map[string]any)
	providerConfig, ok := memCfg[chosen.Name].(map[string]any)
	if !ok {
		providerConfig = map[string]any{}
	}
	var envWrites []envWrite
	if schema := chosen.Provider.ConfigSchema(); len(schema) > 0 {
		if done, err := m.promptSchemaFields(ctx, chosen.Name, schema, providerConfig, &envWrites); !done || err != nil {
			return err
		}
	}
	memCfg["provider"] = chosen.Name
	if err := m.saveConfig(ctx, cfg); err != nil {
		return err
	}
	if len(providerConfig) > 0 {
		if err := chosen.Provider.SaveConfig(providerConfig, envHome(m.Env)); err != nil {
			m.printf("  Failed to write provider config: %s\n", err)
		}
	}
	if len(envWrites) > 0 {
		if err := m.writeEnvVars(ctx, envWrites, ""); err != nil {
			return err
		}
	}
	m.printf("\n  Memory provider: %s\n", chosen.Name)
	m.printf("  Activation saved to config.yaml\n")
	if len(providerConfig) > 0 {
		m.printf("  Provider config saved\n")
	}
	if len(envWrites) > 0 {
		m.printf("  API keys saved to .env\n")
	}
	m.printf("\n  Start a new session to activate.\n\n")
	return nil
}

// Persists provider env vars through the validating .env writer, skipping names it rejects
func (m *MemorySetup) writeEnvVars(ctx context.Context, writes []envWrite, icebathHome string) error {
	h := envHome(m.Env)
	if icebathHome != "" {
		h = icebathHome
	}
	for _, w := range writes {
		if err := config.SaveEnvValueContext(ctx, h, w.Key, w.Value); err != nil {
			var nonASCII *config.NonASCIICredentialError
			if !errors.As(err, &nonASCII) && config.ValidateEnvNameForWrite(w.Key) == nil {
				return err
			}
			m.printf("  Skipping %s: %s\n", w.Key, err)
		}
	}
	return nil
}

func setupMark(enabled any) string {
	if pyTruthy(enabled) {
		return "enabled \u2713"
	}
	return "disabled \u2717"
}

// Prints the built in store flags, memory tool gate, active provider config and installed plugins
func (m *MemorySetup) CmdStatus(ctx context.Context) error {
	cfg, err := m.loadConfig(ctx)
	if err != nil {
		return err
	}
	memCfg, _ := dictGet(cfg, "memory", map[string]any{}).(map[string]any)
	providerName, _ := dictGet(memCfg, "provider", "").(string)
	cliTools := tools.Default.PlatformTools(ctx, "", config.Tree(cfg), "cli", tools.PlatformToolsOptions{})
	toolEnabled := cliTools["memory"] && checkMemoryRequirements(m.Env)
	m.printf("\nMemory status\n%s\n", strings.Repeat("\u2500", 40))
	m.printf("  Built-in (MEMORY.md / USER.md):\n")
	m.printf("    Memory injection:   %s\n", setupMark(dictGet(memCfg, "memory_enabled", true)))
	m.printf("    User profile:       %s\n", setupMark(dictGet(memCfg, "user_profile_enabled", true)))
	m.printf("    Memory tool:        %s\n", setupMark(toolEnabled))
	shown := providerName
	if shown == "" {
		shown = "(none \u2014 built-in only)"
	}
	m.printf("  Provider:  %s\n", shown)
	providers := m.availableProviders(ctx)
	var p Provider
	if match := findSetupProvider(providers, providerName); match != nil {
		p = match.Provider
	}
	if providerName != "" {
		providerConfig := dictGet(memCfg, providerName, map[string]any{})
		display := providerConfig
		if sp, ok := p.(statusConfigProvider); ok {
			if d, err := sp.StatusConfig(providerConfig); err != nil {
				if pc, isMap := providerConfig.(map[string]any); isMap {
					copied := make(map[string]any, len(pc)+1)
					for k, v := range pc {
						copied[k] = v
					}
					copied["status_config_error"] = err.Error()
					display = copied
				}
			} else {
				display = d
			}
		}
		if dm, ok := display.(map[string]any); ok && len(dm) > 0 {
			m.printf("\n  %s config:\n", providerName)
			for _, k := range config.MappingKeys(dm) {
				m.printf("    %s: %s\n", k, pyrepr.Str(dm[k]))
			}
		}
		if parked, ok := ParkedProviderNotice(ctx, m.Env, providerName); ok && parked != "" {
			m.printf("\n  Plugin:    turned off \u2717\n  %s\n", parked)
		} else if p != nil {
			m.printf("\n  Plugin:    installed \u2713\n")
			if p.Available(ctx) {
				m.printf("  Status:    available \u2713\n")
			} else {
				m.printf("  Status:    not available \u2717\n")
				var required []map[string]any
				for _, f := range p.ConfigSchema() {
					if pyTruthy(f["env_var"]) {
						required = append(required, f)
					}
				}
				if len(required) > 0 {
					m.printf("  Missing:\n")
					for _, f := range required {
						envVar := pyStr(f["env_var"])
						url, _ := f["url"].(string)
						set := os.Getenv(envVar) != ""
						mark := "\u2717"
						if set {
							mark = "\u2713"
						}
						line := fmt.Sprintf("    %s %s", mark, envVar)
						if url != "" && !set {
							line += "  \u2192 " + url
						}
						m.printf("%s\n", line)
					}
				}
				m.printf("  Note: systemd/gateway services do not inherit %s \u2014\n", home.Display(home.EnvPath(envHome(m.Env))))
				m.printf("        set any variables above in the service environment.\n")
			}
		} else {
			m.printf("\n  Plugin:    NOT installed \u2717\n")
			if install := m.catalogInstallHint(ctx, providerName); install != "" {
				m.printf("  Install it with: %s\n", install)
			} else {
				m.printf("  Install the '%s' memory plugin to %s\n", providerName, home.Display(home.PluginsDir(envHome(m.Env))))
			}
		}
	}
	if len(providers) > 0 {
		m.printf("\n  Installed plugins:\n")
		for _, sp := range providers {
			active := ""
			if sp.Name == providerName {
				active = " \u2190 active"
			}
			m.printf("    \u2022 %s  (%s)%s\n", sp.Name, sp.Hint, active)
		}
	}
	m.printf("\n")
	return nil
}

// Routes icebath memory setup and status, exit code 1 when dependency preparation fails
func (m *MemorySetup) Run(ctx context.Context, subcommand, provider string) (int, error) {
	if subcommand != "setup" {
		return 0, m.CmdStatus(ctx)
	}
	var err error
	if provider != "" {
		err = m.CmdSetupProvider(ctx, provider)
	} else {
		err = m.CmdSetup(ctx)
	}
	var installErr *pm.InstallError
	if errors.As(err, &installErr) {
		fmt.Fprintf(m.Stderr, "Memory setup failed: %s\n", installErr)
		fmt.Fprintf(m.Stderr, "Correct the dependency error and retry setup.\n")
		return 1, nil
	}
	return 0, err
}

func registerMemorySetupCommands() {}
