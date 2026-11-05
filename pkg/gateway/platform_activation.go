package gateway

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Keys of every mapping a platform block may live in
func yamlBlockNames(yamlCfg any) map[string]bool {
	out := map[string]bool{}
	cfg, ok := yamlCfg.(map[string]any)
	if !ok {
		return out
	}
	gw, _ := cfg["gateway"].(map[string]any)
	for _, parent := range []any{cfg, cfg["platforms"], gw, gw["platforms"]} {
		if m, ok := parent.(map[string]any); ok {
			for k, v := range m {
				if _, isMap := v.(map[string]any); isMap {
					out[k] = true
				}
			}
		}
	}
	return out
}

// Env names holding a non blank value in the process, the profile scope, the home .env, secret sources and managed env
func envNames(ctx context.Context, h string) map[string]bool {
	names := map[string]bool{}
	add := func(m map[string]string) {
		for k, v := range m {
			if pystr.Strip(v) != "" {
				names[k] = true
			}
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && pystr.Strip(v) != "" {
			names[k] = true
		}
	}
	add(provider.CurrentSecretScope(ctx))
	if h != "" {
		add(config.LoadEnvFile(filepath.Join(h, ".env")))
		add(provider.SecretSourceValues(h))
	}
	add(config.LoadManagedEnv())
	return names
}

// Top level keys of a JSON file's named mapping, empty when missing or unreadable
func jsonSectionKeys(path, section, what string) map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("platform activation: " + what + " unreadable")
		}
		return out
	}
	if !utf8.Valid(raw) {
		slog.Debug("platform activation: " + what + " unreadable")
		return out
	}
	doc, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if err != nil {
		slog.Debug("platform activation: " + what + " unreadable")
		return out
	}
	doc = plainJSON(doc)
	if !pyrepr.Truthy(doc) {
		doc = map[string]any{}
	}
	m, ok := doc.(map[string]any)
	if !ok {
		slog.Debug("platform activation: " + what + " unreadable")
		return out
	}
	rows := m[section]
	if !pyrepr.Truthy(rows) {
		return out
	}
	if rm, ok := rows.(map[string]any); ok {
		for k := range rm {
			out[k] = true
		}
	}
	return out
}

// Whether a candidate equals the name or extends it with an underscore suffix, case insensitively
func activationMatches(name string, candidates map[string]bool) bool {
	lowered := pystr.Lower(name)
	for c := range candidates {
		cl := pystr.Lower(c)
		if cl == lowered || strings.HasPrefix(cl, lowered+"_") {
			return true
		}
	}
	return false
}

// Predicate telling whether this profile shows evidence of configuring a platform, yamlCfg nil reads the home's layers
func ActivationPredicate(ctx context.Context, yamlCfg any, platformNames []string, h string, registry *PlatformRegistry) func(string) bool {
	if h == "" {
		h = home.Dir(ctx)
	}
	if yamlCfg == nil {
		if layers, err := ReadYAMLLayers(ctx, h); err == nil {
			yamlCfg = layers
		} else {
			slog.Debug("platform activation: config.yaml unreadable")
		}
	}
	blocks := yamlBlockNames(yamlCfg)
	for _, n := range platformNames {
		blocks[n] = true
	}
	for k := range jsonSectionKeys(filepath.Join(h, "gateway.json"), "platforms", "gateway.json") {
		blocks[k] = true
	}
	upperEnv := map[string]bool{}
	for e := range envNames(ctx, h) {
		upperEnv[pystr.Upper(e)] = true
	}
	pools := jsonSectionKeys(filepath.Join(h, "auth.json"), "credential_pool", "auth.json")
	return func(name string) bool {
		if activationMatches(name, blocks) || activationMatches(name, pools) {
			return true
		}
		prefix := strings.ReplaceAll(pystr.Upper(name), "-", "_") + "_"
		for e := range upperEnv {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		if registry == nil {
			return false
		}
		for d := range registry.ActivationEnv(name) {
			if upperEnv[pystr.Upper(d)] {
				return true
			}
		}
		return false
	}
}

// Plugin entries of platforms this profile configures, already loaded entries always included
func ConfiguredPluginEntries(ctx context.Context, registry *PlatformRegistry, yamlCfg any, platformNames []string, h string) []*PlatformEntry {
	if registry == nil {
		return []*PlatformEntry{}
	}
	out := []*PlatformEntry{}
	for _, e := range registry.ConfiguredEntries(ctx, ActivationPredicate(ctx, yamlCfg, platformNames, h, registry)) {
		if e.Source == "plugin" {
			out = append(out, e)
		}
	}
	return out
}
