package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Schema ids of the portable Agent Plugins v1 manifests
const (
	PluginSchemaV1 = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	MCPSchemaV1    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// Vendor extension namespace carrying icebath specific portable fields
const portableExtension = "com.nousresearch.hermes"

var portablePluginFields = map[string]bool{"$schema": true, "name": true, "version": true, "description": true, "author": true, "homepage": true, "repository": true, "license": true, "keywords": true, "extensions": true}
var portableAuthorFields = map[string]bool{"name": true, "email": true, "url": true}
var portableStdioFields = map[string]bool{"type": true, "command": true, "args": true, "env": true, "cwd": true}
var portableRemoteFields = map[string]bool{"type": true, "url": true, "headers": true}
var portablePluginNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
var portableSkillNameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var portablePlaceholderRe = regexp.MustCompile(`\$\{(PLUGIN_ROOT|PLUGIN_DATA)\}`)
var headerNameRe = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
var serverDeclarationKeys = map[string]bool{"app": true, "requires": true, "liveness": true, "trust": true, "timeout": true, "costs": true, "spend_limit": true, "cost_unit": true}
var serverPolicyKeys = map[string]bool{"trust": true, "timeout": true, "costs": true, "spend_limit": true, "cost_unit": true}

// Fatal portable manifest validation failure
type AgentPluginError struct{ Msg string }

func (e *AgentPluginError) Error() string { return e.Msg }

func agentErr(format string, args ...any) error {
	return &AgentPluginError{Msg: fmt.Sprintf(format, args...)}
}

// One non fatal portable package problem
type AgentPluginDiagnostic struct {
	Scope   string
	Message string
}

// One skill a portable package ships
type AgentPluginSkill struct {
	Name        string
	Description string
	Root        string
	SkillMD     string
	Frontmatter map[string]any
}

// One desktop app declaration of a portable package server
type AgentPluginServerDeclaration struct {
	App      map[string]any
	Requires map[string]any
	Liveness map[string]any
}

// Validated and translated portable package
type AgentPluginPackage struct {
	Name               string
	Version            string
	Description        string
	Root               string
	DataRoot           string
	Manifest           map[string]any
	Skills             []AgentPluginSkill
	MCPServers         map[string]map[string]any
	ServerDeclarations map[string]AgentPluginServerDeclaration
	Diagnostics        []AgentPluginDiagnostic
}

// Reports whether a path resolves inside root
func insideRoot(path, root string) bool {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	target := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		target = r
	} else if a, err := filepath.Abs(path); err == nil {
		target = a
	}
	rel, err := filepath.Rel(rr, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Reports whether every item is a string
func allStrings(items []any) bool {
	for _, it := range items {
		if _, ok := it.(string); !ok {
			return false
		}
	}
	return true
}

// Reports whether v is a string to string mapping
func stringMap(v any) bool {
	m := asMap(v)
	if m == nil {
		return false
	}
	for _, val := range m {
		if _, ok := val.(string); !ok {
			return false
		}
	}
	return true
}

// Parsed JSON object of a file
func readJSONObject(path, label string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, agentErr("%s is not valid readable JSON: %v", label, err)
	}
	var v any
	if err := json.Unmarshal(stripBOM(raw), &v); err != nil {
		return nil, agentErr("%s is not valid readable JSON: %v", label, err)
	}
	m := asMap(v)
	if m == nil {
		return nil, agentErr("%s must contain a JSON object", label)
	}
	return m, nil
}

// Validated plugin.json mapping with unknown fields dropped as diagnostics
func validatePortableManifest(root string) (map[string]any, []AgentPluginDiagnostic, error) {
	path := filepath.Join(root, "plugin.json")
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || !insideRoot(path, root) {
		return nil, nil, agentErr("plugin.json must be a regular file within the plugin root")
	}
	manifest, err := readJSONObject(path, "plugin.json")
	if err != nil {
		return nil, nil, err
	}
	var diags []AgentPluginDiagnostic
	var unknown []string
	for k := range manifest {
		if !portablePluginFields[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		diags = append(diags, AgentPluginDiagnostic{"manifest", "ignored unknown top-level field: " + k})
		delete(manifest, k)
	}
	if manifest["$schema"] != PluginSchemaV1 {
		return nil, nil, agentErr("plugin.json declares an unsupported or missing Agent Plugins schema")
	}
	name, _ := manifest["name"].(string)
	if name == "" || len(name) > 64 || !portablePluginNameRe.MatchString(name) || strings.Contains(name, "--") || strings.Contains(name, "..") {
		return nil, nil, agentErr("plugin.json name does not satisfy v1 constraints")
	}
	for _, f := range []string{"version", "description", "homepage", "repository", "license"} {
		if v, ok := manifest[f]; ok {
			if _, isStr := v.(string); !isStr {
				return nil, nil, agentErr("plugin.json %s must be a string", f)
			}
		}
	}
	if kw, ok := manifest["keywords"]; ok {
		items, isList := kw.([]any)
		if !isList || !allStrings(items) {
			return nil, nil, agentErr("plugin.json keywords must be an array of strings")
		}
	}
	if a, ok := manifest["author"]; ok {
		am := asMap(a)
		if am == nil {
			return nil, nil, agentErr("plugin.json author must be an object")
		}
		for k, v := range am {
			if _, isStr := v.(string); !portableAuthorFields[k] || !isStr {
				return nil, nil, agentErr("plugin.json author may contain only string name, email, and url fields")
			}
		}
	}
	if ext, ok := manifest["extensions"]; ok {
		em := asMap(ext)
		if em == nil {
			diags = append(diags, AgentPluginDiagnostic{"manifest", "ignored non-object extensions field"})
			delete(manifest, "extensions")
		} else {
			for _, v := range em {
				if asMap(v) == nil {
					return nil, nil, agentErr("plugin.json extension namespace values must be objects")
				}
			}
		}
	}
	return normalizeAnyMaps(manifest), diags, nil
}

// The icebath vendor extension mapping of a portable manifest
func manifestVendorExtension(manifest map[string]any) map[string]any {
	if m := asMap(asMap(manifest["extensions"])[portableExtension]); m != nil {
		return m
	}
	return map[string]any{}
}

// Raw config schema a portable manifest declares in the vendor extension
func manifestConfigSchema(manifest map[string]any) any {
	return manifestVendorExtension(manifest)["config_schema"]
}

// Frontmatter mapping of a SKILL.md file
func parseSkillFrontmatter(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(stripBOM(raw))
	if !strings.HasPrefix(content, "---") {
		return nil, errors.New("missing YAML frontmatter")
	}
	rest := content[3:]
	end := regexp.MustCompile(`\n---\s*\n`).FindStringIndex(rest)
	if end == nil {
		return nil, errors.New("unterminated YAML frontmatter")
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(rest[:end[0]]), &parsed); err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %v", err)
	}
	m := asMap(parsed)
	if m == nil {
		return nil, errors.New("YAML frontmatter must be an object")
	}
	return normalizeAnyMaps(m), nil
}

// Validation problem of a skill frontmatter, empty when valid
func validSkillFrontmatter(fm map[string]any, dirName string) string {
	name, _ := fm["name"].(string)
	if name != dirName || name == "" || len(name) > 64 || !portableSkillNameRe.MatchString(name) {
		return "name must match the directory and satisfy Agent Skills constraints"
	}
	desc, ok := fm["description"].(string)
	if !ok || desc == "" || len(desc) > 1024 {
		return "description must be a non-empty string of at most 1024 characters"
	}
	if v, ok := fm["license"]; ok {
		if _, isStr := v.(string); !isStr {
			return "license must be a string"
		}
	}
	if v, ok := fm["compatibility"]; ok {
		s, isStr := v.(string)
		if !isStr || len(s) == 0 || len(s) > 500 {
			return "compatibility must be a string of 1 to 500 characters"
		}
	}
	if v, ok := fm["metadata"]; ok && !stringMap(v) {
		return "metadata must map string keys to string values"
	}
	if v, ok := fm["allowed-tools"]; ok {
		if _, isStr := v.(string); !isStr {
			return "allowed-tools must be a string"
		}
	}
	return ""
}

// Skills under the package skills directory with invalid ones reported
func discoverPortableSkills(root string, diags *[]AgentPluginDiagnostic) []AgentPluginSkill {
	skillsRoot := filepath.Join(root, "skills")
	st, err := os.Lstat(skillsRoot)
	if err != nil {
		return nil
	}
	if real, err := os.Stat(skillsRoot); err != nil || !real.IsDir() || !insideRoot(skillsRoot, root) {
		_ = st
		*diags = append(*diags, AgentPluginDiagnostic{"skills", "skills must be an in-root directory"})
		return nil
	}
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		*diags = append(*diags, AgentPluginDiagnostic{"skills", "cannot list skills: " + err.Error()})
		return nil
	}
	var skills []AgentPluginSkill
	for _, e := range entries {
		child := filepath.Join(skillsRoot, e.Name())
		skillMD := filepath.Join(child, "SKILL.md")
		cst, err := os.Stat(child)
		if err != nil || !cst.IsDir() {
			continue
		}
		mst, err := os.Stat(skillMD)
		if err != nil {
			continue
		}
		problem := ""
		var fm map[string]any
		if !mst.Mode().IsRegular() || !insideRoot(skillMD, root) {
			problem = "SKILL.md must be a regular in-root file"
		} else if fm, err = parseSkillFrontmatter(skillMD); err != nil {
			problem = "invalid SKILL.md: " + err.Error()
		} else {
			problem = validSkillFrontmatter(fm, e.Name())
		}
		if problem != "" {
			*diags = append(*diags, AgentPluginDiagnostic{"skill:" + e.Name(), problem})
			continue
		}
		rootReal, _ := filepath.EvalSymlinks(child)
		mdReal, _ := filepath.EvalSymlinks(skillMD)
		desc, _ := fm["description"].(string)
		skills = append(skills, AgentPluginSkill{Name: e.Name(), Description: desc, Root: rootReal, SkillMD: mdReal, Frontmatter: fm})
	}
	return skills
}

// Value with PLUGIN_ROOT and PLUGIN_DATA placeholders expanded
func expandPlaceholders(value, pluginRoot, dataRoot string) string {
	return portablePlaceholderRe.ReplaceAllStringFunc(value, func(m string) string {
		if strings.Contains(m, "PLUGIN_ROOT") {
			return pluginRoot
		}
		return dataRoot
	})
}

// Resolved path scoped to the plugin root or the data root
func resolveScopedPath(value, pluginRoot, dataRoot string, expand bool) (string, error) {
	expanded := value
	if expand {
		expanded = expandPlaceholders(value, pluginRoot, dataRoot)
	}
	var base, candidate string
	switch {
	case strings.HasPrefix(value, "./"):
		base, candidate = pluginRoot, filepath.Join(pluginRoot, expanded[2:])
	case value == "${PLUGIN_ROOT}" || strings.HasPrefix(value, "${PLUGIN_ROOT}/"):
		base, candidate = pluginRoot, expanded
	case value == "${PLUGIN_DATA}" || strings.HasPrefix(value, "${PLUGIN_DATA}/"):
		base, candidate = dataRoot, expanded
	default:
		return "", errors.New("path must start with ./, ${PLUGIN_ROOT}, or ${PLUGIN_DATA}")
	}
	resolved := filepath.Clean(candidate)
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	baseResolved := filepath.Clean(base)
	if r, err := filepath.EvalSymlinks(base); err == nil {
		baseResolved = r
	}
	rel, err := filepath.Rel(baseResolved, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes its resolved root")
	}
	return resolved, nil
}

// Reports whether a headers value is a well formed header mapping
func validHeaders(v any) bool {
	if v == nil {
		return true
	}
	m := asMap(v)
	if m == nil {
		return false
	}
	seen := map[string]bool{}
	for name, val := range m {
		if !headerNameRe.MatchString(name) || seen[strings.ToLower(name)] {
			return false
		}
		seen[strings.ToLower(name)] = true
		s, ok := val.(string)
		if !ok || strings.ContainsAny(s, "\r\n") {
			return false
		}
	}
	return true
}

// Validated remote MCP url, http allowed on loopback only
func validateRemoteURL(v any) (string, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", errors.New("url must be a non-empty string")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("url is not parseable: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("url scheme must be http or https")
	}
	if u.User != nil {
		return "", errors.New("url must not contain user information")
	}
	if u.Fragment != "" {
		return "", errors.New("url must not contain a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("url must have a host")
	}
	if scheme == "http" && host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("non-loopback url must use https")
		}
	}
	return s, nil
}

// Native MCP config of a portable streamable-http server entry
func translateRemoteServer(cfg map[string]any) (map[string]any, error) {
	for k := range cfg {
		if !portableRemoteFields[k] {
			return nil, errors.New("unknown remote field")
		}
	}
	u, err := validateRemoteURL(cfg["url"])
	if err != nil {
		return nil, err
	}
	if !validHeaders(cfg["headers"]) {
		return nil, errors.New("invalid headers")
	}
	out := map[string]any{"url": u, "strict_redirect_headers": true}
	if h := asMap(cfg["headers"]); len(h) > 0 {
		out["headers"] = h
	}
	return out, nil
}

// Native MCP config of a portable stdio server entry
func translateStdioServer(cfg map[string]any, pluginRoot, dataRoot string, createData bool) (map[string]any, error) {
	for k := range cfg {
		if !portableStdioFields[k] {
			return nil, errors.New("unknown stdio field")
		}
	}
	command, ok := cfg["command"].(string)
	if !ok || command == "" || strings.ContainsRune(command, 0) {
		return nil, errors.New("command must be a non-empty executable token")
	}
	switch {
	case strings.HasPrefix(command, "./"):
		resolved, err := resolveScopedPath(command, pluginRoot, dataRoot, false)
		if err != nil {
			return nil, err
		}
		command = resolved
	case strings.ContainsAny(command, " \t\n\r"):
		return nil, errors.New("command must contain one executable token")
	case strings.ContainsAny(command, `/\`) || command == "." || command == "..":
		return nil, errors.New("command must be a bare executable or begin with ./")
	}
	args := []any{}
	if raw, ok := cfg["args"]; ok {
		items, isList := raw.([]any)
		if !isList || !allStrings(items) {
			return nil, errors.New("args must be an array of strings")
		}
		args = items
	}
	env := map[string]any{}
	if raw, ok := cfg["env"]; ok {
		if !stringMap(raw) {
			return nil, errors.New("env must map string keys to string values")
		}
		env = asMap(raw)
	}
	for k := range env {
		if k == "PLUGIN_ROOT" || k == "PLUGIN_DATA" {
			return nil, errors.New("PLUGIN_ROOT and PLUGIN_DATA are reserved")
		}
	}
	cwdPath := pluginRoot
	if raw, ok := cfg["cwd"]; ok && raw != nil {
		s, isStr := raw.(string)
		if !isStr {
			return nil, errors.New("cwd must be a string")
		}
		resolved, err := resolveScopedPath(s, pluginRoot, dataRoot, true)
		if err != nil {
			return nil, err
		}
		cwdPath = resolved
	}
	if createData {
		if err := os.MkdirAll(dataRoot, 0o700); err != nil {
			return nil, err
		}
		if rel, err := filepath.Rel(dataRoot, cwdPath); err == nil && !strings.HasPrefix(rel, "..") {
			if err := os.MkdirAll(cwdPath, 0o700); err != nil {
				return nil, err
			}
		}
	}
	outEnv := map[string]any{}
	for k, v := range env {
		outEnv[k] = expandPlaceholders(v.(string), pluginRoot, dataRoot)
	}
	outEnv["PLUGIN_ROOT"] = pluginRoot
	outEnv["PLUGIN_DATA"] = dataRoot
	outArgs := make([]any, 0, len(args))
	for _, a := range args {
		outArgs = append(outArgs, expandPlaceholders(a.(string), pluginRoot, dataRoot))
	}
	return map[string]any{"command": command, "args": outArgs, "env": outEnv, "cwd": cwdPath}, nil
}

// Translated mcp.json servers of a package, file level problems reported as one diagnostic
func discoverPortableMCP(root, dataRoot string, diags *[]AgentPluginDiagnostic, createData bool) map[string]map[string]any {
	path := filepath.Join(root, "mcp.json")
	if _, err := os.Lstat(path); err != nil {
		return map[string]map[string]any{}
	}
	fail := func(msg string) map[string]map[string]any {
		*diags = append(*diags, AgentPluginDiagnostic{"mcp", msg})
		return map[string]map[string]any{}
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || !insideRoot(path, root) {
		return fail("mcp.json must be a regular in-root file")
	}
	cfg, err := readJSONObject(path, "mcp.json")
	if err != nil {
		return fail(err.Error())
	}
	if len(cfg) != 2 || cfg["$schema"] == nil || cfg["mcpServers"] == nil {
		return fail("mcp.json has an invalid top-level shape")
	}
	if cfg["$schema"] != MCPSchemaV1 {
		return fail("mcp.json declares an unsupported schema")
	}
	servers := asMap(cfg["mcpServers"])
	if servers == nil {
		return fail("mcpServers must be an object")
	}
	out := map[string]map[string]any{}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		server := asMap(servers[name])
		var translated map[string]any
		var terr error
		switch {
		case name == "" || server == nil:
			terr = errors.New("invalid server entry")
		default:
			switch server["type"] {
			case "stdio":
				translated, terr = translateStdioServer(server, root, dataRoot, createData)
			case "streamable-http":
				translated, terr = translateRemoteServer(server)
			case "sse":
				terr = errors.New("portable sse transport is not supported")
			default:
				terr = errors.New("unknown MCP server type")
			}
		}
		if terr != nil {
			*diags = append(*diags, AgentPluginDiagnostic{"mcp:" + name, terr.Error()})
			continue
		}
		out[name] = translated
	}
	return out
}

// Vendor server declarations keyed by mcp.json server name
func portableServerDeclarations(manifest map[string]any, servers map[string]map[string]any, diags *[]AgentPluginDiagnostic) (map[string]AgentPluginServerDeclaration, error) {
	raw, present := manifestVendorExtension(manifest)["servers"]
	out := map[string]AgentPluginServerDeclaration{}
	if !present {
		return out, nil
	}
	rawServers := asMap(raw)
	if rawServers == nil {
		return nil, agentErr("extension '%s'.servers must be an object", portableExtension)
	}
	for name, entry := range rawServers {
		if _, ok := servers[name]; !ok {
			return nil, agentErr("server declaration '%s' has no matching mcp.json server", name)
		}
		decl := asMap(entry)
		if decl == nil {
			return nil, agentErr("server declaration '%s' must be an object", name)
		}
		var unknown []string
		for k := range decl {
			if !serverDeclarationKeys[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			if diags != nil {
				*diags = append(*diags, AgentPluginDiagnostic{"server:" + name, fmt.Sprintf("ignored unknown keys %v (needs a newer icebath)", unknown)})
			}
			kept := map[string]any{}
			for k, v := range decl {
				if serverDeclarationKeys[k] {
					kept[k] = v
				}
			}
			decl = kept
			if len(decl) == 0 {
				continue
			}
		}
		_, hasLive := decl["liveness"]
		_, hasApp := decl["app"]
		_, hasReq := decl["requires"]
		if hasLive && !hasApp && !hasReq {
			return nil, agentErr("server declaration '%s' has liveness without app or requires", name)
		}
		policyOnly := true
		for k := range decl {
			if !serverPolicyKeys[k] {
				policyOnly = false
			}
		}
		if policyOnly {
			continue
		}
		app := asMap(decl["app"])
		req := asMap(decl["requires"])
		if decl["app"] != nil && app == nil {
			return nil, agentErr("plugin.json extension server '%s' app must be an object", name)
		}
		if decl["requires"] != nil && req == nil {
			return nil, agentErr("plugin.json extension server '%s' requires must be an object", name)
		}
		var live map[string]any
		if decl["liveness"] != nil {
			live = asMap(decl["liveness"])
			if live == nil {
				return nil, agentErr("server declaration '%s' liveness must be an object", name)
			}
		}
		out[name] = AgentPluginServerDeclaration{App: app, Requires: req, Liveness: live}
	}
	return out, nil
}

// Per server trust narrowing a package declares, only untrusted survives
func portableServerTrust(manifest map[string]any, servers map[string]map[string]any) (map[string]string, error) {
	out := map[string]string{}
	for name, entry := range asMap(manifestVendorExtension(manifest)["servers"]) {
		decl := asMap(entry)
		if decl == nil {
			continue
		}
		raw, ok := decl["trust"]
		if !ok {
			continue
		}
		if _, known := servers[name]; !known {
			continue
		}
		switch raw {
		case "full":
		case "untrusted":
			out[name] = "untrusted"
		default:
			return nil, agentErr("server declaration '%s' trust must be 'untrusted' or 'full', not %v", name, raw)
		}
	}
	return out, nil
}

// Per server call deadlines a package declares
func portableServerTimeouts(manifest map[string]any, servers map[string]map[string]any) (map[string]float64, error) {
	out := map[string]float64{}
	for name, entry := range asMap(manifestVendorExtension(manifest)["servers"]) {
		decl := asMap(entry)
		if decl == nil {
			continue
		}
		raw, ok := decl["timeout"]
		if !ok {
			continue
		}
		if _, known := servers[name]; !known {
			continue
		}
		var f float64
		switch n := raw.(type) {
		case int:
			f = float64(n)
		case int64:
			f = float64(n)
		case float64:
			f = n
		default:
			return nil, agentErr("server declaration '%s' timeout must be positive seconds within the platform wait limit", name)
		}
		if f <= 0 || f > 1e9 {
			return nil, agentErr("server declaration '%s' timeout must be positive seconds within the platform wait limit", name)
		}
		out[name] = f
	}
	return out, nil
}

// Per server cost policies translated to the native tool_costs keys
func portableServerCosts(manifest map[string]any, servers map[string]map[string]any) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for name, entry := range asMap(manifestVendorExtension(manifest)["servers"]) {
		decl := asMap(entry)
		if decl == nil {
			continue
		}
		if _, known := servers[name]; !known {
			continue
		}
		declared := map[string]any{}
		for _, k := range []string{"costs", "spend_limit", "cost_unit"} {
			if v, ok := decl[k]; ok {
				declared[k] = v
			}
		}
		if len(declared) == 0 {
			continue
		}
		if err := validateCostPolicy(declared["costs"], declared["spend_limit"], declared["cost_unit"], fmt.Sprintf("server declaration '%s'", name)); err != nil {
			return nil, agentErr("%s", err.Error())
		}
		if c, ok := declared["costs"]; ok {
			declared["tool_costs"] = c
			delete(declared, "costs")
		}
		out[name] = declared
	}
	return out, nil
}

// Shape check of a declared cost table and spend limit
func validateCostPolicy(costs, spendLimit, unit any, where string) error {
	if costs != nil {
		m := asMap(costs)
		if m == nil {
			return fmt.Errorf("%s costs must map tool names to non-negative numbers", where)
		}
		for tool, v := range m {
			f, ok := toFloat(v)
			if tool == "" || !ok || f < 0 {
				return fmt.Errorf("%s costs must map tool names to non-negative numbers", where)
			}
		}
	}
	if spendLimit != nil {
		f, ok := toFloat(spendLimit)
		if !ok || f < 0 {
			return fmt.Errorf("%s spend_limit must be a non-negative number", where)
		}
		if costs == nil {
			return fmt.Errorf("%s spend_limit needs a costs table", where)
		}
	}
	if unit != nil {
		s, ok := unit.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s cost_unit must be a non-empty string", where)
		}
	}
	return nil
}

// Float of a numeric JSON or YAML value
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

// Resolved root with its validated manifest and diagnostics
func validatePortableRoot(pluginRoot string) (string, map[string]any, []AgentPluginDiagnostic, error) {
	root, err := filepath.EvalSymlinks(pluginRoot)
	if err != nil {
		return "", nil, nil, agentErr("plugin root must be a directory")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", nil, nil, agentErr("plugin root must be a directory")
	}
	manifest, diags, err := validatePortableManifest(root)
	if err != nil {
		return "", nil, nil, err
	}
	return root, manifest, diags, nil
}

// Validates and translates one installed portable package
func LoadAgentPlugin(pluginRoot, dataRoot string) (*AgentPluginPackage, error) {
	root, manifest, diags, err := validatePortableRoot(pluginRoot)
	if err != nil {
		return nil, err
	}
	resolvedData := filepath.Clean(dataRoot)
	if abs, err := filepath.Abs(resolvedData); err == nil {
		resolvedData = abs
	}
	skills := discoverPortableSkills(root, &diags)
	servers := discoverPortableMCP(root, resolvedData, &diags, true)
	trust, err := portableServerTrust(manifest, servers)
	if err != nil {
		return nil, err
	}
	for name, tier := range trust {
		servers[name]["trust"] = tier
	}
	timeouts, err := portableServerTimeouts(manifest, servers)
	if err != nil {
		return nil, err
	}
	for name, t := range timeouts {
		servers[name]["timeout"] = t
	}
	costs, err := portableServerCosts(manifest, servers)
	if err != nil {
		return nil, err
	}
	for name, c := range costs {
		for k, v := range c {
			servers[name][k] = v
		}
	}
	decls, err := portableServerDeclarations(manifest, servers, &diags)
	if err != nil {
		return nil, err
	}
	version, _ := manifest["version"].(string)
	desc, _ := manifest["description"].(string)
	return &AgentPluginPackage{
		Name: manifest["name"].(string), Version: version, Description: desc, Root: root, DataRoot: resolvedData,
		Manifest: manifest, Skills: skills, MCPServers: servers, ServerDeclarations: decls, Diagnostics: diags,
	}, nil
}

// Validated root plugin.json without discovering components
func ReadAgentPluginManifest(pluginRoot string) (map[string]any, []AgentPluginDiagnostic, error) {
	_, manifest, diags, err := validatePortableRoot(pluginRoot)
	return manifest, diags, err
}

// Manifest of a portable package directory with its diagnostics logged
func portablePluginManifest(child, source, prefix string) (*Manifest, error) {
	data, diags, err := ReadAgentPluginManifest(child)
	if err != nil {
		return nil, err
	}
	for _, d := range diags {
		logWarn("portable plugin diagnostic", "plugin", child, "message", d.Message)
	}
	name := data["name"].(string)
	key := name
	if prefix != "" {
		key = prefix + "/" + filepath.Base(child)
	}
	version, _ := data["version"].(string)
	desc, _ := data["description"].(string)
	m := &Manifest{
		Name: name, Version: version, Description: desc, Author: displayAuthor(data["author"]), Source: source,
		Path: child, Key: key, Kind: "standalone", Portable: true, DataNamespace: portableDataNamespace(key),
		ConfigSchema: configSchemaEntries(key, manifestConfigSchema(data)),
		ConfigKeys:   manifestConfigKeys(manifestVendorExtension(data), key), ManifestVersion: 1, Raw: data,
	}
	if st, err := os.Stat(filepath.Join(child, "skills")); err == nil && st.IsDir() {
		m.SkillDirs = []string{filepath.Join(child, "skills")}
	}
	return m, nil
}
