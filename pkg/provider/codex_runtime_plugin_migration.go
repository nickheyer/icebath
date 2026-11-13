package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pytoml"
)

// Marker comments wrapping the managed section of codex config.toml so a rerun replaces only its own text
const (
	CodexMigrationMarker    = "# managed by icebath-agent — `icebath codex-runtime migrate` regenerates this section"
	CodexMigrationEndMarker = "# end icebath-agent managed section"
)

// CodexMigrateOptions are the migrate keywords, an empty profile is the workspace built in and the No flag is None
type CodexMigrateOptions struct {
	CodexHome                  string
	DryRun                     bool
	SkipPluginDiscovery        bool
	DefaultPermissionProfile   string
	NoDefaultPermissionProfile bool
	SkipToolsMCPServer         bool
}

// CodexMigrationReport is the outcome of one migration pass
type CodexMigrationReport struct {
	TargetPath              string
	Migrated                []string
	SkippedKeysPerServer    map[string][]string
	MigratedPlugins         []string
	PluginQueryError        string
	WrotePermissionsDefault string
	PreservedUserServers    []string
	Errors                  []string
	Written                 bool
	DryRun                  bool
}

// Human readable lines naming what was written, migrated, skipped, preserved and every error
func (r *CodexMigrationReport) Summary() string {
	var lines []string
	target := r.TargetPath
	if target == "" {
		target = "None"
	}
	if r.DryRun {
		lines = append(lines, "(dry run) Would write "+target)
	} else if r.Written {
		lines = append(lines, "Wrote "+target)
	}
	if len(r.Migrated) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d MCP server(s):", len(r.Migrated)))
		for _, name := range r.Migrated {
			note := ""
			if skipped := r.SkippedKeysPerServer[name]; len(skipped) > 0 {
				note = " (skipped: " + strings.Join(skipped, ", ") + ")"
			}
			lines = append(lines, "  - "+name+note)
		}
	} else {
		lines = append(lines, "No MCP servers found in Icebath config.")
	}
	if len(r.MigratedPlugins) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d native Codex plugin(s):", len(r.MigratedPlugins)))
		for _, name := range r.MigratedPlugins {
			lines = append(lines, "  - "+name)
		}
	} else if r.PluginQueryError != "" {
		lines = append(lines, "Codex plugin discovery skipped: "+r.PluginQueryError)
	}
	if r.WrotePermissionsDefault != "" {
		lines = append(lines, "Wrote default_permissions = "+pyrepr.Repr(r.WrotePermissionsDefault))
	}
	if len(r.PreservedUserServers) > 0 {
		lines = append(lines, fmt.Sprintf("Kept %d user-owned MCP server(s) already in config.toml (Icebath projection skipped): %s",
			len(r.PreservedUserServers), strings.Join(r.PreservedUserServers, ", ")))
	}
	for _, e := range r.Errors {
		lines = append(lines, "⚠ "+e)
	}
	return strings.Join(lines, "\n")
}

// Icebath MCP keys codex understands, the key dropped for having no codex equivalent, and the timeout renames
var (
	codexKnownIcebathKeys = map[string]bool{
		"command": true, "args": true, "env": true, "cwd": true,
		"url": true, "headers": true, "transport": true,
		"timeout": true, "connect_timeout": true,
		"enabled": true, "description": true}
	codexKeysDroppedWithWarning = map[string]bool{"sampling": true}
	codexTimeoutKeys            = [][3]string{
		{"timeout", "tool_timeout_sec", "timeout (not numeric)"},
		{"connect_timeout", "startup_timeout_sec", "connect_timeout (not numeric)"}}
)

// Keys of a config dict in iteration order, sorted for an unordered map, false for a non dict
func codexDictKeys(v any) ([]string, bool) {
	switch o := v.(type) {
	case *pyjson.Object:
		if o == nil {
			return nil, false
		}
		return slices.Clone(o.Keys), true
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys, true
	}
	return nil, false
}

// String keys and values of an env or header table, the AttributeError of a non dict escapes
func codexMigrationStrMap(d any) (*pyjson.Object, error) {
	keys, ok := codexDictKeys(d)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'items'", pyrepr.TypeName(d))
	}
	out := pyjson.NewObject()
	for _, k := range keys {
		out.Set(k, pyrepr.Str(cxGet(d, k)))
	}
	return out, nil
}

// Python list of str over any iterable config value, the TypeError of a non iterable escapes
func codexMigrationStrList(v any) ([]any, error) {
	var out []any
	switch t := v.(type) {
	case []any:
		for _, a := range t {
			out = append(out, pyrepr.Str(a))
		}
	case []string:
		for _, a := range t {
			out = append(out, a)
		}
	case string:
		for _, r := range t {
			out = append(out, string(r))
		}
	default:
		keys, ok := codexDictKeys(v)
		if !ok {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
		}
		for _, k := range keys {
			out = append(out, k)
		}
	}
	return out, nil
}

// Codex inline table entry and skipped key notes for one Icebath MCP server, nil entry when unusable
func codexTranslateOneServer(name string, cfg any) (*pyjson.Object, []string, error) {
	keys, ok := codexDictKeys(cfg)
	if !ok {
		return nil, []string{}, nil
	}
	skipped := []string{}
	out := pyjson.NewObject()
	get := func(k string) any { return cxGet(cfg, k) }
	if cxTruthy(get("command")) {
		if cxTruthy(get("url")) {
			skipped = append(skipped, "url (both command and url set; preferring stdio)")
		}
		out.Set("command", pyrepr.Str(get("command")))
		if cxTruthy(get("args")) {
			args, err := codexMigrationStrList(get("args"))
			if err != nil {
				return nil, nil, err
			}
			out.Set("args", args)
		}
		if cxTruthy(get("env")) {
			env, err := codexMigrationStrMap(get("env"))
			if err != nil {
				return nil, nil, err
			}
			out.Set("env", env)
		}
		if cxTruthy(get("cwd")) {
			out.Set("cwd", pyrepr.Str(get("cwd")))
		}
	} else if cxTruthy(get("url")) {
		out.Set("url", pyrepr.Str(get("url")))
		if cxTruthy(get("headers")) {
			headers, err := codexMigrationStrMap(get("headers"))
			if err != nil {
				return nil, nil, err
			}
			out.Set("http_headers", headers)
		}
		if transport, ok := get("transport").(string); ok && transport == "sse" {
			skipped = append(skipped, "transport=sse (codex auto-negotiates)")
		}
	} else {
		return nil, []string{"no command or url field"}, nil
	}
	for _, spec := range codexTimeoutKeys {
		if !cxHas(cfg, spec[0]) {
			continue
		}
		f, err := pyint.Float(get(spec[0]))
		if err != nil {
			var pyErr *pyint.Error
			if errors.As(err, &pyErr) && (pyErr.Kind == "TypeError" || pyErr.Kind == "ValueError") {
				skipped = append(skipped, spec[2])
				continue
			}
			return nil, nil, err
		}
		out.Set(spec[1], f)
	}
	if enabled, ok := get("enabled").(bool); ok && !enabled {
		out.Set("enabled", false)
	}
	for _, key := range keys {
		if codexKeysDroppedWithWarning[key] {
			skipped = append(skipped, key+" (no codex equivalent)")
		} else if !codexKnownIcebathKeys[key] {
			skipped = append(skipped, key+" (unknown Icebath key)")
		}
	}
	return out, skipped, nil
}

// TOML basic string escapes, backslash first so the others are not escaped twice
var codexTOMLEscapes = [][2]string{
	{"\\", "\\\\"}, {"\"", "\\\""}, {"\b", "\\b"}, {"\t", "\\t"},
	{"\n", "\\n"}, {"\f", "\\f"}, {"\r", "\\r"}}

func codexEscapeTOMLString(value string) string {
	for _, esc := range codexTOMLEscapes {
		value = strings.ReplaceAll(value, esc[0], esc[1])
	}
	return "\"" + value + "\""
}

// TOML text of a string, number, bool or flat list or table of those, other types are a ValueError
func codexFormatTOMLValue(value any) (string, error) {
	if b, ok := value.(bool); ok {
		if b {
			return "true", nil
		}
		return "false", nil
	}
	switch v := value.(type) {
	case float64:
		return pyrepr.Float(v), nil
	case float32:
		return pyrepr.Float(float64(v)), nil
	case string:
		return codexEscapeTOMLString(v), nil
	case []any, []string:
		var parts []string
		for _, item := range cxStringsAsAny(v) {
			text, err := codexFormatTOMLValue(item)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	if n, ok := pyrepr.Int(value); ok {
		return n.String(), nil
	}
	if keys, ok := codexDictKeys(value); ok {
		var items []string
		for _, k := range keys {
			text, err := codexFormatTOMLValue(cxGet(value, k))
			if err != nil {
				return "", err
			}
			items = append(items, codexQuoteTOMLKey(k)+" = "+text)
		}
		if len(items) == 0 {
			return "{}", nil
		}
		return "{ " + strings.Join(items, ", ") + " }", nil
	}
	return "", fmt.Errorf("Unsupported TOML value type: %s", pyrepr.TypeName(value))
}

// Elements of a list value as any, a string slice widened
func cxStringsAsAny(v any) []any {
	if s, ok := v.([]string); ok {
		out := make([]any, len(s))
		for i, x := range s {
			out[i] = x
		}
		return out
	}
	return v.([]any)
}

// Key bare when every character is alphanumeric, dash or underscore, else quoted
func codexQuoteTOMLKey(key string) string {
	bare := key != ""
	for _, c := range key {
		if !pystr.IsAlnum(c) && c != '-' && c != '_' {
			bare = false
			break
		}
	}
	if bare {
		return key
	}
	return "\"" + strings.ReplaceAll(strings.ReplaceAll(key, "\\", "\\\\"), "\"", "\\\"") + "\""
}

// Managed block of default permissions, mcp_servers tables and plugins tables between the two markers
func RenderCodexTOMLSection(servers map[string]*pyjson.Object, plugins []map[string]any, defaultPermissionProfile string) (string, error) {
	out := []string{CodexMigrationMarker}
	if len(servers) == 0 && len(plugins) == 0 && defaultPermissionProfile == "" {
		out = append(out, "# (no MCP servers, plugins, or permissions configured by Icebath)", CodexMigrationEndMarker)
		return strings.Join(out, "\n") + "\n", nil
	}
	if defaultPermissionProfile != "" {
		normalized := defaultPermissionProfile
		if !strings.HasPrefix(normalized, ":") {
			normalized = ":" + normalized
		}
		text, _ := codexFormatTOMLValue(normalized)
		out = append(out, "", "default_permissions = "+text)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, "", "[mcp_servers."+codexQuoteTOMLKey(name)+"]")
		entry := servers[name]
		for _, k := range entry.Keys {
			v, _ := entry.Get(k)
			text, err := codexFormatTOMLValue(v)
			if err != nil {
				return "", err
			}
			out = append(out, codexQuoteTOMLKey(k)+" = "+text)
		}
	}
	getOr := func(p map[string]any, k string) string {
		if v, ok := p[k]; ok {
			return pyrepr.Str(v)
		}
		return ""
	}
	sorted := slices.Clone(plugins)
	sort.SliceStable(sorted, func(i, j int) bool {
		return getOr(sorted[i], "name")+"@"+getOr(sorted[i], "marketplace") < getOr(sorted[j], "name")+"@"+getOr(sorted[j], "marketplace")
	})
	for _, plugin := range sorted {
		qualified := pyrepr.Str(cxOr(plugin["name"], "")) + "@" + pyrepr.Str(cxOr(plugin["marketplace"], "openai-curated"))
		enabled := true
		if v, ok := plugin["enabled"]; ok {
			enabled = cxTruthy(v)
		}
		text, _ := codexFormatTOMLValue(enabled)
		out = append(out, "", "[plugins."+codexQuoteTOMLKey(qualified)+"]", "enabled = "+text)
	}
	out = append(out, "", CodexMigrationEndMarker)
	return strings.Join(out, "\n") + "\n", nil
}

// Places the managed block before the user's first table header so its root keys stay at the root
func codexInsertManagedBlockAtTopLevel(userText, managedBlock string) string {
	if pystr.Strip(userText) == "" {
		return managedBlock
	}
	lines := pystr.Splitlines(userText, true)
	first := -1
	for i, ln := range lines {
		if strings.HasPrefix(pystr.Lstrip(ln), "[") {
			first = i
			break
		}
	}
	if first < 0 {
		prefix := pystr.RstripChars(userText, "\n")
		if prefix == "" {
			return managedBlock
		}
		return prefix + "\n\n" + managedBlock
	}
	prefix := pystr.RstripChars(strings.Join(lines[:first], ""), "\n")
	suffix := pystr.LstripChars(strings.Join(lines[first:], ""), "\n")
	if prefix == "" {
		return managedBlock + "\n" + suffix
	}
	return prefix + "\n\n" + managedBlock + "\n" + suffix
}

// Drops every plugins table outside the managed block, multi line array continuations included
func codexStripUnmanagedPluginTables(tomlText string) string {
	var out strings.Builder
	inPluginTable := false
	for _, line := range pystr.Splitlines(tomlText, true) {
		stripped := pystr.Lstrip(line)
		if codexLooksLikeTableHeader(stripped) {
			inPluginTable = strings.HasPrefix(stripped, "[plugins.")
		}
		if !inPluginTable {
			out.WriteString(line)
		}
	}
	return out.String()
}

// Names of the mcp_servers the user declares outside the managed block, the parsed document first then header lines
func codexUnmanagedMCPServerNames(tomlText string) (map[string]bool, error) {
	names := map[string]bool{}
	if doc, err := pytoml.Loads(tomlText); err == nil {
		if parsed, ok := doc["mcp_servers"].(map[string]any); ok {
			for name := range parsed {
				names[name] = true
			}
			return names, nil
		}
	}
	for _, line := range pystr.Splitlines(tomlText, false) {
		stripped := pystr.Lstrip(line)
		if !codexLooksLikeTableHeader(stripped) || !strings.HasPrefix(stripped, "[mcp_servers.") {
			continue
		}
		namePart := pystr.Strip(stripped[1:strings.Index(stripped, "]")][len("mcp_servers."):])
		if strings.HasPrefix(namePart, "\"") {
			end := strings.Index(namePart[1:], "\"")
			if end < 0 {
				return nil, errors.New("substring not found")
			}
			namePart = namePart[1 : 1+end]
		} else {
			namePart = strings.SplitN(namePart, ".", 2)[0]
		}
		if namePart != "" {
			names[namePart] = true
		}
	}
	return names, nil
}

// True for a bracketed table header line whose closing bracket comes before any equals sign
func codexLooksLikeTableHeader(strippedLine string) bool {
	if !strings.HasPrefix(strippedLine, "[") {
		return false
	}
	head := pystr.Rstrip(strings.SplitN(strippedLine, "#", 2)[0])
	return strings.HasSuffix(head, "]") && !strings.Contains(head[:strings.Index(head, "]")+1], "=")
}

// Section headers an old managed block without an end marker may contain
var codexLegacyManagedHeaders = []string{"[mcp_servers", "[plugins", "[permissions]", "[permissions."}

// Removes the prior managed section between the markers, a legacy block ending at its first foreign table
func codexStripExistingManagedBlock(tomlText string) string {
	var out strings.Builder
	inManaged := false
	for _, line := range pystr.Splitlines(tomlText, true) {
		bare := pystr.RstripChars(line, "\n")
		if bare == CodexMigrationMarker {
			inManaged = true
			continue
		}
		if inManaged {
			if bare == CodexMigrationEndMarker {
				inManaged = false
				continue
			}
			stripped := pystr.Lstrip(line)
			legacy := slices.ContainsFunc(codexLegacyManagedHeaders, func(h string) bool { return strings.HasPrefix(stripped, h) })
			if !strings.HasPrefix(stripped, "[") || legacy {
				continue
			}
			inManaged = false
		}
		out.WriteString(line)
	}
	return out.String()
}

// Client constructor the plugin query spawns through, the codex_app_server CodexAppServerClient class
var codexPluginQueryClient = func(opts CodexAppServerOptions) (CodexRPCClient, error) {
	return NewCodexAppServerClient(opts)
}

// Installed and available codex plugins from a brief plugin/list, a query failure is the returned reason
var codexQueryCodexPlugins = func(codexHome string, timeout float64, codexBin string) ([]map[string]any, string, error) {
	resp, err := codexPluginListResponse(codexHome, timeout, codexBin)
	if err != nil {
		return []map[string]any{}, "plugin/list query failed: " + err.Error(), nil
	}
	if !cxIsObject(resp) {
		return nil, "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(resp))
	}
	marketplaces, ok := cxOr(cxGet(resp, "marketplaces"), []any{}).([]any)
	if !ok {
		return []map[string]any{}, "plugin/list response missing 'marketplaces'", nil
	}
	out := []map[string]any{}
	seen := map[[2]string]bool{}
	for _, marketplace := range marketplaces {
		if !cxIsObject(marketplace) {
			continue
		}
		marketName := cxStr(marketplace, "name", "openai-curated")
		plugins, _ := cxOr(cxGet(marketplace, "plugins"), []any{}).([]any)
		for _, plugin := range plugins {
			if !cxIsObject(plugin) || !cxTruthy(cxGet(plugin, "installed")) {
				continue
			}
			availability := pystr.Upper(cxStr(plugin, "availability", ""))
			if availability != "" && availability != "AVAILABLE" {
				slog.Debug(fmt.Sprintf("skipping plugin %s: availability=%s", pyrepr.Str(cxGet(plugin, "name")), availability))
				continue
			}
			name := cxStr(plugin, "name", "")
			if name == "" || seen[[2]string{name, marketName}] {
				continue
			}
			seen[[2]string{name, marketName}] = true
			enabled := !cxHas(plugin, "enabled") || cxTruthy(cxGet(plugin, "enabled"))
			out = append(out, map[string]any{"name": name, "marketplace": marketName, "enabled": enabled})
		}
	}
	return out, "", nil
}

// Spawns codex app-server, initializes as the migration client and returns the plugin/list result
func codexPluginListResponse(codexHome string, timeout float64, codexBin string) (any, error) {
	client, err := codexPluginQueryClient(CodexAppServerOptions{CodexBin: codexBin, CodexHome: codexHome})
	if err != nil {
		return nil, err
	}
	defer client.Close(3 * time.Second)
	if _, err := client.Initialize("icebath-migration", "Icebath Agent", "0.1", nil, 10*time.Second); err != nil {
		return nil, err
	}
	return client.Request("plugin/list", pyjson.NewObject(), time.Duration(timeout*float64(time.Second)))
}

// Path fragments of pytest and macOS transient temp homes
var codexTestTempdirNeedles = []string{"pytest-of-", "/pytest-", "/tmp/pytest", "/private/var/folders/"}

// True when the path looks like a transient test temp dir that would be reaped later
func codexLooksLikeTestTempdir(path string) bool {
	if path == "" {
		return false
	}
	lower := pystr.Lower(path)
	return slices.ContainsFunc(codexTestTempdirNeedles, func(n string) bool { return strings.Contains(lower, n) })
}

// Stdio entry launching this binary as the Icebath tools MCP server, the home passed only when set and not transient
func codexBuildToolsMCPEntry() *pyjson.Object {
	env := pyjson.NewObject()
	if home := os.Getenv("ICEBATH_HOME"); home != "" && !codexLooksLikeTestTempdir(home) {
		env.Set("ICEBATH_HOME", home)
	}
	if pythonPath := os.Getenv("PYTHONPATH"); pythonPath != "" {
		env.Set("PYTHONPATH", pythonPath)
	}
	env.Set("ICEBATH_QUIET", "1")
	env.Set("ICEBATH_REDACT_SECRETS", "true")
	exe, _ := os.Executable()
	entry := pyjson.NewObject()
	entry.Set("command", exe)
	entry.Set("args", []any{CodexToolsMCPServerEntry})
	entry.Set("env", env)
	entry.Set("startup_timeout_sec", 30.0)
	entry.Set("tool_timeout_sec", 600.0)
	return entry
}

// Atomic rewrite of config.toml carrying an existing file's permission bits and owner across
func codexWriteAtomic(ctx context.Context, target, text string) error {
	return pyfs.AtomicWriteTextWith(ctx, target, text, pyfs.TextWrite{Prefix: ".config.toml.", PreserveMode: true})
}

// Codex home of the options, else CODEX_HOME, else the user's .codex directory
func codexMigrationHome(opts CodexMigrateOptions) (string, error) {
	if opts.CodexHome != "" {
		return pypath.PathStr(opts.CodexHome), nil
	}
	raw := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if raw == "" {
		userHome, err := pypath.PathHome()
		if err != nil {
			return "", err
		}
		raw = pypath.PathJoin(userHome, ".codex")
	}
	return pypath.PathExpanduser(pypath.PathStr(raw))
}

// Projects the Icebath mcp_servers, installed codex plugins and the tools server into the managed block of codex config.toml
func MigrateCodexRuntimePlugins(ctx context.Context, cfg map[string]any, opts CodexMigrateOptions) (*CodexMigrationReport, error) {
	report := &CodexMigrationReport{DryRun: opts.DryRun, SkippedKeysPerServer: map[string][]string{}}
	codexHome, err := codexMigrationHome(opts)
	if err != nil {
		return nil, err
	}
	target := pypath.PathJoin(codexHome, "config.toml")
	report.TargetPath = target
	var rawServers any
	if len(cfg) > 0 {
		rawServers = cfg["mcp_servers"]
	}
	icebathServers := cxOr(rawServers, map[string]any{})
	serverNames, ok := codexDictKeys(icebathServers)
	if !ok {
		report.Errors = append(report.Errors, "mcp_servers in Icebath config is not a dict; cannot migrate.")
		return report, nil
	}
	translated := map[string]*pyjson.Object{}
	for _, name := range serverNames {
		out, skipped, err := codexTranslateOneServer(name, cxOr(cxGet(icebathServers, name), map[string]any{}))
		if err != nil {
			return nil, err
		}
		if out == nil {
			reason := strings.Join(skipped, ", ")
			if reason == "" {
				reason = "no transport configured"
			}
			report.Errors = append(report.Errors, fmt.Sprintf("server %s skipped: %s", pyrepr.Repr(name), reason))
			continue
		}
		translated[name] = out
		if len(skipped) > 0 {
			report.SkippedKeysPerServer[name] = skipped
		}
		report.Migrated = append(report.Migrated, name)
	}
	var plugins []map[string]any
	pluginQuerySucceeded := false
	if !opts.SkipPluginDiscovery && !opts.DryRun {
		found, pluginErr, err := codexQueryCodexPlugins(codexHome, 8.0, GetConfiguredCodexBinary(cfg))
		if err != nil {
			return nil, err
		}
		plugins = found
		if pluginErr != "" {
			report.PluginQueryError = pluginErr
		}
		pluginQuerySucceeded = pluginErr == ""
		for _, p := range plugins {
			name, hasName := p["name"]
			marketplace, hasMarket := p["marketplace"]
			if !hasName {
				return nil, errors.New("'name'")
			}
			if !hasMarket {
				return nil, errors.New("'marketplace'")
			}
			report.MigratedPlugins = append(report.MigratedPlugins, pyrepr.Str(name)+"@"+pyrepr.Str(marketplace))
		}
	}
	profile := ""
	if !opts.NoDefaultPermissionProfile {
		profile = opts.DefaultPermissionProfile
		if profile == "" {
			profile = ":workspace"
		}
		report.WrotePermissionsDefault = profile
	}
	if !opts.SkipToolsMCPServer {
		translated[CodexToolsMCPServerName] = codexBuildToolsMCPEntry()
		if !slices.Contains(report.Migrated, CodexToolsMCPServerName) {
			report.Migrated = append(report.Migrated, CodexToolsMCPServerName)
		}
	}
	withoutManaged := ""
	if _, statErr := os.Stat(target); statErr == nil {
		existing, err := codexReadConfigText(target)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("could not read %s: %s", target, err))
			return report, nil
		}
		if report.PluginQueryError != "" {
			if _, err := pytoml.Loads(existing); err != nil {
				report.PluginQueryError += "; existing config.toml was unloadable — re-run `icebath codex-runtime migrate` to migrate plugins"
			}
		}
		withoutManaged = codexStripExistingManagedBlock(existing)
		if pluginQuerySucceeded {
			withoutManaged = codexStripUnmanagedPluginTables(withoutManaged)
		}
		userNames, err := codexUnmanagedMCPServerNames(withoutManaged)
		if err != nil {
			return nil, err
		}
		var shared []string
		for name := range userNames {
			if _, ok := translated[name]; ok {
				shared = append(shared, name)
			}
		}
		sort.Strings(shared)
		for _, name := range shared {
			delete(translated, name)
			if i := slices.Index(report.Migrated, name); i >= 0 {
				report.Migrated = slices.Delete(report.Migrated, i, i+1)
			}
			report.PreservedUserServers = append(report.PreservedUserServers, name)
		}
	}
	managedBlock, err := RenderCodexTOMLSection(translated, plugins, profile)
	if err != nil {
		return nil, err
	}
	newText := codexInsertManagedBlockAtTopLevel(withoutManaged, managedBlock)
	if _, err := pytoml.Loads(newText); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("refusing to write %s: rendered config is not valid TOML (%s)", target, err))
		return report, nil
	}
	if opts.DryRun {
		return report, nil
	}
	if err := os.MkdirAll(codexHome, 0o777); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("could not write %s: %s", target, pyfs.OSErrorText(err)))
		return report, nil
	}
	if err := codexWriteAtomic(ctx, target, newText); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("could not write %s: %s", target, pyfs.OSErrorText(err)))
		return report, nil
	}
	report.Written = true
	return report, nil
}

// Text of config.toml read as utf-8-sig with universal newlines, the OSError or UnicodeDecodeError text on failure
func codexReadConfigText(target string) (string, error) {
	data, err := os.ReadFile(target)
	if err != nil {
		return "", errors.New(pyfs.OSErrorText(err))
	}
	text, err := pycodecs.Decode(data, "utf-8-sig")
	if err != nil {
		return "", err
	}
	return pystr.TranslateNewlines(text), nil
}
