package plugins

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
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
var portableVarRe = regexp.MustCompile(`\$\{(PLUGIN_ROOT|PLUGIN_DATA)\}`)
var frontmatterEndRe = regexp.MustCompile(`\n---\s*\n`)
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
	Name     string
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

// Value with PLUGIN_ROOT and PLUGIN_DATA placeholders expanded
func expandPortableVars(value, pluginRoot, dataRoot string) string {
	return portableVarRe.ReplaceAllStringFunc(value, func(m string) string {
		if strings.Contains(m, "PLUGIN_ROOT") {
			return pluginRoot
		}
		return dataRoot
	})
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

// Reports whether path resolves under root resolved strictly, false when either resolution fails
func insideRoot(path, root string) bool {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return false
	}
	rootResolved, err := pypath.Realpath(pypath.PathStr(root), pypath.StrictAll)
	if err != nil {
		return false
	}
	_, ok := pypath.PathRelativeTo(resolved, pypath.PathStr(rootResolved))
	return ok
}

// Ordered JSON object of a utf-8-sig file, read and decode failures named under label
func readJSONObject(path, label string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, agentErr("%s is not valid readable JSON: %s", label, pyfs.OSErrorText(err))
	}
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, agentErr("%s is not valid readable JSON: %v", label, err)
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, agentErr("%s is not valid readable JSON: %v", label, err)
	}
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, agentErr("%s must contain a JSON object", label)
	}
	return obj, nil
}

// Validated plugin.json mapping with unknown fields dropped as diagnostics
func validatePortableManifest(root string) (map[string]any, []AgentPluginDiagnostic, error) {
	path := filepath.Join(root, "plugin.json")
	if st, err := os.Stat(path); !insideRoot(path, root) || err != nil || !st.Mode().IsRegular() {
		return nil, nil, agentErr("plugin.json must be a regular file within the plugin root")
	}
	obj, err := readJSONObject(path, "plugin.json")
	if err != nil {
		return nil, nil, err
	}
	manifest := pyjson.Plain(obj).(map[string]any)
	var diags []AgentPluginDiagnostic
	for _, k := range sortedKeys(manifest) {
		if !portablePluginFields[k] {
			diags = append(diags, AgentPluginDiagnostic{"manifest", "ignored unknown top-level field: " + k})
			delete(manifest, k)
		}
	}
	if manifest["$schema"] != PluginSchemaV1 {
		return nil, nil, agentErr("plugin.json declares an unsupported or missing Agent Plugins schema")
	}
	name, isStr := manifest["name"].(string)
	if n := utf8.RuneCountInString(name); !isStr || n < 1 || n > 64 || !portablePluginNameRe.MatchString(name) ||
		strings.Contains(name, "--") || strings.Contains(name, "..") {
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
		if items, isList := kw.([]any); !isList || !allStrings(items) {
			return nil, nil, agentErr("plugin.json keywords must be an array of strings")
		}
	}
	if a, ok := manifest["author"]; ok {
		author, isMap := a.(map[string]any)
		if !isMap {
			return nil, nil, agentErr("plugin.json author must be an object")
		}
		for k, v := range author {
			if _, isStr := v.(string); !portableAuthorFields[k] || !isStr {
				return nil, nil, agentErr("plugin.json author may contain only string name, email, and url fields")
			}
		}
	}
	if ext, ok := manifest["extensions"]; ok {
		if extensions, isMap := ext.(map[string]any); !isMap {
			diags = append(diags, AgentPluginDiagnostic{"manifest", "ignored non-object extensions field"})
			delete(manifest, "extensions")
		} else {
			for _, v := range extensions {
				if _, isMap := v.(map[string]any); !isMap {
					return nil, nil, agentErr("plugin.json extension namespace values must be objects")
				}
			}
		}
	}
	return manifest, diags, nil
}

// Frontmatter mapping of a SKILL.md file, failing with the Python ValueError texts
func parseSkillFrontmatter(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	content = strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return nil, errors.New("missing YAML frontmatter")
	}
	end := frontmatterEndRe.FindStringIndex(content[3:])
	if end == nil {
		return nil, errors.New("unterminated YAML frontmatter")
	}
	parsed, err := config.SafeLoad(content[3 : end[0]+3])
	if err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %v", err)
	}
	m, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("YAML frontmatter must be an object")
	}
	return m, nil
}

// Validation problem of a skill frontmatter, empty when valid
func validSkillFrontmatter(fm map[string]any, dirName string) string {
	name, isStr := fm["name"].(string)
	if n := utf8.RuneCountInString(name); !isStr || name != dirName || n < 1 || n > 64 || !portableSkillNameRe.MatchString(name) || strings.Contains(name, "--") {
		return "name must match the directory and satisfy Agent Skills constraints"
	}
	desc, isStr := fm["description"].(string)
	if n := utf8.RuneCountInString(desc); !isStr || n < 1 || n > 1024 {
		return "description must be a non-empty string of at most 1024 characters"
	}
	if v, ok := fm["license"]; ok {
		if _, isStr := v.(string); !isStr {
			return "license must be a string"
		}
	}
	if v, ok := fm["compatibility"]; ok {
		s, isStr := v.(string)
		if n := utf8.RuneCountInString(s); !isStr || n < 1 || n > 500 {
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

// Text of a SKILL.md read or parse failure as the Python exception str
func skillReadErrorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pyfs.OSErrorText(err)
	}
	return err.Error()
}

// Skills under the package skills directory sorted by name, invalid ones reported as diagnostics
func discoverPortableSkills(root string, diags *[]AgentPluginDiagnostic) ([]AgentPluginSkill, error) {
	skillsRoot := filepath.Join(root, "skills")
	if _, err := os.Lstat(skillsRoot); err != nil {
		return nil, nil
	}
	if st, err := os.Stat(skillsRoot); !insideRoot(skillsRoot, root) || err != nil || !st.IsDir() {
		*diags = append(*diags, AgentPluginDiagnostic{"skills", "skills must be an in-root directory"})
		return nil, nil
	}
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		*diags = append(*diags, AgentPluginDiagnostic{"skills", "cannot list skills: " + pyfs.OSErrorText(err)})
		return nil, nil
	}
	var skills []AgentPluginSkill
	for _, e := range entries {
		child := filepath.Join(skillsRoot, e.Name())
		skillMD := filepath.Join(child, "SKILL.md")
		if cst, err := os.Stat(child); err != nil || !cst.IsDir() {
			continue
		}
		if _, err := os.Stat(skillMD); err != nil {
			continue
		}
		problem := ""
		var fm map[string]any
		if mst, err := os.Stat(skillMD); !insideRoot(skillMD, root) || err != nil || !mst.Mode().IsRegular() {
			problem = "SKILL.md must be a regular in-root file"
		} else if fm, err = parseSkillFrontmatter(skillMD); err != nil {
			problem = "invalid SKILL.md: " + skillReadErrorText(err)
		} else {
			problem = validSkillFrontmatter(fm, e.Name())
		}
		if problem != "" {
			*diags = append(*diags, AgentPluginDiagnostic{"skill:" + e.Name(), problem})
			continue
		}
		rootReal, err := pypath.Realpath(child, pypath.StrictAll)
		if err != nil {
			return nil, err
		}
		mdReal, err := pypath.Realpath(skillMD, pypath.StrictAll)
		if err != nil {
			return nil, err
		}
		skills = append(skills, AgentPluginSkill{Name: e.Name(), Description: fm["description"].(string), Root: rootReal,
			SkillMD: mdReal, Frontmatter: maps.Clone(fm)})
	}
	return skills, nil
}

// Resolved path scoped to the plugin root or the data root, refusing one that escapes its root
func resolveScopedPath(value, pluginRoot, dataRoot string, expand bool) (string, error) {
	expanded := value
	if expand {
		expanded = expandPortableVars(value, pluginRoot, dataRoot)
	}
	var base, candidate string
	switch {
	case strings.HasPrefix(value, "./"):
		base, candidate = pluginRoot, pathJoin(pluginRoot, expanded[2:])
	case value == "${PLUGIN_ROOT}" || strings.HasPrefix(value, "${PLUGIN_ROOT}/"):
		base, candidate = pluginRoot, expanded
	case value == "${PLUGIN_DATA}" || strings.HasPrefix(value, "${PLUGIN_DATA}/"):
		base, candidate = dataRoot, expanded
	default:
		return "", errors.New("path must start with ./, ${PLUGIN_ROOT}, or ${PLUGIN_DATA}")
	}
	resolved, err := pypath.Resolve(candidate)
	if err != nil {
		return "", errors.New("path escapes its resolved root")
	}
	baseResolved, err := pypath.Resolve(base)
	if err != nil {
		return "", errors.New("path escapes its resolved root")
	}
	if _, ok := pypath.PathRelativeTo(resolved, baseResolved); !ok {
		return "", errors.New("path escapes its resolved root")
	}
	return resolved, nil
}

// Path joined the pathlib way, an absolute second part replacing the first
func pathJoin(base, rest string) string {
	if filepath.IsAbs(rest) {
		return rest
	}
	if rest == "" {
		return base
	}
	return strings.TrimRight(base, string(filepath.Separator)) + string(filepath.Separator) + rest
}

// Validated remote MCP url, absolute http or https without user info or fragment, http on loopback only
func validateRemoteURL(v any) (string, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", errors.New("url must be a non-empty string")
	}
	u, err := pyurl.Split(s)
	if err != nil {
		return "", fmt.Errorf("url is not parseable: %v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("url scheme must be http or https")
	}
	if strings.Contains(u.Netloc, "@") {
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
		ip, parsed := pyipaddress.IPAddress(host)
		if !parsed || !pyipaddress.IsLoopback(ip) {
			return "", errors.New("non-loopback url must use https")
		}
	}
	return s, nil
}

// Native MCP config of a portable streamable-http server entry with strict redirect headers
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
	if h, ok := cfg["headers"].(map[string]any); ok && len(h) > 0 {
		out["headers"] = maps.Clone(h)
	}
	return out, nil
}

// Native MCP config of a portable stdio server entry, the data root created when asked
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
	case strings.ContainsFunc(command, pystr.IsSpace):
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
		env = raw.(map[string]any)
	}
	for k := range env {
		if runtime.GOOS == "windows" {
			k = strings.ToUpper(k)
		}
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
		if err := os.MkdirAll(dataRoot, 0o777); err != nil {
			return nil, err
		}
		if _, under := pypath.PathRelativeTo(cwdPath, dataRoot); under {
			if err := os.MkdirAll(cwdPath, 0o777); err != nil {
				return nil, err
			}
		}
	}
	outEnv := map[string]any{}
	for k, v := range env {
		outEnv[k] = expandPortableVars(v.(string), pluginRoot, dataRoot)
	}
	outEnv["PLUGIN_ROOT"] = pluginRoot
	outEnv["PLUGIN_DATA"] = dataRoot
	outArgs := make([]any, 0, len(args))
	for _, a := range args {
		outArgs = append(outArgs, expandPortableVars(a.(string), pluginRoot, dataRoot))
	}
	return map[string]any{"command": command, "args": outArgs, "env": outEnv, "cwd": cwdPath}, nil
}

// Refusal of a portable sse entry, naming a malformed remote entry first
func rejectSSE(server map[string]any) (map[string]any, error) {
	for k := range server {
		if !portableRemoteFields[k] {
			return nil, errors.New("invalid remote entry")
		}
	}
	if u, ok := server["url"].(string); !ok || u == "" || !validHeaders(server["headers"]) {
		return nil, errors.New("invalid remote entry")
	}
	return nil, errors.New("portable sse transport is not supported")
}

// Text of a server translation failure as the Python exception str
func translateErrorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pyfs.OSErrorText(err)
	}
	return err.Error()
}

// Translated mcp.json servers in file order, file level problems reported as one diagnostic
func discoverPortableMCP(root, dataRoot string, diags *[]AgentPluginDiagnostic, createData bool) (map[string]map[string]any, error) {
	path := filepath.Join(root, "mcp.json")
	if _, err := os.Lstat(path); err != nil {
		return map[string]map[string]any{}, nil
	}
	fail := func(msg string) (map[string]map[string]any, error) {
		*diags = append(*diags, AgentPluginDiagnostic{"mcp", msg})
		return map[string]map[string]any{}, nil
	}
	if st, err := os.Stat(path); !insideRoot(path, root) || err != nil || !st.Mode().IsRegular() {
		return fail("mcp.json must be a regular in-root file")
	}
	cfg, err := readJSONObject(path, "mcp.json")
	if err != nil {
		return fail(err.Error())
	}
	_, hasSchema := cfg.Vals["$schema"]
	_, hasServers := cfg.Vals["mcpServers"]
	if len(cfg.Keys) != 2 || !hasSchema || !hasServers {
		return fail("mcp.json has an invalid top-level shape")
	}
	if cfg.Vals["$schema"] != MCPSchemaV1 {
		return fail("mcp.json declares an unsupported schema")
	}
	servers, ok := cfg.Vals["mcpServers"].(*pyjson.Object)
	if !ok {
		return fail("mcpServers must be an object")
	}
	out := map[string]map[string]any{}
	for _, name := range servers.Keys {
		server, isMap := pyjson.Plain(servers.Vals[name]).(map[string]any)
		var translated map[string]any
		var terr error
		if name == "" || !isMap {
			terr = errors.New("invalid server entry")
		} else {
			switch kind := server["type"].(type) {
			case []any, map[string]any:
				return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("unhashable type: '%s'", pyTypeName(kind))}
			default:
				switch kind {
				case "stdio":
					translated, terr = translateStdioServer(server, root, dataRoot, createData)
				case "streamable-http":
					translated, terr = translateRemoteServer(server)
				case "sse":
					translated, terr = rejectSSE(server)
				default:
					terr = errors.New("unknown MCP server type")
				}
			}
		}
		if terr != nil {
			*diags = append(*diags, AgentPluginDiagnostic{"mcp:" + name, translateErrorText(terr)})
			continue
		}
		out[name] = translated
	}
	return out, nil
}

// Vendor extension servers mapping of a manifest as the Python chained get reads it, raising on a non mapping link
func vendorServers(manifest map[string]any) (any, error) {
	extensions, present := manifest["extensions"]
	if !present {
		return map[string]any{}, nil
	}
	ext, ok := extensions.(map[string]any)
	if !ok {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(extensions))}
	}
	namespace, present := ext[portableExtension]
	if !present {
		return map[string]any{}, nil
	}
	ns, ok := namespace.(map[string]any)
	if !ok {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(namespace))}
	}
	servers, present := ns["servers"]
	if !present {
		return map[string]any{}, nil
	}
	return servers, nil
}

// Vendor server declarations keyed by mcp.json server name in manifest order, policy only entries skipped
func portableServerDeclarations(manifest map[string]any, servers map[string]map[string]any, order []string, diags *[]AgentPluginDiagnostic) (map[string]AgentPluginServerDeclaration, error) {
	raw, err := vendorServers(manifest)
	if err != nil {
		return nil, err
	}
	rawServers, ok := raw.(map[string]any)
	if !ok {
		return nil, agentErr("extension '%s'.servers must be an object", portableExtension)
	}
	out := map[string]AgentPluginServerDeclaration{}
	for _, name := range orderedMapKeys(rawServers, order) {
		if _, ok := servers[name]; !ok {
			return nil, agentErr("server declaration '%s' has no matching mcp.json server", name)
		}
		decl, ok := rawServers[name].(map[string]any)
		if !ok {
			return nil, agentErr("server declaration '%s' must be an object", name)
		}
		var unknown []string
		for _, k := range sortedKeys(decl) {
			if !serverDeclarationKeys[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			if diags != nil {
				*diags = append(*diags, AgentPluginDiagnostic{"server:" + name,
					fmt.Sprintf("ignored unknown keys %s (needs a newer Icebath)", pyrepr.Repr(stringsAny(unknown)))})
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
		if len(decl) > 0 && policyOnly {
			continue
		}
		if _, err := tools.ParsePlatformDeclaration(name, decl["app"], decl["requires"], "plugin.json extension server "+pyrepr.Repr(name)); err != nil {
			return nil, agentErr("%s", err.Error())
		}
		var live map[string]any
		if raw := decl["liveness"]; raw != nil {
			if live, ok = raw.(map[string]any); !ok {
				return nil, agentErr("server declaration '%s' liveness must be an object", name)
			}
			live = maps.Clone(live)
		}
		app, _ := decl["app"].(map[string]any)
		req, _ := decl["requires"].(map[string]any)
		out[name] = AgentPluginServerDeclaration{Name: name, App: app, Requires: req, Liveness: live}
	}
	return out, nil
}

// Declared server entries in manifest order, a non mapping servers value yielding none
func declaredServerEntries(manifest map[string]any, servers map[string]map[string]any, order []string) ([]string, map[string]map[string]any, error) {
	raw, err := vendorServers(manifest)
	if err != nil {
		return nil, nil, err
	}
	rawServers, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, nil
	}
	var names []string
	entries := map[string]map[string]any{}
	for _, name := range orderedMapKeys(rawServers, order) {
		decl, ok := rawServers[name].(map[string]any)
		if _, known := servers[name]; !ok || !known {
			continue
		}
		names = append(names, name)
		entries[name] = decl
	}
	return names, entries, nil
}

// Per server trust narrowing a package declares, only untrusted survives
func portableServerTrust(manifest map[string]any, servers map[string]map[string]any, order []string) (map[string]string, error) {
	names, entries, err := declaredServerEntries(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range names {
		value, ok := entries[name]["trust"]
		if !ok {
			continue
		}
		switch value {
		case "full":
		case "untrusted":
			out[name] = "untrusted"
		default:
			return nil, agentErr("server declaration '%s' trust must be 'untrusted' or 'full', not %s", name, pyrepr.Repr(value))
		}
	}
	return out, nil
}

// Largest timeout threading accepts on this platform, threading.TIMEOUT_MAX
const threadingTimeoutMax = 9223372036.0

// Per server call deadlines a package declares, positive seconds within the platform wait limit
func portableServerTimeouts(manifest map[string]any, servers map[string]map[string]any, order []string) (map[string]float64, error) {
	names, entries, err := declaredServerEntries(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, name := range names {
		value, ok := entries[name]["timeout"]
		if !ok {
			continue
		}
		_, isBool := value.(bool)
		f, numeric := pyNumber(value)
		if isBool || !numeric || !(f > 0 && f <= threadingTimeoutMax) {
			return nil, agentErr("server declaration '%s' timeout must be positive seconds within the platform wait limit", name)
		}
		out[name] = f
	}
	return out, nil
}

// Per server cost policies validated by the mcp cost parser and translated to the native tool_costs keys
func portableServerCosts(manifest map[string]any, servers map[string]map[string]any, order []string) (map[string]map[string]any, error) {
	names, entries, err := declaredServerEntries(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, name := range names {
		declared := map[string]any{}
		for _, k := range []string{"costs", "spend_limit", "cost_unit"} {
			if v, ok := entries[name][k]; ok {
				declared[k] = v
			}
		}
		if len(declared) == 0 {
			continue
		}
		parse, err := needService("ParseCostPolicy", func(s *Services) func(costs, spendLimit, costUnit any, where string) error {
			return s.ParseCostPolicy
		})
		if err != nil {
			return nil, err
		}
		if err := parse(declared["costs"], declared["spend_limit"], declared["cost_unit"], fmt.Sprintf("server declaration '%s'", name)); err != nil {
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

// Strictly resolved root directory with its validated manifest and diagnostics
func validatePortableRoot(pluginRoot string) (string, map[string]any, []AgentPluginDiagnostic, error) {
	root, err := pypath.Realpath(pypath.PathStr(pluginRoot), pypath.StrictAll)
	if err != nil {
		return "", nil, nil, err
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
	resolvedData, err := pypath.Resolve(dataRoot)
	if err != nil {
		return nil, err
	}
	skills, err := discoverPortableSkills(root, &diags)
	if err != nil {
		return nil, err
	}
	servers, err := discoverPortableMCP(root, resolvedData, &diags, true)
	if err != nil {
		return nil, err
	}
	order := jsonObjectKeyOrder(filepath.Join(root, "plugin.json"), "extensions", portableExtension, "servers")
	trust, err := portableServerTrust(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	for name, tier := range trust {
		servers[name] = withKey(servers[name], "trust", tier)
	}
	timeouts, err := portableServerTimeouts(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	for name, t := range timeouts {
		servers[name] = withKey(servers[name], "timeout", t)
	}
	costs, err := portableServerCosts(manifest, servers, order)
	if err != nil {
		return nil, err
	}
	for name, c := range costs {
		merged := maps.Clone(servers[name])
		maps.Copy(merged, c)
		servers[name] = merged
	}
	decls, err := portableServerDeclarations(manifest, servers, order, &diags)
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

// Copy of a server config with one key set, the dict unpacking merge of the Python
func withKey(server map[string]any, key string, value any) map[string]any {
	out := maps.Clone(server)
	out[key] = value
	return out
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
