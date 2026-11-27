package mcp

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
)

// Manifests of the servers icebath ships in its catalog, one folder each
//
//go:embed catalog
var bundled embed.FS

// Version of the manifest format this build reads
const manifestVersion = 1

var (
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	slugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	oauthKeys    = []string{"client_id", "client_secret", "redirect_host", "redirect_port", "scope"}
)

type catalog struct{}

func Register(registry *mcp.Registry) {
	registry.Register(catalog{})
}

// Entries of the folder tools.mcp.catalog_dir names, else of the bundled manifests; a manifest that can't be read is an entry with its
// problem
func (catalog) Catalog(cfg *config.Config) []mcp.Entry {
	root, label := fs.FS(nil), "optional-mcps"
	if dir := strings.TrimSpace(cfg.GetString(mcpconfig.CatalogDir)); dir != "" {
		root, label = os.DirFS(dir), dir
	} else {
		root, _ = fs.Sub(bundled, "catalog")
	}
	children, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil
	}
	var entries []mcp.Entry
	for _, child := range children {
		rel := child.Name() + "/manifest.yaml"
		if info, err := fs.Stat(root, rel); err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(label, child.Name(), "manifest.yaml")
		entry, err := manifest(root, rel, path)
		if err != nil {
			future := strings.Contains(err.Error(), "manifest_version") && strings.Contains(err.Error(), "unsupported")
			entries = append(entries, mcp.Entry{Name: child.Name(), Manifest: path, Problem: err.Error(), Future: future})
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// Entry of one manifest, checked the way the catalog's review checks it
func manifest(root fs.FS, rel, path string) (mcp.Entry, error) {
	data, err := fs.ReadFile(root, rel)
	if err != nil {
		return mcp.Entry{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return mcp.Entry{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if doc == nil {
		return mcp.Entry{}, fmt.Errorf("%s: manifest must be a mapping", path)
	}
	if version, ok := doc["manifest_version"].(int); !ok || version != manifestVersion {
		return mcp.Entry{}, fmt.Errorf("%s: manifest_version %v unsupported (this icebath understands version %d)", path, doc["manifest_version"], manifestVersion)
	}
	name, _ := doc["name"].(string)
	if !namePattern.MatchString(name) {
		return mcp.Entry{}, fmt.Errorf("%s: invalid or missing 'name'", path)
	}
	description := strings.TrimSpace(text(doc["description"]))
	if description == "" {
		return mcp.Entry{}, fmt.Errorf("%s: 'description' required", path)
	}
	entry := mcp.Entry{Name: name, Description: description, Source: strings.TrimSpace(text(doc["source"])), PostInstall: text(doc["post_install"]), Manifest: path}
	if entry.Launch, err = launch(path, doc["transport"]); err != nil {
		return mcp.Entry{}, err
	}
	if entry.Auth, err = auth(path, doc["auth"], name, entry.Launch.Type == "http"); err != nil {
		return mcp.Entry{}, err
	}
	if entry.Tools, err = toolDefaults(path, doc["tools"]); err != nil {
		return mcp.Entry{}, err
	}
	if slug, present := doc["connector_slug"]; present && slug != nil {
		text, ok := slug.(string)
		if !ok || !slugPattern.MatchString(text) {
			return mcp.Entry{}, fmt.Errorf("%s: connector_slug must be a hosted connector slug", path)
		}
		entry.ConnectorSlug = text
	}
	if entry.Install, err = install(path, doc["install"]); err != nil {
		return mcp.Entry{}, err
	}
	return entry, nil
}

func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func mapping(path, key string, raw any, empty map[string]any) (map[string]any, error) {
	if raw == nil {
		return empty, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: '%s' must be a mapping", path, key)
	}
	return m, nil
}

func list(path, field string, raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	l, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be a list", path, field)
	}
	return l, nil
}

func launch(path string, raw any) (mcp.Launch, error) {
	t, err := mapping(path, "transport", raw, map[string]any{})
	if err != nil {
		return mcp.Launch{}, err
	}
	kind, _ := t["type"].(string)
	if kind != "stdio" && kind != "http" {
		return mcp.Launch{}, fmt.Errorf("%s: transport.type must be 'stdio' or 'http'", path)
	}
	args, err := list(path, "transport.args", t["args"])
	if err != nil {
		return mcp.Launch{}, err
	}
	l := mcp.Launch{Type: kind, Command: text(t["command"]), URL: text(t["url"]), Env: map[string]string{}}
	for _, arg := range args {
		l.Args = append(l.Args, text(arg))
	}
	env, err := mapping(path, "transport.env", t["env"], map[string]any{})
	if err != nil {
		return mcp.Launch{}, err
	}
	for k, v := range env {
		value, ok := v.(string)
		if !ok {
			return mcp.Launch{}, fmt.Errorf("%s: transport.env must be a mapping of string to string", path)
		}
		l.Env[k] = value
	}
	if kind == "stdio" && l.Command == "" {
		return mcp.Launch{}, fmt.Errorf("%s: stdio transport requires 'command'", path)
	}
	if kind == "http" && l.URL == "" {
		return mcp.Launch{}, fmt.Errorf("%s: http transport requires 'url'", path)
	}
	return l, nil
}

func auth(path string, raw any, name string, http bool) (mcp.Auth, error) {
	a, err := mapping(path, "auth", raw, map[string]any{"type": "none"})
	if err != nil {
		return mcp.Auth{}, err
	}
	kind := text(a["type"])
	if kind == "" {
		kind = "none"
	}
	if kind != "api_key" && kind != "oauth" && kind != "none" {
		return mcp.Auth{}, fmt.Errorf("%s: auth.type must be 'api_key'|'oauth'|'none'", path)
	}
	envs, err := list(path, "auth.env", a["env"])
	if err != nil {
		return mcp.Auth{}, err
	}
	out := mcp.Auth{Type: kind, Provider: text(a["provider"])}
	declared := map[string]bool{}
	for _, raw := range envs {
		e, ok := raw.(map[string]any)
		if !ok {
			return mcp.Auth{}, fmt.Errorf("env entry must be a mapping, got %T", raw)
		}
		envName, _ := e["name"].(string)
		if !envPattern.MatchString(envName) {
			return mcp.Auth{}, fmt.Errorf("invalid env var name: %q", envName)
		}
		spec := mcp.EnvSpec{Name: envName, Prompt: envName, Required: true, Secret: true}
		if prompt := text(e["prompt"]); prompt != "" {
			spec.Prompt = prompt
		}
		if v, ok := e["required"].(bool); ok {
			spec.Required = v
		}
		if v, ok := e["secret"].(bool); ok {
			spec.Secret = v
		}
		spec.Default = text(e["default"])
		out.Env = append(out.Env, spec)
		declared[envName] = true
	}
	if http && kind == "api_key" && !declared[mcp.KeyEnv(name)] {
		return mcp.Auth{}, fmt.Errorf("%s: http + api_key auth requires auth.env to declare '%s' (the key the Authorization header references)", path, mcp.KeyEnv(name))
	}
	oauth, err := mapping(path, "auth.oauth", a["oauth"], map[string]any{})
	if err != nil {
		return mcp.Auth{}, err
	}
	if len(oauth) > 0 && kind != "oauth" {
		return mcp.Auth{}, fmt.Errorf("%s: auth.oauth is only valid with auth.type 'oauth'", path)
	}
	var unknown, undeclared []string
	for k, v := range oauth {
		if !slices.Contains(oauthKeys, k) {
			unknown = append(unknown, k)
		}
		switch value := v.(type) {
		case string:
			for _, m := range envReference.FindAllStringSubmatch(value, -1) {
				if !declared[m[1]] && !slices.Contains(undeclared, m[1]) {
					undeclared = append(undeclared, m[1])
				}
			}
		case int:
		default:
			return mcp.Auth{}, fmt.Errorf("%s: auth.oauth allows string/int values for %s only", path, strings.Join(oauthKeys, ", "))
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return mcp.Auth{}, fmt.Errorf("%s: auth.oauth allows string/int values for %s only (unknown: %s)", path, strings.Join(oauthKeys, ", "), strings.Join(unknown, ", "))
	}
	if len(undeclared) > 0 {
		slices.Sort(undeclared)
		return mcp.Auth{}, fmt.Errorf("%s: auth.oauth references env vars not declared in auth.env: %s", path, strings.Join(undeclared, ", "))
	}
	out.OAuth = oauth
	scopes, err := list(path, "auth.scopes", a["scopes"])
	if err != nil {
		return mcp.Auth{}, err
	}
	for _, scope := range scopes {
		out.Scopes = append(out.Scopes, text(scope))
	}
	return out, nil
}

func toolDefaults(path string, raw any) (mcp.ToolDefaults, error) {
	t, err := mapping(path, "tools", raw, map[string]any{})
	if err != nil {
		return mcp.ToolDefaults{}, err
	}
	names := func(key string) ([]string, error) {
		items, err := list(path, "tools."+key, t[key])
		if err != nil {
			return nil, err
		}
		if t[key] == nil {
			return nil, nil
		}
		out := []string{}
		for _, item := range items {
			name, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s: tools.%s must be a list of strings", path, key)
			}
			out = append(out, name)
		}
		return out, nil
	}
	var d mcp.ToolDefaults
	if d.Enabled, err = names("default_enabled"); err != nil {
		return d, err
	}
	if d.Excluded, err = names("default_excluded"); err != nil {
		return d, err
	}
	if d.Enabled != nil && d.Excluded != nil {
		return d, fmt.Errorf("%s: tools.default_enabled and tools.default_excluded are mutually exclusive", path)
	}
	return d, nil
}

func install(path string, raw any) (*mcp.GitInstall, error) {
	if raw == nil {
		return nil, nil
	}
	m, err := mapping(path, "install", raw, nil)
	if err != nil {
		return nil, err
	}
	if kind, _ := m["type"].(string); kind != "git" {
		return nil, fmt.Errorf("%s: install.type must be 'git' (got %v)", path, m["type"])
	}
	g := &mcp.GitInstall{URL: text(m["url"]), Ref: text(m["ref"])}
	if g.URL == "" || g.Ref == "" {
		return nil, fmt.Errorf("%s: install.url and install.ref are required", path)
	}
	steps, err := list(path, "install.bootstrap", m["bootstrap"])
	if err != nil {
		return nil, err
	}
	for _, step := range steps {
		g.Bootstrap = append(g.Bootstrap, text(step))
	}
	return g, nil
}
