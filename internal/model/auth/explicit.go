package auth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/fileutil"
)

var placeholderSecretValues = []string{
	"*", "**", "***", "changeme", "your_api_key", "your_api_key_here", "your-api-key",
	"placeholder", "example", "dummy", "null", "none",
}

var placeholderKeyPrefixes = []string{"sk-", "ghp_", "hf_"}

func isPlaceholderShape(value string) bool {
	lowered := strings.ToLower(value)
	if strings.HasPrefix(lowered, "your_") && strings.HasSuffix(lowered, "_here") {
		return true
	}
	for _, prefix := range placeholderKeyPrefixes {
		if tail, ok := strings.CutPrefix(lowered, prefix); ok && tail != "" && strings.Trim(tail, "x") == "" {
			return true
		}
	}
	stripped := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(lowered)
	return stripped != "" && strings.Trim(stripped, "x") == ""
}

func HasUsableSecret(value string, minLength int) bool {
	cleaned := strings.TrimSpace(value)
	return utf8.RuneCountInString(cleaned) >= minLength &&
		!slices.Contains(placeholderSecretValues, strings.ToLower(cleaned)) &&
		!isPlaceholderShape(cleaned)
}

func usableSecret(value string) bool { return HasUsableSecret(value, 4) }

var knownProviderKeyPrefixes = map[string][]string{
	ProviderOpenRoute: {"sk-or-"},
}

func KnownKeyPrefixes(provider string) []string {
	return slices.Clone(knownProviderKeyPrefixes[provider])
}

func matchesKeyPrefix(provider, val string) bool {
	return slices.ContainsFunc(knownProviderKeyPrefixes[provider], func(p string) bool { return strings.HasPrefix(val, p) })
}

func LooksLikeOpenRouterKey(value string) bool {
	return matchesKeyPrefix(ProviderOpenRoute, strings.TrimSpace(value))
}

func usableDeclaredSecret(provider, value, source string) (string, bool) {
	val := strings.TrimSpace(value)
	if !usableSecret(val) {
		return "", false
	}
	if prefixes := knownProviderKeyPrefixes[provider]; len(prefixes) > 0 && !matchesKeyPrefix(provider, val) {
		slog.Warn(fmt.Sprintf("Ignoring %s for provider '%s': value does not match the expected key prefix (%s). "+
			"Falling back to the next credential source. Fix or remove the malformed key to silence this warning.",
			source, provider, strings.Join(prefixes, " or ")))
		return "", false
	}
	return val, true
}

func yamlResolve(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.AliasNode || n.Kind == yaml.DocumentNode) {
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		} else if len(n.Content) > 0 {
			n = n.Content[0]
		} else {
			return nil
		}
	}
	return n
}

func yamlMember(n *yaml.Node, key string) *yaml.Node {
	n = yamlResolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return yamlResolve(n.Content[i+1])
		}
	}
	return nil
}

func yamlValues(n *yaml.Node) []*yaml.Node {
	n = yamlResolve(n)
	if n == nil {
		return nil
	}
	out := []*yaml.Node{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, yamlResolve(n.Content[i+1]))
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			out = append(out, yamlResolve(c))
		}
	}
	return out
}

func yamlScalar(n *yaml.Node) string {
	n = yamlResolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func isMapping(n *yaml.Node) bool {
	n = yamlResolve(n)
	return n != nil && n.Kind == yaml.MappingNode
}

func slotSelects(slot *yaml.Node, normalized string) bool {
	return isMapping(slot) && strings.ToLower(strings.TrimSpace(yamlScalar(yamlMember(slot, "provider")))) == normalized
}

func (s *Store) configTree(ctx context.Context) (*yaml.Node, error) {
	cfg, err := s.cfg.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := root.Encode(cfg); err != nil {
		return nil, err
	}
	return yamlResolve(&root), nil
}

func (s *Store) activeProviderIs(normalized string) (bool, error) {
	store, err := s.load(s.authPath)
	if err != nil {
		return false, err
	}
	active := strings.ToLower(strings.TrimSpace(store.str("active_provider")))
	return active != "" && active == normalized, nil
}

func (s *Store) configSelectsProvider(ctx context.Context, normalized string) (bool, error) {
	root, err := s.configTree(ctx)
	if err != nil {
		return false, err
	}
	if slotSelects(yamlMember(root, "model"), normalized) {
		return true, nil
	}
	if aux := yamlMember(root, "auxiliary"); isMapping(aux) {
		for _, slot := range yamlValues(aux) {
			if slotSelects(slot, normalized) {
				return true, nil
			}
		}
	}
	blockMatches := func(block *yaml.Node) bool {
		if !isMapping(block) {
			return false
		}
		if refs := yamlMember(block, "reference_models"); refs != nil && refs.Kind == yaml.SequenceNode {
			for _, slot := range yamlValues(refs) {
				if slotSelects(slot, normalized) {
					return true
				}
			}
		}
		return slotSelects(yamlMember(block, "aggregator"), normalized)
	}
	moa := yamlMember(root, "moa")
	if !isMapping(moa) {
		return false, nil
	}
	if blockMatches(moa) {
		return true, nil
	}
	if presets := yamlMember(moa, "presets"); isMapping(presets) {
		for _, preset := range yamlValues(presets) {
			if blockMatches(preset) {
				return true, nil
			}
		}
	}
	return false, nil
}

var implicitEnvVars = []string{"CLAUDE_CODE_OAUTH_TOKEN"}

var explicitPoolSources = []string{"device_code", "loopback_pkce", "icebath_pkce", "manual"}

var vertexProviderIDs = []string{"vertex", "google-vertex", "vertex-ai", "gcp-vertex", "vertexai"}

func (s *Store) preferFile(ctx context.Context, name string) (string, error) {
	v, _, err := s.env.GetPreferFile(ctx, name)
	return v, err
}

func (s *Store) lookupEnv(ctx context.Context, name string) string {
	v, _, err := s.env.Lookup(ctx, name)
	if err != nil {
		slog.Debug("auth: scoped environment read refused", "name", name, "error", err)
		return ""
	}
	return v
}

func (s *Store) envSecret(ctx context.Context, name string) (bool, error) {
	v, err := s.preferFile(ctx, name)
	if err != nil {
		return false, err
	}
	return usableSecret(v), nil
}

func (s *Store) explicitEnvCredentialsPresent(ctx context.Context, normalized string) (bool, error) {
	spec, ok := s.dir.Spec(ctx, normalized)
	if !ok {
		return false, nil
	}
	switch spec.AuthType {
	case AuthTypeAPIKey:
		for _, name := range spec.EnvVars {
			if slices.Contains(implicitEnvVars, name) {
				continue
			}
			if ok, err := s.envSecret(ctx, name); err != nil || ok {
				return ok, err
			}
		}
	case AuthTypeAWSSDK:
		if ok, err := s.envSecret(ctx, "AWS_BEARER_TOKEN_BEDROCK"); err != nil || ok {
			return ok, err
		}
		id, err := s.envSecret(ctx, "AWS_ACCESS_KEY_ID")
		if err != nil || !id {
			return false, err
		}
		return s.envSecret(ctx, "AWS_SECRET_ACCESS_KEY")
	}
	return false, nil
}

func (s *Store) poolRowIsExplicit(ctx context.Context, row *doc) (bool, error) {
	raw := textOf(row.value("source"))
	source := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(source, "env:") {
		_, name, _ := strings.Cut(raw, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			return false, nil
		}
		return s.envSecret(ctx, name)
	}
	return source != "" && (slices.Contains(explicitPoolSources, source) || strings.HasPrefix(source, "manual:")), nil
}

func (s *Store) explicitPoolEntryPresent(ctx context.Context, normalized string) (bool, error) {
	rows, err := s.readPool(ctx, normalized)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		ok, err := s.poolRowIsExplicit(ctx, row)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) keylessProviderHasExplicitConfig(ctx context.Context, normalized string) (bool, error) {
	if slices.Contains(vertexProviderIDs, normalized) {
		if strings.TrimSpace(s.lookupEnv(ctx, "VERTEX_PROJECT_ID")) != "" {
			return true, nil
		}
		cfg, err := s.cfg.LoadReadonly(ctx)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(cfg.Vertex.ProjectID) != "" {
			return true, nil
		}
		path := s.lookupEnv(ctx, "VERTEX_CREDENTIALS_PATH")
		if path == "" {
			return false, nil
		}
		fi, err := os.Stat(path)
		return err == nil && fi.Mode().IsRegular() && fileutil.Accessible(path, fileutil.AccessRead), nil
	}
	if normalized == "bedrock" {
		cfg, err := s.cfg.LoadReadonly(ctx)
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(cfg.Bedrock.Region) != "", nil
	}
	return false, nil
}

func (s *Store) IsExplicitlyConfigured(ctx context.Context, provider string) (bool, error) {
	ctx = s.bind(ctx)
	normalized := strings.ToLower(strings.TrimSpace(provider))
	tolerant := func(name string, check func() (bool, error)) bool {
		ok, err := check()
		if err != nil {
			slog.Debug(fmt.Sprintf("explicit-config check %s failed for %s", name, provider), "error", err)
			return false
		}
		return ok
	}
	if tolerant("active_provider", func() (bool, error) { return s.activeProviderIs(normalized) }) {
		return true, nil
	}
	if tolerant("config_selects_provider", func() (bool, error) { return s.configSelectsProvider(ctx, normalized) }) {
		return true, nil
	}
	if ok, err := s.explicitEnvCredentialsPresent(ctx, normalized); err != nil || ok {
		return ok, err
	}
	if tolerant("explicit_pool_entry", func() (bool, error) { return s.explicitPoolEntryPresent(ctx, normalized) }) {
		return true, nil
	}
	return tolerant("keyless_provider_config", func() (bool, error) { return s.keylessProviderHasExplicitConfig(ctx, normalized) }), nil
}

func (s *Store) RefuseEnvAdoptionIfConfigCorrupt(ctx context.Context) error {
	ctx = s.bind(ctx)
	failure, ok := s.cfg.ActiveParseFailure(ctx)
	if !ok || failure == "" {
		return nil
	}
	path, _, err := s.cfg.Paths(ctx)
	if err != nil {
		slog.Debug("Could not probe config parse-failure state", "error", err)
		return nil
	}
	return &Error{
		Message: fmt.Sprintf("config.yaml at %s is corrupt (%s) — refusing to auto-select an inference provider from environment keys. "+
			"Fix the YAML (a backup was saved next to it) or run icebath setup.", path, failure),
		Code: "corrupt_config",
	}
}
