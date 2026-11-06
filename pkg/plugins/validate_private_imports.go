package plugins

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Icebath surfaces documented for plugin authors, module entries or module:Name entries
var privateImportsPublic = func() map[string]bool {
	public := map[string]bool{}
	for _, entry := range []string{
		"agent.account_usage", "agent.acp_client", "agent.browser_provider", "agent.context_engine", "agent.credential_pool",
		"agent.delegation_backends", "agent.delegation_provider", "hermes_cli.auth_model_picker:prompt_model_selection",
		"hermes_cli.auth:resolve_external_process_provider_credentials", "hermes_cli.model_setup",
		"agent.image_gen_provider", "agent.memory_manager", "agent.memory_provider", "agent.secret_scope",
		"agent.secret_sources.base", "agent.subagent_lifecycle", "agent.terminal_env_provider",
		"agent.transports", "agent.transports.chat_completions", "agent.video_gen_provider",
		"agent.web_search_provider", "cli", "gateway.config", "gateway.platform_registry",
		"gateway.platforms._shared", "gateway.platforms.base", "gateway.platforms.event",
		"gateway.platforms.helpers", "gateway.status", "hermes_cli.auth_oauth_pkce_plugin",
		"hermes_cli.config", "hermes_cli.plugin_events", "hermes_cli.plugin_provider_requests",
		"hermes_constants", "hermes_platform", "hermes_state", "plugins.plugin_storage", "plugins.plugin_utils", "pm",
		"providers", "providers.base", "tools.browser_supervisor", "tools.browser_supervisor_capture",
		"tools.computer_use.backend", "tools.registry",
		"agent.context_references", "agent.plugin_llm", "agent.transcription_provider", "agent.tts_provider",
		"cron.execution_identity",
		"hermes_cli.dashboard_auth", "hermes_cli.platform_actions", "hermes_cli.plugins:PluginContext",
		"hermes_cli.plugins_ledger:PluginRegistration", "hermes_cli.plugins_state:PluginState",
		"agent.runtime_cwd:resolve_agent_cwd", "agent.usage_pricing:CostResult", "hermes_cli.auth_constants:AuthError",
		"tools.environments.base:BaseEnvironment", "tools.environments:BaseEnvironment",
		"utils:atomic_json_write", "utils:read_json_or_empty",
		"gateway.platforms.access_policy_mixin:OwnAccessPolicyMixin", "tools.approval:resolve_gateway_approval",
		"tools.clarify_gateway:get_pending", "tools.clarify_gateway:resolve_gateway_clarify",
		"tools.clarify_gateway:mark_awaiting_text", "tools.clarify_gateway:SKIPPED",
		"tools.url_safety:is_safe_url", "tools.url_safety:create_ssrf_safe_async_client",
		"gateway.platforms._http_client_limits:platform_httpx_limits",
		"hermes_cli.gateway_setup_wizard:print_setup_header", "hermes_cli.gateway_setup_wizard:confirm_reconfigure",
		"hermes_cli.gateway_setup_wizard:offer_home_channel", "hermes_cli.gateway_setup_wizard:save_env_values",
		"hermes_cli.gateway_setup_wizard:prompt_csv", "hermes_cli.setup:prompt", "hermes_cli.setup:prompt_choice",
		"hermes_cli.setup:prompt_yes_no", "hermes_cli.cli_output",
		"tools.skills_tool:find_all_skills", "tools.skills_tool:sort_skills", "tools.skills_tool:skill_search_dirs",
		"tools.skills_tool:locate_skill", "tools.skills_tool:skill_lookup_path_error",
		"tools.skills_sync:dir_hash", "tools.skill_usage:read_skill_name",
		"gateway.session_context:get_session_env",
	} {
		public[entry] = true
		for from, to := range map[string]string{"hermes_cli": "icebath_cli", "hermes_constants": "icebath_constants"} {
			if entry == from || strings.HasPrefix(entry, from+".") || strings.HasPrefix(entry, from+":") {
				public[to+entry[len(from):]] = true
			}
		}
	}
	return public
}()

const privateImportsShown = 10

// Every dotted name under which one of the plugin's own files could be imported
func privateLocalModules(files []parsedSource) map[string]bool {
	names := map[string]bool{}
	for _, f := range files {
		parts := modulePartsOf(f.rel)
		for start := range parts {
			for end := start + 1; end <= len(parts); end++ {
				names[strings.Join(parts[start:end], ".")] = true
			}
		}
	}
	return names
}

// Nodes inside the body of an if TYPE_CHECKING block
func typeCheckingNodes(tree *pyast.Node) map[*pyast.Node]bool {
	skipped := map[*pyast.Node]bool{}
	for _, node := range pyast.Walk(tree) {
		if !node.Is("If") {
			continue
		}
		test := node.Child("test")
		key, ok := test.Str("id")
		if !ok || key == "" {
			key, _ = test.Str("attr")
		}
		if test.Is("Name", "Attribute") && key == "TYPE_CHECKING" {
			for _, stmt := range node.Children("body") {
				for _, n := range pyast.Walk(stmt) {
					skipped[n] = true
				}
			}
		}
	}
	return skipped
}

type importedName struct{ module, name string }

// Module and name pairs an import node binds, the name empty for a plain module import
func importedNames(node *pyast.Node) ([]importedName, error) {
	switch {
	case node.Is("Import"):
		out := []importedName{}
		for _, alias := range node.Children("names") {
			name, _ := alias.Str("name")
			out = append(out, importedName{name, ""})
		}
		return out, nil
	case node.Is("ImportFrom"):
		level, _ := node.Int("level")
		module, _ := node.Str("module")
		if level != 0 || module == "" {
			return nil, nil
		}
		out := []importedName{}
		for _, alias := range node.Children("names") {
			name, _ := alias.Str("name")
			out = append(out, importedName{module, name})
		}
		return out, nil
	case node.Is("Call"):
		args := node.Children("args")
		if len(args) == 0 {
			return nil, nil
		}
		value, ok := astStrConstant(args[0])
		if !ok {
			return nil, nil
		}
		fn, err := pyast.Unparse(node.Child("func"))
		if err != nil {
			return nil, err
		}
		if fn == "importlib.import_module" || fn == "import_module" {
			return []importedName{{value, ""}}, nil
		}
	}
	return nil, nil
}

// An Icebath shaped path core does not ship whose tail is one of the plugin's own modules
func isOwnModulePath(module string, local map[string]bool) bool {
	parts := strings.Split(module, ".")
	for i := 1; i < len(parts); i++ {
		if local[strings.Join(parts[i:], ".")] {
			return !coreModuleExists(module)
		}
	}
	return false
}

// Dotted target to report, empty when the import is public, local or not Icebath's
func privateTarget(module, name string, core, local map[string]bool) string {
	target := module
	if name != "" {
		target = module + "." + name
	}
	own := isOwnModulePath(module, local) || (name != "" && isOwnModulePath(target, local))
	icebath := core[strings.Split(module, ".")[0]] && !local[module] && !own
	public := privateImportsPublic[target] || privateImportsPublic[module+":"+name] ||
		(privateImportsPublic[module] && !strings.HasPrefix(name, "_"))
	if icebath && !public {
		return target
	}
	return ""
}

// Plugin modules that can load inside Icebath, hidden directories left out
func privateParsedFiles(pluginDir string) ([]parsedSource, error) {
	return parsePluginSources(pluginDir, func(rel string) bool {
		parts := strings.Split(rel, "/")
		for _, p := range parts[:len(parts)-1] {
			if strings.HasPrefix(p, ".") {
				return false
			}
		}
		return true
	})
}

// Sites of every private core import, the first site of each module leading and then the rest
func privateImportFindings(files []parsedSource) ([]string, error) {
	local := privateLocalModules(files)
	core := coreTables().packages
	type site struct {
		rel          string
		line         int
		target, from string
	}
	found := map[site]bool{}
	for _, f := range files {
		skipped := typeCheckingNodes(f.tree)
		for _, node := range pyast.Walk(f.tree) {
			if skipped[node] {
				continue
			}
			names, err := importedNames(node)
			if err != nil {
				return nil, err
			}
			for _, imp := range names {
				if target := privateTarget(imp.module, imp.name, core, local); target != "" {
					found[site{f.rel, node.Lineno, target, imp.module}] = true
				}
			}
		}
	}
	sorted := make([]site, 0, len(found))
	for s := range found {
		sorted = append(sorted, s)
	}
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.rel != b.rel {
			return a.rel < b.rel
		}
		if a.line != b.line {
			return a.line < b.line
		}
		if a.target != b.target {
			return a.target < b.target
		}
		return a.from < b.from
	})
	seen := map[string]bool{}
	var first, rest []string
	for _, s := range sorted {
		text := fmt.Sprintf("%s:%d %s", s.rel, s.line, s.target)
		if !seen[s.from] {
			seen[s.from] = true
			first = append(first, text)
		} else {
			rest = append(rest, text)
		}
	}
	return append(first, rest...), nil
}

// Every name the plugin defines or assigns, so its own private helpers never read as core
func privateOwnNames(files []parsedSource) map[string]bool {
	own := map[string]bool{}
	for _, f := range files {
		for _, node := range pyast.Walk(f.tree) {
			switch {
			case node.Is("FunctionDef", "AsyncFunctionDef", "ClassDef"):
				name, _ := node.Str("name")
				own[name] = true
			case node.Is("Attribute") && node.Child("ctx").Is("Store"):
				attr, _ := node.Str("attr")
				own[attr] = true
			case node.Is("Name") && node.Child("ctx").Is("Store"):
				id, _ := node.Str("id")
				own[id] = true
			}
		}
	}
	return own
}

// Sites of every access to a private method of a core handle class the plugin does not define
func privateAttributeFindings(files []parsedSource) ([]string, error) {
	own := privateOwnNames(files)
	private := coreTables().private
	type site struct {
		rel  string
		line int
		expr string
	}
	found := map[site]bool{}
	for _, f := range files {
		for _, node := range pyast.Walk(f.tree) {
			attr, _ := node.Str("attr")
			if !node.Is("Attribute") || !private[attr] || own[attr] {
				continue
			}
			if id, _ := node.Child("value").Str("id"); node.Child("value").Is("Name") && (id == "self" || id == "cls") {
				continue
			}
			expr, err := pyast.Unparse(node)
			if err != nil {
				return nil, err
			}
			if runes := pystr.Runes(expr); len(runes) > 80 {
				expr = pystr.FromRunes(runes[:80])
			}
			found[site{f.rel, node.Lineno, expr}] = true
		}
	}
	sorted := make([]site, 0, len(found))
	for s := range found {
		sorted = append(sorted, s)
	}
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.rel != b.rel {
			return a.rel < b.rel
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.expr < b.expr
	})
	out := make([]string, len(sorted))
	for i, s := range sorted {
		out[i] = fmt.Sprintf("%s:%d %s", s.rel, s.line, s.expr)
	}
	return out, nil
}

func warnPrivateSites(report *ValidationReport, label string, hits []string) {
	more := ""
	if len(hits) > privateImportsShown {
		more = fmt.Sprintf("; and %d more", len(hits)-privateImportsShown)
	}
	report.warn(label + " (catalog rule 9: public surfaces only; these can break on any release): " +
		strings.Join(hits[:min(privateImportsShown, len(hits))], "; ") + more +
		". Use ctx.* / documented plugin APIs, or open an issue for the seam you need.")
}

// Warns, never fails, on imports of private core modules and private methods called on core handles
func checkPrivateImports(report *ValidationReport, pluginDir string) error {
	files, err := privateParsedFiles(pluginDir)
	if err != nil {
		return err
	}
	hits, err := privateImportFindings(files)
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		warnPrivateSites(report, "private core imports", hits)
	}
	if hits, err = privateAttributeFindings(files); err != nil {
		return err
	}
	if len(hits) > 0 {
		warnPrivateSites(report, "private core attribute access", hits)
	}
	return nil
}
