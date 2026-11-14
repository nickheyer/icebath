package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	MigrationMarker    = "# managed by icebath — `icebath codex-runtime migrate` regenerates this section"
	MigrationEndMarker = "# end icebath managed section"
)

var knownServerKeys = map[string]bool{
	"command": true, "args": true, "env": true, "cwd": true, "url": true, "headers": true, "transport": true,
	"timeout": true, "connect_timeout": true, "enabled": true, "description": true,
}

var legacyManagedHeaders = []string{"[mcp_servers", "[plugins", "[permissions]", "[permissions."}

type Server struct {
	Name   string
	Config json.RawMessage
}

type MigrateOptions struct {
	Servers           []Server
	CodexHome         string
	DryRun            bool
	DiscoverPlugins   bool
	PermissionProfile string
	ToolsServerName   string
	ToolsCommand      []string
	Bin               string
	Env               []string
}

type MigrationReport struct {
	TargetPath       string
	Migrated         []string
	SkippedKeys      map[string][]string
	MigratedPlugins  []string
	PluginQueryError string
	WrotePermissions string
	PreservedServers []string
	Errors           []string
	Written          bool
	DryRun           bool
}

type plugin struct {
	name, marketplace string
	enabled           bool
}

type tomlEntry struct {
	key   string
	value string
}

func (r *MigrationReport) Summary() string {
	var lines []string
	if r.DryRun {
		lines = append(lines, "(dry run) Would write "+r.TargetPath)
	} else if r.Written {
		lines = append(lines, "Wrote "+r.TargetPath)
	}
	if len(r.Migrated) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d MCP server(s):", len(r.Migrated)))
		for _, name := range r.Migrated {
			note := ""
			if skipped := r.SkippedKeys[name]; len(skipped) > 0 {
				note = " (skipped: " + strings.Join(skipped, ", ") + ")"
			}
			lines = append(lines, "  - "+name+note)
		}
	} else {
		lines = append(lines, "No MCP servers found in icebath config.")
	}
	if len(r.MigratedPlugins) > 0 {
		lines = append(lines, fmt.Sprintf("Migrated %d native Codex plugin(s):", len(r.MigratedPlugins)))
		for _, name := range r.MigratedPlugins {
			lines = append(lines, "  - "+name)
		}
	} else if r.PluginQueryError != "" {
		lines = append(lines, "Codex plugin discovery skipped: "+r.PluginQueryError)
	}
	if r.WrotePermissions != "" {
		lines = append(lines, "Wrote default_permissions = '"+r.WrotePermissions+"'")
	}
	if len(r.PreservedServers) > 0 {
		lines = append(lines, fmt.Sprintf("Kept %d user-owned MCP server(s) already in config.toml (icebath projection skipped): %s",
			len(r.PreservedServers), strings.Join(r.PreservedServers, ", ")))
	}
	for _, e := range r.Errors {
		lines = append(lines, "⚠ "+e)
	}
	return strings.Join(lines, "\n")
}

func tomlString(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\b", "\\b", "\t", "\\t", "\n", "\\n", "\f", "\\f", "\r", "\\r")
	return "\"" + replacer.Replace(value) + "\""
}

func tomlKey(key string) string {
	bare := key != ""
	for _, r := range key {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			bare = false
		}
	}
	if bare {
		return key
	}
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(key) + "\""
}

func tomlFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func tomlStringMap(v wire.Value) string {
	var items []string
	for _, f := range v.Fields() {
		items = append(items, tomlKey(f.Key)+" = "+tomlString(pyDisplay(f.Value)))
	}
	if len(items) == 0 {
		return "{}"
	}
	return "{ " + strings.Join(items, ", ") + " }"
}

func tomlStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = tomlString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func numeric(v wire.Value) (float64, bool) {
	if f, ok := v.Float(); ok {
		return f, true
	}
	if b, ok := v.Bool(); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	if s, ok := v.Str(); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return 0, false
}

func translateServer(cfg wire.Value) ([]tomlEntry, []string, bool) {
	if !cfg.IsObject() {
		return nil, nil, false
	}
	var out []tomlEntry
	var skipped []string
	switch {
	case cfg.Get("command").Truthy():
		if cfg.Get("url").Truthy() {
			skipped = append(skipped, "url (both command and url set; preferring stdio)")
		}
		out = append(out, tomlEntry{"command", tomlString(pyDisplay(cfg.Get("command")))})
		if args := cfg.Get("args"); args.Truthy() {
			var list []string
			for _, a := range args.Elems() {
				list = append(list, pyDisplay(a))
			}
			out = append(out, tomlEntry{"args", tomlStringList(list)})
		}
		if env := cfg.Get("env"); env.Truthy() {
			out = append(out, tomlEntry{"env", tomlStringMap(env)})
		}
		if cwd := cfg.Get("cwd"); cwd.Truthy() {
			out = append(out, tomlEntry{"cwd", tomlString(pyDisplay(cwd))})
		}
	case cfg.Get("url").Truthy():
		out = append(out, tomlEntry{"url", tomlString(pyDisplay(cfg.Get("url")))})
		if headers := cfg.Get("headers"); headers.Truthy() {
			out = append(out, tomlEntry{"http_headers", tomlStringMap(headers)})
		}
		if cfg.Get("transport").Text() == "sse" {
			skipped = append(skipped, "transport=sse (codex auto-negotiates)")
		}
	default:
		return nil, []string{"no command or url field"}, false
	}
	for _, t := range [][3]string{{"timeout", "tool_timeout_sec", "timeout (not numeric)"}, {"connect_timeout", "startup_timeout_sec", "connect_timeout (not numeric)"}} {
		if v := cfg.Get(t[0]); v.Exists() {
			if f, ok := numeric(v); ok {
				out = append(out, tomlEntry{t[1], tomlFloat(f)})
			} else {
				skipped = append(skipped, t[2])
			}
		}
	}
	if cfg.Get("enabled").False() {
		out = append(out, tomlEntry{"enabled", "false"})
	}
	for _, f := range cfg.Fields() {
		switch {
		case f.Key == "sampling":
			skipped = append(skipped, f.Key+" (no codex equivalent)")
		case !knownServerKeys[f.Key]:
			skipped = append(skipped, f.Key+" (unknown icebath key)")
		}
	}
	return out, skipped, true
}

func renderSection(servers map[string][]tomlEntry, plugins []plugin, profile string) string {
	out := []string{MigrationMarker}
	if len(servers) == 0 && len(plugins) == 0 && profile == "" {
		out = append(out, "# (no MCP servers, plugins, or permissions configured by icebath)", MigrationEndMarker)
		return strings.Join(out, "\n") + "\n"
	}
	if profile != "" {
		if !strings.HasPrefix(profile, ":") {
			profile = ":" + profile
		}
		out = append(out, "", "default_permissions = "+tomlString(profile))
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, "", "[mcp_servers."+tomlKey(name)+"]")
		for _, e := range servers[name] {
			out = append(out, tomlKey(e.key)+" = "+e.value)
		}
	}
	sorted := append([]plugin(nil), plugins...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].name+"@"+sorted[i].marketplace < sorted[j].name+"@"+sorted[j].marketplace
	})
	for _, p := range sorted {
		marketplace := p.marketplace
		if marketplace == "" {
			marketplace = "openai-curated"
		}
		out = append(out, "", "[plugins."+tomlKey(p.name+"@"+marketplace)+"]", "enabled = "+strconv.FormatBool(p.enabled))
	}
	out = append(out, "", MigrationEndMarker)
	return strings.Join(out, "\n") + "\n"
}

func splitLines(text string) []string {
	var lines []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

func looksLikeTableHeader(stripped string) bool {
	if !strings.HasPrefix(stripped, "[") {
		return false
	}
	head, _, _ := strings.Cut(stripped, "#")
	head = strings.TrimRight(head, " \t\r\n\f\v")
	end := strings.Index(head, "]")
	return strings.HasSuffix(head, "]") && end >= 0 && !strings.Contains(head[:end+1], "=")
}

func leftStrip(line string) string { return strings.TrimLeft(line, " \t\r\n\f\v") }

func insertManagedBlock(userText, block string) string {
	if strings.TrimSpace(userText) == "" {
		return block
	}
	lines := splitLines(userText)
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(leftStrip(line), "[") {
			first = i
			break
		}
	}
	if first < 0 {
		if prefix := strings.TrimRight(userText, "\n"); prefix != "" {
			return prefix + "\n\n" + block
		}
		return block
	}
	prefix := strings.TrimRight(strings.Join(lines[:first], ""), "\n")
	suffix := strings.TrimLeft(strings.Join(lines[first:], ""), "\n")
	if prefix != "" {
		return prefix + "\n\n" + block + "\n" + suffix
	}
	return block + "\n" + suffix
}

func stripUnmanagedPluginTables(text string) string {
	var out strings.Builder
	inPlugin := false
	for _, line := range splitLines(text) {
		stripped := leftStrip(line)
		if looksLikeTableHeader(stripped) {
			inPlugin = strings.HasPrefix(stripped, "[plugins.")
		}
		if !inPlugin {
			out.WriteString(line)
		}
	}
	return out.String()
}

func unmanagedServerNames(text string) map[string]bool {
	names := map[string]bool{}
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(text), &parsed); err == nil {
		if servers, ok := parsed["mcp_servers"].(map[string]any); ok {
			for name := range servers {
				names[name] = true
			}
			return names
		}
	}
	for _, line := range strings.Split(text, "\n") {
		stripped := leftStrip(line)
		if !looksLikeTableHeader(stripped) || !strings.HasPrefix(stripped, "[mcp_servers.") {
			continue
		}
		part := strings.TrimSpace(stripped[1:strings.Index(stripped, "]")][len("mcp_servers."):])
		if strings.HasPrefix(part, "\"") {
			end := strings.Index(part[1:], "\"")
			if end < 0 {
				continue
			}
			part = part[1 : 1+end]
		} else {
			part, _, _ = strings.Cut(part, ".")
		}
		if part != "" {
			names[part] = true
		}
	}
	return names
}

func stripManagedBlock(text string) string {
	var out strings.Builder
	inManaged := false
	for _, line := range splitLines(text) {
		bare := strings.TrimRight(line, "\n")
		if bare == MigrationMarker {
			inManaged = true
			continue
		}
		if inManaged {
			if bare == MigrationEndMarker {
				inManaged = false
				continue
			}
			stripped := leftStrip(line)
			if !strings.HasPrefix(stripped, "[") || hasPrefixAny(stripped, legacyManagedHeaders) {
				continue
			}
			inManaged = false
		}
		out.WriteString(line)
	}
	return out.String()
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func queryPlugins(ctx context.Context, o MigrateOptions, home string) ([]plugin, string) {
	c, err := startClient(o.Bin, home, nil, o.Env)
	if err != nil {
		return nil, "plugin/list query failed: " + err.Error()
	}
	defer c.close()
	if err := c.initialize(ctx, "icebath-migration", "icebath", "0.1"); err != nil {
		return nil, "plugin/list query failed: " + err.Error()
	}
	resp, err := c.request(ctx, "plugin/list", nil, 8*time.Second)
	if err != nil {
		return nil, "plugin/list query failed: " + err.Error()
	}
	marketplaces := resp.Get("marketplaces")
	if marketplaces.Truthy() && !marketplaces.IsArray() {
		return nil, "plugin/list response missing 'marketplaces'"
	}
	seen := map[string]bool{}
	var out []plugin
	for _, market := range marketplaces.Elems() {
		if !market.IsObject() {
			continue
		}
		marketName := orText(market.Get("name"), "openai-curated")
		for _, p := range market.Get("plugins").Elems() {
			if !p.IsObject() || !p.Get("installed").Truthy() {
				continue
			}
			if availability := strings.ToUpper(orText(p.Get("availability"), "")); availability != "" && availability != "AVAILABLE" {
				continue
			}
			name := orText(p.Get("name"), "")
			if name == "" || seen[name+"\x00"+marketName] {
				continue
			}
			seen[name+"\x00"+marketName] = true
			enabled := p.Get("enabled")
			out = append(out, plugin{name: name, marketplace: marketName, enabled: !enabled.Exists() || enabled.Truthy()})
		}
	}
	return out, ""
}

func toolsEntry(command []string) []tomlEntry {
	var env []string
	if v := os.Getenv("ICEBATH_HOME"); v != "" {
		env = append(env, "ICEBATH_HOME = "+tomlString(v))
	}
	env = append(env, "ICEBATH_QUIET = "+tomlString("1"), "ICEBATH_REDACT_SECRETS = "+tomlString("true"))
	return []tomlEntry{
		{"command", tomlString(command[0])},
		{"args", tomlStringList(command[1:])},
		{"env", "{ " + strings.Join(env, ", ") + " }"},
		{"startup_timeout_sec", "30.0"},
		{"tool_timeout_sec", "600.0"},
	}
}

func remove(list []string, name string) []string {
	out := list[:0]
	for _, item := range list {
		if item != name {
			out = append(out, item)
		}
	}
	return out
}

func Migrate(ctx context.Context, o MigrateOptions) *MigrationReport {
	report := &MigrationReport{DryRun: o.DryRun, SkippedKeys: map[string][]string{}}
	home := o.CodexHome
	if home == "" {
		home = strings.TrimSpace(os.Getenv("CODEX_HOME"))
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			report.Errors = append(report.Errors, "could not locate the codex home: "+err.Error())
			return report
		}
		home = filepath.Join(userHome, ".codex")
	}
	target := filepath.Join(home, "config.toml")
	report.TargetPath = target
	translated := map[string][]tomlEntry{}
	for _, server := range o.Servers {
		cfg, err := wire.ParseValue(server.Config)
		if err != nil || !cfg.Truthy() {
			cfg, _ = wire.ParseValue([]byte("{}"))
		}
		entries, skipped, ok := translateServer(cfg)
		if !ok {
			reason := strings.Join(skipped, ", ")
			if reason == "" {
				reason = "no transport configured"
			}
			report.Errors = append(report.Errors, fmt.Sprintf("server '%s' skipped: %s", server.Name, reason))
			continue
		}
		translated[server.Name] = entries
		if len(skipped) > 0 {
			report.SkippedKeys[server.Name] = skipped
		}
		report.Migrated = append(report.Migrated, server.Name)
	}
	var plugins []plugin
	pluginsKnown := false
	if o.DiscoverPlugins && !o.DryRun {
		found, problem := queryPlugins(ctx, o, home)
		plugins, report.PluginQueryError, pluginsKnown = found, problem, problem == ""
		for _, p := range plugins {
			report.MigratedPlugins = append(report.MigratedPlugins, p.name+"@"+p.marketplace)
		}
	}
	report.WrotePermissions = o.PermissionProfile
	if len(o.ToolsCommand) > 0 && o.ToolsServerName != "" {
		if _, present := translated[o.ToolsServerName]; !present {
			report.Migrated = append(report.Migrated, o.ToolsServerName)
		}
		translated[o.ToolsServerName] = toolsEntry(o.ToolsCommand)
	}
	withoutManaged := ""
	if data, err := os.ReadFile(target); err == nil {
		existing := strings.TrimPrefix(strings.ToValidUTF8(string(data), string(utf8.RuneError)), string(rune(0xFEFF)))
		if report.PluginQueryError != "" {
			var probe map[string]any
			if toml.Unmarshal([]byte(existing), &probe) != nil {
				report.PluginQueryError += "; existing config.toml was unloadable — re-run `icebath codex-runtime migrate` to migrate plugins"
			}
		}
		withoutManaged = stripManagedBlock(existing)
		if pluginsKnown {
			withoutManaged = stripUnmanagedPluginTables(withoutManaged)
		}
		var owned []string
		for name := range unmanagedServerNames(withoutManaged) {
			if _, ok := translated[name]; ok {
				owned = append(owned, name)
			}
		}
		sort.Strings(owned)
		for _, name := range owned {
			delete(translated, name)
			report.Migrated = remove(report.Migrated, name)
			report.PreservedServers = append(report.PreservedServers, name)
		}
	} else if !os.IsNotExist(err) {
		report.Errors = append(report.Errors, "could not read "+target+": "+err.Error())
		return report
	}
	text := insertManagedBlock(withoutManaged, renderSection(translated, plugins, o.PermissionProfile))
	var probe map[string]any
	if err := toml.Unmarshal([]byte(text), &probe); err != nil {
		report.Errors = append(report.Errors, "refusing to write "+target+": rendered config is not valid TOML ("+err.Error()+")")
		return report
	}
	if o.DryRun {
		return report
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		report.Errors = append(report.Errors, "could not write "+target+": "+err.Error())
		return report
	}
	if err := fileutil.WriteFile(ctx, target, []byte(text), fileutil.TempPrefix(".config.toml."), fileutil.KeepPerm()); err != nil {
		report.Errors = append(report.Errors, "could not write "+target+": "+err.Error())
		return report
	}
	report.Written = true
	return report
}
