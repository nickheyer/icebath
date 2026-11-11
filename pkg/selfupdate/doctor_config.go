package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reports whether the .env text names a provider credential or base URL setting
func doctorHasProviderEnvConfig(content string) bool {
	for _, key := range doctorProviderEnvHints {
		if strings.Contains(content, key) {
			return true
		}
	}
	return false
}

// Legacy config keys still read for compatibility, each a section, a key and the modern replacement
var doctorDeprecatedConfigKeys = [][3]string{
	{"display", "tool_progress_overrides", "display.platforms"},
	{"delegation", "max_async_children", "delegation.max_concurrent_children"},
	{"compression", "summary_model", "auxiliary.compression"},
	{"compression", "summary_provider", "auxiliary.compression"},
	{"compression", "summary_base_url", "auxiliary.compression"},
}

// Deprecated variables looked up in the .env file, each with its replacement
var doctorDeprecatedEnvVars = [][2]string{
	{"ICEBATH_TOOL_PROGRESS", "display.tool_progress in config.yaml — ignored/unsupported since config floor v12"},
	{"ICEBATH_TOOL_PROGRESS_MODE", "display.tool_progress in config.yaml"},
	{"TERMINAL_CWD", "terminal.cwd in config.yaml"},
	{"MESSAGING_CWD", "terminal.cwd in config.yaml"},
	{"QQ_HOME_CHANNEL", "QQBOT_HOME_CHANNEL"},
	{"QQ_HOME_CHANNEL_NAME", "QQBOT_HOME_CHANNEL_NAME"},
}

// Legacy dotted path and replacement of each deprecated key present in the raw config, empty containers counting
func doctorCollectDeprecatedConfigKeys(rawConfig any) [][2]string {
	out := [][2]string{}
	raw, ok := rawConfig.(map[string]any)
	if !ok {
		return out
	}
	for _, k := range doctorDeprecatedConfigKeys {
		section, isMap := raw[k[0]].(map[string]any)
		if !isMap {
			continue
		}
		if _, has := section[k[1]]; has {
			out = append(out, [2]string{k[0] + "." + k[1], k[2]})
		}
	}
	return out
}

// Deprecated variable and replacement for each one set to a non blank value in the given env map
func doctorCollectDeprecatedEnvVars(envMap map[string]any) [][2]string {
	out := [][2]string{}
	for _, v := range doctorDeprecatedEnvVars {
		if value, ok := envMap[v[0]]; ok && value != nil && pystr.Strip(pyrepr.Str(value)) != "" {
			out = append(out, v)
		}
	}
	return out
}

// Findings for the removed Relay plugin, its enabled keys and its ignored exporter variables
func doctorCollectRelayPluginCutoverFindings(rawConfig any, envMap map[string]any) [][2]string {
	findings := [][2]string{}
	raw, _ := rawConfig.(map[string]any)
	if pluginsCfg, ok := raw["plugins"].(map[string]any); ok {
		for _, key := range plugins.LegacyRelayPluginKeysIn(pluginsCfg["enabled"]) {
			findings = append(findings, [2]string{"plugins.enabled: " + key,
				"remove it and configure a standard user or system Relay plugins.toml; " +
					"use " + tools.RelayPluginsConfigEnv + " for an explicit user-file override"})
		}
	}
	effective := map[string]any{}
	for k, v := range envMap {
		effective[k] = v
	}
	if envMap == nil {
		for _, name := range append(append([]string{}, tools.LegacyRelayExportEnvVars...), tools.RelayPluginsConfigEnv) {
			if _, has := effective[name]; has {
				continue
			}
			if v, set := os.LookupEnv(name); set {
				effective[name] = v
			}
		}
	}
	selected := ""
	if v, has := effective[tools.RelayPluginsConfigEnv]; has {
		selected = pyrepr.Str(v)
	}
	if pystr.Strip(selected) == "" {
		for _, name := range tools.ConfiguredLegacyRelayEnvVars(effective) {
			findings = append(findings, [2]string{name,
				"run `icebath migrate relay` to generate relay-plugins.toml and set " + tools.RelayPluginsConfigEnv + "; " +
					"this legacy variable is now ignored and does not configure an exporter"})
		}
	}
	return findings
}

// Prints warn rows for deprecated config keys, env vars and Relay cutover findings, returning what it reported
func doctorReportDeprecatedConfigAndEnv(w io.Writer, rawConfig any, envMap map[string]any) [][2]string {
	deprecated := append(doctorCollectDeprecatedConfigKeys(rawConfig), doctorCollectDeprecatedEnvVars(envMap)...)
	relayCutover := doctorCollectRelayPluginCutoverFindings(rawConfig, envMap)
	findings := append(append([][2]string{}, deprecated...), relayCutover...)
	if len(findings) == 0 {
		doctorCheckOK(w, "No deprecated config keys or env vars", "")
		return findings
	}
	for _, d := range deprecated {
		doctorCheckWarn(w, "Deprecated: "+d[0], "(use "+d[1]+" instead)")
		doctorCheckInfo(w, "Replace "+d[0]+" → "+d[1]+" (warn-only; not auto-migrated here)")
	}
	for _, r := range relayCutover {
		doctorCheckWarn(w, "Breaking Relay migration: "+r[0], "("+r[1]+")")
		doctorCheckInfo(w, "Migrate "+r[0]+": "+r[1])
	}
	return findings
}

// Names the plugins.toml files Relay applies, warning when Relay would reject them
func checkRelayPlugins(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Relay plugin check failed: {e}", "", func(f *Finding) error {
		sources, err := tools.ResolveRelayPluginSources(ctx, nil)
		if err != nil {
			if name, missing := strings.CutPrefix(err.Error(), "No module named "); missing {
				if strings.Trim(name, "'") != "nemo_relay" {
					return err
				}
				doctorCheckOK(d.W, "NeMo Relay is not available on this platform", "")
				return nil
			}
			doctorCheckWarn(d.W, "Relay plugin configuration could not be read", "(Icebath runs without Relay plugins)")
			causes := []string{err.Error()}
			if cause := errors.Unwrap(err); cause != nil {
				causes = append(causes, cause.Error())
			}
			doctorRelayInfoLines(d.W, causes)
			f.ManualIssues = append(f.ManualIssues, "Fix the Relay plugin configuration shown under NeMo Relay Plugins.")
			return nil
		}
		if len(sources.ConfigPaths) == 0 {
			doctorCheckOK(d.W, "No Relay plugin files found", "")
			return nil
		}
		if len(sources.Errors) > 0 {
			doctorCheckWarn(d.W, "Relay will reject this plugin configuration", "(Icebath runs without Relay plugins)")
			f.ManualIssues = append(f.ManualIssues, "Fix the Relay plugin configuration shown under NeMo Relay Plugins.")
		} else {
			report := doctorCheckOK
			if len(sources.Warnings) > 0 {
				report = doctorCheckWarn
			}
			if sources.Enabled {
				report(d.W, "Relay plugins enabled", "(applies to every profile an Icebath process hosts)")
			} else {
				report(d.W, "Relay plugin files found, nothing enabled", "")
			}
		}
		lines := append(append(append([]string{}, sources.ConfigPaths...), sources.Errors...), sources.Warnings...)
		doctorRelayInfoLines(d.W, lines)
		return nil
	})
}

// Prints Relay paths and messages as info rows, the lines of a multi line message indented under the first
func doctorRelayInfoLines(w io.Writer, lines []string) {
	for _, line := range lines {
		var parts []string
		for _, part := range pystr.Splitlines(pystr.Strip(line), false) {
			if pystr.Strip(part) != "" {
				parts = append(parts, part)
			}
		}
		doctorCheckInfo(w, strings.Join(parts, "\n      "))
	}
}

// Prints the active managed scope directory with its pinned key counts, silent when no scope is present
func doctorManagedScopeCheck(w io.Writer) {
	managedDir := config.ManagedDir()
	if managedDir == "" {
		return
	}
	nCfg, nEnv := len(config.ManagedConfigKeys()), len(config.LoadManagedEnv())
	doctorCheckOK(w, fmt.Sprintf("Managed scope active: %d config key(s), %d env key(s) pinned by %s", nCfg, nEnv, managedDir), "")
	if pystr.Strip(os.Getenv(config.EnvManagedDir)) != "" {
		doctorCheckInfo(w, "managed dir set via "+config.EnvManagedDir+"="+managedDir)
	}
}

// Flags mcp_servers entries whose stdio command looks suspicious
func checkMCPSecurity(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "MCP security check failed: {e}", "", func(f *Finding) error {
		cfg, err := config.LoadContext(ctx, d.Home, nil)
		if err != nil {
			return err
		}
		servers, _ := cfg["mcp_servers"].(map[string]any)
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		suspicious := 0
		for _, name := range names {
			entry, ok := servers[name].(map[string]any)
			if !ok {
				continue
			}
			found := mcp.ValidateServerEntry(name, entry)
			if len(found) == 0 {
				continue
			}
			suspicious++
			doctorCheckWarn(d.W, "MCP server '"+name+"' has suspicious stdio command", strings.Join(found, "; "))
			f.ManualIssues = append(f.ManualIssues, "Review/remove mcp_servers."+name+" in config.yaml; rotate any credentials that may have been exposed.")
		}
		if suspicious == 0 {
			doctorCheckOK(d.W, "No suspicious MCP stdio commands", "")
		}
		return nil
	})
}

// Reports the managed scope, then the home .env presence and whether it holds provider credentials
func checkEnvFile(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		doctorManagedScopeCheck(d.W)
		envPath := pypath.PathJoin(d.Home, ".env")
		if _, err := os.Stat(envPath); err == nil {
			doctorCheckOK(d.W, d.DisplayHome+"/.env file exists", "")
			content, err := os.ReadFile(envPath)
			if err != nil {
				return err
			}
			if !doctorCheckBool(d.W, doctorHasProviderEnvConfig(string(content)),
				doctorRow{Text: "API key or custom endpoint configured"},
				doctorRow{Text: "No API key found in " + d.DisplayHome + "/.env"}, false) {
				f.Issues = append(f.Issues, "Run 'icebath setup' to configure API keys")
			}
			return nil
		}
		if _, err := os.Stat(pypath.PathJoin(d.ProjectRoot, ".env")); err == nil {
			doctorCheckOK(d.W, ".env file exists (in project directory)", "")
			return nil
		}
		doctorCheckFail(d.W, d.DisplayHome+"/.env file missing", "")
		if !shouldFix {
			doctorCheckInfo(d.W, "Run 'icebath setup' to create one")
			f.Issues = append(f.Issues, "Run 'icebath setup' to create .env")
			return nil
		}
		if err := pyfs.Makedirs(pypath.PathParent(envPath), 0o777, true); err != nil {
			return err
		}
		file, err := os.OpenFile(envPath, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		_ = os.Chmod(envPath, 0o600)
		doctorCheckOK(d.W, "Created empty "+d.DisplayHome+"/.env", "")
		doctorCheckInfo(d.W, "Run 'icebath setup' to configure API keys")
		f.Fixed++
		return nil
	})
}

// Known provider ids of a raw config with its compatible custom providers
func doctorKnownProviderIDs(ctx context.Context, cfg map[string]any) (map[string]bool, []map[string]any) {
	known := map[string]bool{"openrouter": true, "custom": true, "auto": true, "moa": true}
	for _, key := range provider.RegistryKeys(ctx) {
		known[key] = true
	}
	customProviders := provider.CompatibleCustomProviders(cfg)
	if userProviders, ok := cfg["providers"].(map[string]any); ok {
		for name, provCfg := range userProviders {
			if stripped := pystr.Strip(name); stripped != "" && provider.IsProviderEnabled(provCfg) {
				known[pystr.Lower(stripped)] = true
			}
		}
	}
	for _, entry := range customProviders {
		name := ""
		if pyrepr.Truthy(entry["name"]) {
			name = pystr.Strip(pyrepr.Str(entry["name"]))
		}
		if name == "" {
			continue
		}
		providerKey := ""
		if pyrepr.Truthy(entry["provider_key"]) {
			providerKey = pystr.Strip(pyrepr.Str(entry["provider_key"]))
		}
		for alias := range provider.CustomProviderAliases(name, providerKey) {
			known[alias] = true
		}
	}
	return known, customProviders
}

// Pool environment of a home bound to the context of the doctor run
func doctorPoolEnv(ctx context.Context, h string) provider.PoolEnv {
	env := provider.HomePoolEnv(h)
	env.Ctx = ctx
	return env
}

// Providers whose model ids are vendor slugs, the aggregators plus Fireworks and DeepInfra
var doctorVendorSlugProviders = map[string]bool{
	"openrouter": true, "auto": true, "ai-gateway": true, "kilocode": true, "opencode-zen": true, "huggingface": true,
	"lmstudio": true, "nous": true, "nvidia": true, "fireworks": true, "deepinfra": true,
}

// Whether an API key provider has a credential, true for every provider with its own check elsewhere
func doctorProviderHasCredentials(ctx context.Context, h, runtimeProvider string) (bool, error) {
	if runtimeProvider == "openrouter" {
		for _, k := range []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY"} {
			v, _, err := config.GetEnvValue(ctx, h, k)
			if err != nil {
				return false, err
			}
			if pystr.Strip(v) != "" {
				return true, nil
			}
		}
		return false, nil
	}
	if row := provider.RegistryRow(ctx, runtimeProvider); row != nil && row.AuthType == provider.AuthTypeAPIKey {
		status := provider.GetAuthStatus(doctorPoolEnv(ctx, h), runtimeProvider)
		return pyrepr.Truthy(status["configured"]) || pyrepr.Truthy(status["logged_in"]) || pyrepr.Truthy(status["api_key"]), nil
	}
	return true, nil
}

// Stripped text of the first truthy mapping value, an error when that value is not a string
func doctorStripField(m map[string]any, keys ...string) (string, error) {
	for _, k := range keys {
		v := m[k]
		if !pyrepr.Truthy(v) {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))
		}
		return pystr.Strip(s), nil
	}
	return "", nil
}

// Validates model.provider and model.default of the raw config file against the provider registry
func doctorValidateModelConfig(ctx context.Context, d *Doctor, configPath string, issues *[]string) error {
	cfg, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	modelSection := map[string]any{}
	if raw := cfg["model"]; pyrepr.Truthy(raw) {
		section, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		modelSection = section
	}
	providerRaw, err := doctorStripField(modelSection, "provider")
	if err != nil {
		return err
	}
	providerID := pystr.Lower(providerRaw)
	defaultModel, err := doctorStripField(modelSection, "default", "model")
	if err != nil {
		return err
	}
	knownProviders, customProviders := doctorKnownProviderIDs(ctx, cfg)
	validProviderIDs := map[string]bool{}
	for known := range knownProviders {
		validProviderIDs[known] = true
		validProviderIDs[provider.NormalizeProviderDef(ctx, known)] = true
	}
	accept := map[string]bool{}
	if providerID != "" {
		accept[providerID] = true
	}
	runtimeProvider, catalogProvider, catalogKnown := providerID, providerID, true
	if providerID != "" && providerID != "auto" && providerID != "custom" {
		if resolved, rerr := provider.ResolveAuthProvider(doctorPoolEnv(ctx, d.Home), providerID); rerr == nil {
			runtimeProvider = resolved
			accept[resolved] = true
		}
		userProviders, _ := cfg["providers"].(map[string]any)
		customList := make([]any, 0, len(customProviders))
		for _, entry := range customProviders {
			customList = append(customList, entry)
		}
		if def := provider.ResolveProviderFull(ctx, providerID, userProviders, customList); def != nil {
			catalogProvider = def.ID
			accept[def.ID] = true
		} else {
			catalogProvider, catalogKnown = "", false
		}
	}
	accepted := false
	for id := range accept {
		accepted = accepted || validProviderIDs[id]
	}
	if providerID != "" && providerID != "auto" && (!catalogKnown || (len(knownProviders) > 0 && !accepted)) {
		knownList := "(unavailable)"
		if len(knownProviders) > 0 {
			names := make([]string, 0, len(knownProviders))
			for known := range knownProviders {
				names = append(names, known)
			}
			sort.Strings(names)
			knownList = strings.Join(names, ", ")
		}
		doctorFailAndIssue(d.W, "model.provider '"+providerRaw+"' is not a recognised provider", "(known: "+knownList+")",
			"model.provider '"+providerRaw+"' is unknown. Valid providers: "+knownList+". "+
				"Fix: run 'icebath config set model.provider <valid_provider>'", issues)
	}
	policyID := runtimeProvider
	if policyID == "" {
		policyID = catalogProvider
	}
	policyID = pystr.Lower(pystr.Strip(policyID))
	acceptsVendorSlug := doctorVendorSlugProviders[policyID] || policyID == "custom" || strings.HasPrefix(policyID, "custom:")
	modelBaseURL := ""
	if pyrepr.Truthy(modelSection["base_url"]) {
		modelBaseURL = pystr.Strip(pyrepr.Str(modelSection["base_url"]))
	}
	if policyID == "openai-api" && modelBaseURL != "" && !acceptsVendorSlug {
		matches, merr := pyurl.BaseURLHostMatches(modelBaseURL, "api.openai.com")
		if merr != nil {
			return merr
		}
		acceptsVendorSlug = !matches
	}
	if defaultModel != "" && strings.Contains(defaultModel, "/") && policyID != "" && !acceptsVendorSlug {
		doctorCheckWarn(d.W, "model.default '"+defaultModel+"' uses a vendor/model slug but provider is '"+providerRaw+"'",
			"(vendor-prefixed slugs belong to aggregators like openrouter)")
		*issues = append(*issues, "model.default '"+defaultModel+"' is vendor-prefixed but model.provider is '"+providerRaw+"'. "+
			"Either set model.provider to 'openrouter', or drop the vendor prefix.")
	}
	if runtimeProvider != "" && runtimeProvider != "auto" && runtimeProvider != "custom" {
		if has, cerr := doctorProviderHasCredentials(ctx, d.Home, runtimeProvider); cerr == nil && !has {
			doctorFailAndIssue(d.W, "model.provider '"+runtimeProvider+"' is set but no API key is configured",
				"(add it to "+d.DisplayHome+"/.env or run 'icebath setup')",
				"No credentials found for provider '"+runtimeProvider+"'. Run 'icebath setup' or set the provider's "+
					"API key in "+d.DisplayHome+"/.env, or switch providers with 'icebath config set model.provider <name>'", issues)
		}
	}
	return nil
}

// Resolves every routed auxiliary task block through the runtime resolver and reports the ones that fail
func doctorValidateAuxiliaryConfig(ctx context.Context, d *Doctor, configPath string, issues *[]string) error {
	cfg, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	field := func(block map[string]any, key string) string {
		if !pyrepr.Truthy(block[key]) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(block[key]))
	}
	aux, _ := cfg["auxiliary"].(map[string]any)
	var tasks []string
	for name, raw := range aux {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if p := pystr.Lower(field(block, "provider")); p != "" && p != "auto" {
			tasks = append(tasks, name)
		}
	}
	sort.Strings(tasks)
	var resolved []string
	for _, task := range tasks {
		block := aux[task].(map[string]any)
		requested, model, baseURL, apiKey := field(block, "provider"), field(block, "model"), field(block, "base_url"), field(block, "api_key")
		runtime, rerr := provider.ResolveRuntimeProvider(ctx, d.Home, requested, model, baseURL, apiKey)
		if rerr != nil {
			lines := pystr.Splitlines(rerr.Error(), false)
			if len(lines) == 0 {
				return errors.New("list index out of range")
			}
			reason, moved := lines[0], provider.ProviderInstallHint(ctx, requested)
			if moved == "" {
				moved = "Fix the provider name/credentials in auxiliary." + task + "."
			}
			doctorFailAndIssue(d.W, "auxiliary."+task+".provider '"+requested+"' does not resolve", "("+reason+")",
				"auxiliary."+task+".provider '"+requested+"' cannot be resolved ("+reason+"); the task silently runs "+
					"on the main model. "+moved, issues)
			continue
		}
		if runtime.APIKey == "" && runtime.TokenSource == nil && runtime.Command == "" {
			doctorCheckWarn(d.W, "auxiliary."+task+".provider '"+requested+"' resolved without credentials",
				"("+runtime.Provider+" @ "+runtime.BaseURL+")")
			continue
		}
		host, herr := pyurl.BaseURLHostname(runtime.BaseURL)
		if herr != nil {
			return herr
		}
		if host == "" {
			host = "?"
		}
		resolved = append(resolved, task+"→"+runtime.Provider+"@"+host)
	}
	if len(resolved) > 0 {
		doctorCheckOK(d.W, "auxiliary task routing resolves: "+strings.Join(resolved, ", "), "")
	}
	return nil
}

// Reports config.yaml presence with the project cli-config.yaml as fallback, then validates model and auxiliary routing
func checkConfigFile(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		configPath := home.ConfigPath(d.Home)
		backend, err := config.GetConfigBackend(ctx)
		if err != nil {
			return err
		}
		if !backend.SupportsFileTooling() {
			ok, berr := doctorCheckConfigBackend(ctx, d, backend, f)
			if berr != nil || !ok {
				return berr
			}
		}
		exists, err := config.ConfigExists(ctx, configPath)
		if err != nil {
			return err
		}
		switch {
		case exists:
			if backend.SupportsFileTooling() {
				doctorCheckOK(d.W, d.DisplayHome+"/config.yaml exists", "")
			}
			if werr := doctorWarnOnError(d.W, "Could not validate model/provider config", "({e})", nil, func() error {
				return doctorValidateModelConfig(ctx, d, configPath, &f.Issues)
			}); werr != nil {
				return werr
			}
			return doctorWarnOnError(d.W, "Could not validate auxiliary task routing", "({e})", nil, func() error {
				return doctorValidateAuxiliaryConfig(ctx, d, configPath, &f.Issues)
			})
		case doctorPathExists(pypath.PathJoin(d.ProjectRoot, "cli-config.yaml")):
			doctorCheckOK(d.W, "cli-config.yaml exists (in project directory)", "")
		case shouldFix:
			fromTemplate, serr := config.SeedConfigFile(ctx, d.Home, configPath, pypath.PathJoin(d.ProjectRoot, "cli-config.yaml.example"))
			if serr != nil {
				return serr
			}
			source := "defaults"
			if fromTemplate {
				source = "cli-config.yaml.example"
			}
			doctorCheckOK(d.W, "Created "+d.DisplayHome+"/config.yaml from "+source, "")
			f.Fixed++
		default:
			doctorCheckWarn(d.W, "config.yaml not found", "(using defaults)")
		}
		return nil
	})
}

// Reports whether a path exists the way Path.exists answers
func doctorPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Runs the config file check of one home and project root, returning the number of fixes it applied
func DoctorCheckConfigFile(ctx context.Context, h, projectRoot string, shouldFix bool, w io.Writer) (int, error) {
	f, err := checkConfigFile(ctx, &Doctor{W: w, Home: h, ProjectRoot: projectRoot, DisplayHome: home.Display(h)}, shouldFix)
	if err != nil {
		return 0, err
	}
	return f.Fixed, nil
}

// Reports where a backend without a config file reads config from plus its own warnings, false when it cannot serve
func doctorCheckConfigBackend(ctx context.Context, d *Doctor, backend config.ConfigBackend, f *Finding) (bool, error) {
	if _, err := backend.ReadUserDoc(ctx, d.Home); err != nil {
		var exit interface{ SystemExitCode() string }
		if !errors.As(err, &exit) {
			return false, err
		}
		doctorCheckFail(d.W, "Config backend "+pyrepr.Repr(backend.Name())+" unavailable", exit.SystemExitCode())
		f.Issues = append(f.Issues, "Config backend "+pyrepr.Repr(backend.Name())+" cannot serve config for "+d.Home)
		return false, nil
	}
	doctorCheckOK(d.W, "Config backend: "+backend.Describe(ctx, d.Home), "")
	for _, warning := range backend.DoctorWarnings(ctx, d.Home) {
		doctorCheckWarn(d.W, warning.Message, warning.Hint)
		f.Issues = append(f.Issues, warning.Issue)
	}
	return true, nil
}

// Reports the config version and migrates an outdated config when fixing
func doctorDriftConfigVersion(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	currentVer, latestVer, err := config.CheckConfigVersion(ctx, d.Home, false)
	if err != nil {
		return err
	}
	if doctorCheckBool(d.W, currentVer >= latestVer,
		doctorRow{Text: fmt.Sprintf("Config version up to date (v%d)", currentVer)},
		doctorRow{Text: fmt.Sprintf("Config version outdated (v%d → v%d)", currentVer, latestVer), Detail: "(new settings available)"}, false) {
		return nil
	}
	if !shouldFix {
		f.Issues = append(f.Issues, "Run 'icebath doctor --fix' or 'icebath setup' to migrate config")
		return nil
	}
	if _, merr := config.MigrateConfig(ctx, d.Home, false, false); merr != nil {
		doctorCheckWarn(d.W, "Auto-migration failed: "+merr.Error(), "")
		f.Issues = append(f.Issues, "Run 'icebath setup' to migrate config")
		return nil
	}
	doctorCheckOK(d.W, "Config migrated to latest version", "")
	f.Fixed++
	return nil
}

// Warns about root level provider and base_url keys and moves them under model when fixing
func doctorDriftStaleRootKeys(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	rawConfig, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	var staleRootKeys []string
	for _, k := range []string{"provider", "base_url"} {
		if _, isStr := rawConfig[k].(string); isStr {
			staleRootKeys = append(staleRootKeys, k)
		}
	}
	if len(staleRootKeys) == 0 {
		return nil
	}
	doctorCheckWarn(d.W, "Stale root-level config keys: "+strings.Join(staleRootKeys, ", "), "(should be under 'model:' section)")
	if !shouldFix {
		f.Issues = append(f.Issues, "Stale root-level provider/base_url in config.yaml — run 'icebath doctor --fix'")
		return nil
	}
	rawModel, isMap := rawConfig["model"].(map[string]any)
	if !isMap {
		rawModel = map[string]any{}
		if s, isStr := rawConfig["model"].(string); isStr && pystr.Strip(s) != "" {
			rawModel["default"] = pystr.Strip(s)
		}
		rawConfig["model"] = rawModel
	}
	for _, k := range staleRootKeys {
		value := rawConfig[k]
		delete(rawConfig, k)
		if !pyrepr.Truthy(rawModel[k]) {
			rawModel[k] = value
		}
	}
	if err := config.AtomicConfigReplace(ctx, d.Home, configPath, rawConfig, ""); err != nil {
		return err
	}
	doctorCheckOK(d.W, "Migrated stale root-level keys into model section", "")
	f.Fixed++
	return nil
}

// Warns when a stale ICEBATH_MAX_ITERATIONS in .env shadows agent.max_turns and removes it when fixing
func doctorDriftMaxIterationsGhost(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	rawConfig, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	var cfgMaxTurns any
	if agentCfg, ok := rawConfig["agent"].(map[string]any); ok {
		cfgMaxTurns = agentCfg["max_turns"]
	}
	if cfgMaxTurns == nil {
		cfgMaxTurns = rawConfig["max_turns"]
	}
	envGhost, hasGhost := config.LoadEnv(d.Home)["ICEBATH_MAX_ITERATIONS"]
	if cfgMaxTurns == nil || !hasGhost || pystr.Strip(pyrepr.Str(cfgMaxTurns)) == pystr.Strip(envGhost) {
		return nil
	}
	turns := pyrepr.Str(cfgMaxTurns)
	doctorCheckWarn(d.W, "ICEBATH_MAX_ITERATIONS="+envGhost+" in .env shadows agent.max_turns="+turns+" in config.yaml",
		"(stale ghost from an earlier `icebath setup` run)")
	if !shouldFix {
		f.Issues = append(f.Issues, "Stale ICEBATH_MAX_ITERATIONS in .env shadows config.yaml — run 'icebath doctor --fix'")
		return nil
	}
	removed, err := config.RemoveHomeEnvValue(ctx, d.Home, "ICEBATH_MAX_ITERATIONS")
	if err != nil {
		return err
	}
	if removed {
		doctorCheckOK(d.W, "Removed stale ICEBATH_MAX_ITERATIONS from .env (config.yaml agent.max_turns="+turns+" is now authoritative)", "")
		f.Fixed++
		return nil
	}
	doctorCheckWarn(d.W, "Could not remove ICEBATH_MAX_ITERATIONS from .env", "")
	f.ManualIssues = append(f.ManualIssues, "Manually delete the ICEBATH_MAX_ITERATIONS line from "+d.DisplayHome+"/.env — config.yaml agent.max_turns is authoritative.")
	return nil
}

// Runs the warn only deprecation sweep over the raw config file and the .env file on disk
func doctorDriftDeprecations(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	raw := map[string]any{}
	if configPath != "" {
		loaded, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return err
		}
		raw = loaded
	}
	env := map[string]any{}
	for k, v := range config.LoadEnv(d.Home) {
		env[k] = v
	}
	doctorReportDeprecatedConfigAndEnv(d.W, raw, env)
	return nil
}

// Prints the structural validation issues of the loaded config under their own section
func doctorDriftStructure(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	configIssues := config.ValidateLoadedConfigStructure(ctx, d.Home, config.EnvRefLookupContext(ctx), tools.RemovedBackendNote)
	if len(configIssues) == 0 {
		return nil
	}
	doctorSection(d.W, "Config Structure")
	for _, ci := range configIssues {
		if ci.Severity == "error" {
			doctorCheckFail(d.W, ci.Message, "")
		} else {
			doctorCheckWarn(d.W, ci.Message, "")
		}
		for _, hintLine := range pystr.Splitlines(ci.Hint, false) {
			doctorCheckInfo(d.W, hintLine)
		}
		f.Issues = append(f.Issues, ci.Message)
	}
	return nil
}

// Comparable endpoint URL of a legacy list entry or a providers entry, lowercased without trailing slashes
func doctorEndpointURL(entry map[string]any) string {
	for _, k := range []string{"api", "base_url", "url"} {
		if pyrepr.Truthy(entry[k]) {
			return pystr.Lower(strings.TrimRight(pystr.Strip(pyrepr.Str(entry[k])), "/"))
		}
	}
	return ""
}

// Warns about legacy custom_providers list entries with no providers twin for the same endpoint
func doctorDriftLegacyCustomProviders(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error {
	rawConfig, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		return err
	}
	legacy, ok := rawConfig["custom_providers"].([]any)
	if !ok {
		return nil
	}
	twins := map[string]bool{}
	if providers, isMap := rawConfig["providers"].(map[string]any); isMap {
		for _, e := range providers {
			if entry, isEntry := e.(map[string]any); isEntry {
				twins[doctorEndpointURL(entry)] = true
			}
		}
	}
	for _, e := range legacy {
		entry, isEntry := e.(map[string]any)
		if !isEntry || doctorEndpointURL(entry) == "" || twins[doctorEndpointURL(entry)] {
			continue
		}
		label := ""
		if pyrepr.Truthy(entry["name"]) {
			label = pystr.Strip(pyrepr.Str(entry["name"]))
		}
		if label == "" {
			label = doctorEndpointURL(entry)
		}
		doctorCheckWarn(d.W, "Legacy custom_providers entry '"+label+"' has no providers: twin",
			"(still read from the retired list store; every other surface edits providers:)")
		f.ManualIssues = append(f.ManualIssues,
			"Move custom_providers entry '"+label+"' into config.yaml providers: as `providers.<key>.api: "+
				doctorEndpointURL(entry)+"` and delete it from the list — the v12 list migration ran once and does not re-fire")
	}
	return nil
}

// One config drift step, it prints its rows and records what it found in the shared finding
type doctorDriftStep func(ctx context.Context, d *Doctor, f *Finding, shouldFix bool, configPath string) error

var doctorConfigDriftSteps = []doctorDriftStep{
	doctorDriftConfigVersion, doctorDriftStaleRootKeys, doctorDriftMaxIterationsGhost, doctorDriftDeprecations, doctorDriftStructure,
	doctorDriftLegacyCustomProviders,
}

// Warns about stale per install channel records, naming the config key the user removes
func doctorCheckChannelRecordHygiene(ctx context.Context, d *Doctor) {
	var stale []StaleChannelRecord
	cfg, err := config.LoadContext(ctx, d.Home, nil)
	if err == nil {
		stale, err = StaleChannelRecords(ctx, map[string]any(cfg))
	}
	if err != nil {
		doctorCheckWarn(d.W, "Channel-record hygiene unreadable", "("+err.Error()+")")
		return
	}
	for _, s := range stale {
		recorded := "<no path>"
		if pyrepr.Truthy(s.Record["path"]) {
			recorded = pyrepr.Str(s.Record["path"])
		}
		var detail string
		switch s.Reason {
		case "replaced":
			detail = "(the install at " + recorded + " is a different install now — stale channel entry)"
		case "missing":
			detail = "(nothing at " + recorded + " — safe to remove update.installs." + s.SHA16 + ")"
		default:
			detail = "(no live install claims " + s.SHA16 + " — safe to remove update.installs." + s.SHA16 + ")"
		}
		doctorCheckWarn(d.W, "Stale channel record: "+s.SHA16, detail)
	}
}

// Runs every config drift step independently, then the channel record hygiene report
func checkConfigDrift(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		configPath := home.ConfigPath(d.Home)
		exists, err := config.ConfigExists(ctx, configPath)
		if err != nil {
			return err
		}
		steps := doctorConfigDriftSteps
		if !exists {
			configPath = ""
			steps = []doctorDriftStep{doctorDriftDeprecations}
		}
		for _, step := range steps {
			_ = step(ctx, d, f, shouldFix, configPath)
		}
		doctorCheckChannelRecordHygiene(ctx, d)
		return nil
	})
}

// Warns about retired xAI model references in the loaded config
func checkXAIRetirement(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "xAI retirement check skipped", "({e})", func(f *Finding) error {
		cfg, err := config.LoadContext(ctx, d.Home, nil)
		if err != nil {
			return err
		}
		retiredRefs := provider.FindRetiredXAIRefs(map[string]any(cfg))
		if len(retiredRefs) == 0 {
			doctorCheckOK(d.W, "No retired xAI models in config", "")
			return nil
		}
		for _, ref := range retiredRefs {
			doctorCheckWarn(d.W, provider.FormatRetirementIssue(ref), "")
		}
		doctorCheckInfo(d.W, "Migration guide: "+provider.XAIMigrationGuideURL)
		f.ManualIssues = append(f.ManualIssues, fmt.Sprintf("Update %d retired xAI model reference(s) in config.yaml — see %s", len(retiredRefs), provider.XAIMigrationGuideURL))
		return nil
	})
}

// Reports a timed session_reset policy the config still declares unless the restoring plugin is enabled
func checkRetiredSessionReset(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Session reset check skipped", "({e})", func(f *Finding) error {
		cfg, err := config.LoadUserEffective(home.ConfigPath(d.Home), os.LookupEnv)
		if err != nil {
			return err
		}
		path, mode, found := plugins.RetiredResetPolicy(cfg)
		switch {
		case !found:
			doctorCheckOK(d.W, "No idle/daily session_reset policy configured", "")
		case plugins.ResetPluginEnabled(ctx):
			doctorCheckOK(d.W, path+".mode: "+mode+" is applied by the session reset plugin", "")
		default:
			notice := plugins.FormatSessionResetNotice(path, mode)
			doctorCheckWarn(d.W, notice, "")
			f.ManualIssues = append(f.ManualIssues, notice)
		}
		return nil
	})
}
