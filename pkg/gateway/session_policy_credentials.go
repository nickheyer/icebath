package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prefix of a durable frozen config secret reference
const configSecretPrefix = "profile-config-v1:"

// Config path of one frozen secret with string keys, int list indexes and a nil head for the terminal scope
type secretPath []any

// Owner facts and in memory launch secret slots a session authority holds for its local policies
type launchSecretOwner interface {
	launchOwnerDBPath() string
	launchOwnerProfileID() string
	launchSecretSlots() *launchSecretSlots
}

// Launch keys and frozen config secrets by slot, alive only for the authority lifetime
type launchSecretSlots struct {
	mu      sync.Mutex
	keys    map[string]string
	configs map[string]*RecoveredSecrets
}

// Frozen config secrets in path order with the paths whose profile value changed and the stamping session
type RecoveredSecrets struct {
	paths   []secretPath
	values  []any
	dropped []secretPath
	session any
}

// Frozen secret value at a path and whether the path is held
func (r *RecoveredSecrets) Get(path secretPath) (any, bool) {
	if r == nil {
		return nil, false
	}
	for i, held := range r.paths {
		if pathEqual(held, path) {
			return r.values[i], true
		}
	}
	return nil, false
}

// Value stored at a path, replacing an earlier value in place
func (r *RecoveredSecrets) Set(path secretPath, value any) {
	for i, held := range r.paths {
		if pathEqual(held, path) {
			r.values[i] = value
			return
		}
	}
	r.paths = append(r.paths, path)
	r.values = append(r.values, value)
}

// Number of held secrets
func (r *RecoveredSecrets) Len() int {
	if r == nil {
		return 0
	}
	return len(r.paths)
}

// Copy holding only the secrets outside the skipped sections
func (r *RecoveredSecrets) without(skip map[any]bool) *RecoveredSecrets {
	out := &RecoveredSecrets{}
	if r == nil {
		return out
	}
	for i, path := range r.paths {
		if skipped(path, skip) {
			continue
		}
		out.Set(path, r.values[i])
	}
	return out
}

// Same path set with equal values, dict equality
func (r *RecoveredSecrets) equal(o *RecoveredSecrets) bool {
	if r.Len() != o.Len() {
		return false
	}
	for i, path := range r.paths {
		value, ok := o.Get(path)
		if !ok || !reflect.DeepEqual(pyjson.Plain(value), pyjson.Plain(r.values[i])) {
			return false
		}
	}
	return true
}

// List index of a path element, false for strings and None
func pathIndex(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// Tuple equality of two paths with ints compared by value
func pathEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ai, aInt := pathIndex(a[i])
		bi, bInt := pathIndex(b[i])
		if aInt || bInt {
			if aInt != bInt || ai != bi {
				return false
			}
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Hashable form of a path for set membership
func pathKey(path []any) string {
	text, _ := pyjson.Dumps([]any(path))
	return text
}

// Mapping view of a decoded config node
func configMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out, true
	case tools.TerminalPolicy:
		return configMap(map[string]string(m))
	}
	return nil, false
}

// Value a root defines at a path and false when a key along it is simply not there
func definedAt(root any, path []any) (any, bool) {
	value := root
	for _, key := range path {
		if list, ok := value.([]any); ok {
			if i, isInt := pathIndex(key); isInt {
				if i < 0 || i >= len(list) {
					return nil, false
				}
				value = list[i]
				continue
			}
			return nil, false
		}
		m, ok := configMap(value)
		name, isStr := key.(string)
		if !ok || !isStr {
			return nil, false
		}
		next, present := m[name]
		if !present {
			return nil, false
		}
		value = next
	}
	return value, true
}

// Tuple form of a provider config path
func configPathTuple(p *provider.ConfigPath) secretPath {
	if p == nil {
		return nil
	}
	if p.Section == "custom_providers" {
		return secretPath{p.Section, p.Index}
	}
	return secretPath{p.Section, p.Key}
}

// Secret paths the session route authenticates with, its model block, alias key and routed provider entry
func routeSecretPaths(ctx context.Context, policy *LocalSessionPolicy) map[string]bool {
	cfg := policyObject(policy.ConfigJSON)
	model, _ := cfg["model"].(map[string]any)
	paths := map[string]bool{}
	for key := range model {
		paths[pathKey(secretPath{"model", key})] = true
	}
	if from, ok := model["api_key_from"].([]any); ok {
		paths[pathKey(from)] = true
	}
	providerName := routeStr(model["provider"])
	var selected secretPath
	if strings.ToLower(strings.TrimSpace(providerName)) == "custom" {
		selected = configPathTuple(provider.CustomProviderConfigPathForURL(model["base_url"], cfg))
	} else {
		selected = configPathTuple(provider.NamedCustomProviderPath(ctx, providerName, cfg))
	}
	if selected != nil {
		if entry, ok := definedAt(cfg, selected); ok {
			if fields, isMap := entry.(map[string]any); isMap {
				for field := range fields {
					paths[pathKey(append(append(secretPath{}, selected...), field))] = true
				}
			}
		}
	}
	return paths
}

// Optional policy text as its Python value
func optValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Field dict of a policy as dataclasses asdict gives it
func (p *LocalSessionPolicy) asDict() map[string]any {
	toolsets := make([]any, 0, len(p.Toolsets))
	for _, name := range p.Toolsets {
		toolsets = append(toolsets, name)
	}
	return map[string]any{
		"source": p.Source, "platform": p.Platform, "cwd": p.Cwd, "model": optValue(p.Model), "toolsets": toolsets,
		"config_json": p.ConfigJSON, "request_json": p.RequestJSON, "terminal_json": p.TerminalJSON,
		"credential_ref": optValue(p.CredentialRef), "config_secret_ref": optValue(p.ConfigSecretRef),
		"editor_mcp_json": optValue(p.EditorMCPJSON), "kanban_json": optValue(p.KanbanJSON), "safe_mode": p.SafeMode,
		"ignore_user_config": p.IgnoreUserConfig, "skills_prompt": optValue(p.SkillsPrompt),
		"coding_posture": p.CodingPosture, "client_env_json": optValue(p.ClientEnvJSON),
		"client_env_remote": p.ClientEnvRemote, "client_env_rules_json": optValue(p.ClientEnvRulesJSON),
		"edit_approval_policy": p.EditApprovalPolicy,
	}
}

// Hex sha256 of sorted key JSON
func sortedDigest(value any) string {
	text, _ := pyjson.Dumps(value, pyjson.SortKeys(true))
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Whole policy hash without its references, later fields hashed only when set
func policyIdentity(policy *LocalSessionPolicy, unhashed []string) string {
	data := policy.asDict()
	delete(data, "credential_ref")
	delete(data, "config_secret_ref")
	if policy.EditorMCPJSON == nil {
		delete(data, "editor_mcp_json")
	}
	if !policy.CodingPosture {
		delete(data, "coding_posture")
	}
	if policy.ClientEnvJSON == nil {
		delete(data, "client_env_json")
	}
	if policy.ClientEnvRulesJSON == nil {
		delete(data, "client_env_rules_json")
	}
	if !policy.ClientEnvRemote {
		delete(data, "client_env_remote")
	}
	if policy.EditApprovalPolicy == "ask" {
		delete(data, "edit_approval_policy")
	}
	for _, name := range unhashed {
		delete(data, name)
	}
	return sortedDigest(data)
}

// Every whole policy hash an older gateway could have stamped, later fields dropped while at their defaults
func legacyIdentities(policy *LocalSessionPolicy) map[string]bool {
	var unset []string
	if policy.SkillsPrompt == nil {
		unset = append(unset, "skills_prompt")
	}
	if policy.KanbanJSON == nil {
		unset = append(unset, "kanban_json")
	}
	if !policy.SafeMode {
		unset = append(unset, "safe_mode")
	}
	if !policy.IgnoreUserConfig {
		unset = append(unset, "ignore_user_config")
	}
	out := map[string]bool{}
	for mask := 0; mask < 1<<len(unset); mask++ {
		var dropped []string
		for i, name := range unset {
			if mask&(1<<i) != 0 {
				dropped = append(dropped, name)
			}
		}
		out[policyIdentity(policy, dropped)] = true
	}
	return out
}

// Secret fingerprint of a value, sorted JSON for structures, nil for a blank value
func secretFingerprint(value any) any {
	text, isStr := value.(string)
	if !isStr {
		text, _ = pyjson.Dumps(value, pyjson.SortKeys(true))
	}
	if fp := provider.FingerprintSecretValue(text); fp != "" {
		return fp
	}
	return nil
}

var (
	endpointKeys   = []string{"provider", "base_url", "api", "url", "command", "args", "api_mode"}
	entrySections  = map[string]bool{"providers": true, "custom_providers": true}
	auxFollowsMain = map[string]bool{"": true, "auto": true, "main": true}
	urlKeys        = []string{"base_url", "api", "url"}
)

// Whether a path element is the given section name
func isSection(v any, name string) bool {
	s, ok := v.(string)
	return ok && s == name
}

// Route block path a secret at the path rides with, nil when it rides with no route
func routeOwner(config map[string]any, path secretPath) secretPath {
	head, rest := path[0], path[1:]
	model, _ := config["model"].(map[string]any)
	if from, ok := model["api_key_from"].([]any); ok && pathEqual(from, path) {
		return secretPath{"model"}
	}
	switch {
	case isSection(head, "model"):
		if len(rest) > 2 && isSection(rest[0], "aliases") {
			return secretPath{"model", "aliases", rest[1]}
		}
		return secretPath{"model"}
	case isSection(head, "model_aliases") && len(rest) > 1:
		return secretPath{"model_aliases", rest[0]}
	case isSection(head, "fallback_model") || isSection(head, "fallback_providers"):
		if _, isList := config[head.(string)].([]any); isList && len(rest) > 0 {
			if _, isInt := pathIndex(rest[0]); isInt {
				return secretPath{head, rest[0]}
			}
		}
		return secretPath{head}
	case isSection(head, "auxiliary") && len(rest) > 1:
		task := rest[0]
		if isSection(rest[1], "fallback_chain") && len(rest) > 3 {
			if _, isInt := pathIndex(rest[2]); isInt {
				return secretPath{"auxiliary", task, "fallback_chain", rest[2]}
			}
		}
		return secretPath{"auxiliary", task}
	}
	return nil
}

// Block path whose endpoint the secret authenticates, a route, a whole provider entry or its parent block
func ownerBlock(config map[string]any, path secretPath) secretPath {
	if route := routeOwner(config, path); route != nil {
		return route
	}
	if head, ok := path[0].(string); ok && entrySections[head] {
		if len(path) > 2 {
			return path[:2]
		}
		return path
	}
	return path[:len(path)-1]
}

// Provider identity and URL the route block at the owner path is actually sent to
func resolvedEndpoint(ctx context.Context, owner secretPath, block, config map[string]any) ([]any, error) {
	if isSection(owner[0], "auxiliary") {
		providerName := strings.ToLower(strings.TrimSpace(routeStr(block["provider"])))
		if auxFollowsMain[providerName] && strings.TrimSpace(routeStr(block["base_url"])) == "" {
			main, _ := config["model"].(map[string]any)
			if main == nil {
				main = map[string]any{}
			}
			identity, baseURL, err := ResolvedRoute(ctx, main, config)
			if err != nil {
				return nil, err
			}
			return []any{"main", identity, baseURL}, nil
		}
	}
	identity, baseURL, err := ResolvedRoute(ctx, block, config)
	if err != nil {
		return nil, err
	}
	return []any{identity, baseURL}, nil
}

// Route identity form of an endpoint URL
func endpointURL(value any) string {
	return provider.NormalizeRouteBaseURL(strings.TrimSpace(routeStr(value)))
}

// Digest of the endpoint the secret at the path authenticates in the frozen config
func secretBinding(ctx context.Context, config map[string]any, path secretPath) (string, error) {
	var owner secretPath
	if len(path) > 0 && path[0] != nil {
		owner = ownerBlock(config, path)
	}
	var block map[string]any
	if len(owner) > 0 {
		if value, ok := definedAt(config, owner); ok {
			block, _ = value.(map[string]any)
		}
	}
	endpoint := map[string]any{}
	for _, key := range endpointKeys {
		if value, ok := block[key]; ok {
			endpoint[key] = value
		}
	}
	if block != nil && routeOwner(config, path) != nil {
		resolved, err := resolvedEndpoint(ctx, owner, block, config)
		if err != nil {
			return "", err
		}
		resolved[len(resolved)-1] = endpointURL(resolved[len(resolved)-1])
		endpoint["resolved"] = resolved
	} else if head, ok := path[0].(string); block != nil && ok && entrySections[head] {
		endpoint["resolved"] = endpointURL(provider.EntryURL(block))
	}
	for _, key := range urlKeys {
		if s, ok := endpoint[key].(string); ok {
			endpoint[key] = endpointURL(s)
		}
	}
	return sortedDigest([]any{[]any(path), endpoint}), nil
}

// Profile home holding the authority database
func launchOwnerHome(owner launchSecretOwner) (string, error) {
	resolved, err := pypath.Resolve(owner.launchOwnerDBPath())
	if err != nil {
		return "", err
	}
	return filepath.Dir(resolved), nil
}

// Durable reference binding each frozen secret to its home, profile, session and authenticated endpoint
func configReference(ctx context.Context, owner launchSecretOwner, sessionID string, policy *LocalSessionPolicy, secrets *RecoveredSecrets) (string, error) {
	home, err := launchOwnerHome(owner)
	if err != nil {
		return "", err
	}
	config := policyObject(policy.ConfigJSON)
	bound, entries := []any{}, []any{}
	for i, path := range secrets.paths {
		digest, err := secretBinding(ctx, config, path)
		if err != nil {
			return "", err
		}
		bound = append(bound, []any{[]any(path), digest})
		entries = append(entries, []any{[]any(path), secretFingerprint(secrets.values[i])})
	}
	text, err := pyjson.Dumps(map[string]any{"home": home, "profile": owner.launchOwnerProfileID(), "session": sessionID,
		"policy": policyIdentity(policy, nil), "bound": bound, "entries": entries}, pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	return configSecretPrefix + text, nil
}

// Dotted field name of a secret path
func secretField(path []any) string {
	var parts []string
	for _, part := range path {
		if part != nil {
			parts = append(parts, pyrepr.Str(part))
		}
	}
	return strings.Join(parts, ".")
}

// Launch credentials refusal naming the field and why
func credentialsRefusal(field, detail string) error {
	return state.NewRuntimeStoreError("launch_credentials_unavailable", &field, &detail)
}

// Python exception a malformed reference raises, reported by class name
func referenceFault(kind string) error { return &PyError{Kind: kind, Msg: kind} }

// Path list of a decoded reference row, a TypeError when the row is not a two item pair
func referencePair(row any) (secretPath, any, error) {
	pair, ok := row.([]any)
	if !ok {
		return nil, nil, referenceFault("TypeError")
	}
	if len(pair) != 2 {
		return nil, nil, referenceFault("ValueError")
	}
	path, ok := pair[0].([]any)
	if !ok {
		return nil, nil, referenceFault("TypeError")
	}
	if len(path) == 0 {
		return nil, nil, &PyError{Kind: "IndexError", Msg: "list index out of range"}
	}
	return secretPath(path), pair[1], nil
}

// Constant time digest comparison, a TypeError when the stored digest is not text
func digestMatches(actual string, stored any) (bool, error) {
	s, ok := stored.(string)
	if !ok {
		return false, referenceFault("TypeError")
	}
	return hmac.Equal([]byte(actual), []byte(s)), nil
}

// Whether a path head is one of the skipped sections
func skipped(path secretPath, skip map[any]bool) bool {
	return skip[path[0]]
}

// Refuses a reference from another home or profile or a secret whose endpoint moved without a re-bind
func checkSecretScope(ctx context.Context, owner launchSecretOwner, source map[string]any, policy *LocalSessionPolicy, home string, skip map[any]bool) error {
	for _, scope := range [][2]string{{"home", home}, {"profile", owner.launchOwnerProfileID()}} {
		value, ok := source[scope[0]]
		if !ok {
			return referenceFault("KeyError")
		}
		if s, isStr := value.(string); !isStr || s != scope[1] {
			return credentialsRefusal(scope[0], "the frozen config credentials belong to another "+scope[0])
		}
	}
	bound, hasBound := source["bound"]
	if !hasBound {
		stamped, ok := source["policy"]
		if !ok {
			return referenceFault("KeyError")
		}
		switch stamped.(type) {
		case []any, map[string]any:
			return referenceFault("TypeError")
		}
		if s, isStr := stamped.(string); !isStr || !legacyIdentities(policy)[s] {
			return credentialsRefusal("config_secret_ref", "stamped by an older gateway for another session policy")
		}
		return nil
	}
	rows, ok := bound.([]any)
	if !ok {
		return referenceFault("TypeError")
	}
	config := policyObject(policy.ConfigJSON)
	for _, row := range rows {
		path, digest, err := referencePair(row)
		if err != nil {
			return err
		}
		if skipped(path, skip) {
			continue
		}
		binding, err := secretBinding(ctx, config, path)
		if err != nil {
			return err
		}
		match, err := digestMatches(binding, digest)
		if err != nil {
			return err
		}
		if !match {
			return credentialsRefusal(secretField(path), "its endpoint moved in the session policy without a re-bind")
		}
	}
	return nil
}

// Decoded body of a durable reference
func referenceSource(ref string) (any, error) {
	parsed, err := pyjson.Loads(strings.TrimPrefix(ref, configSecretPrefix))
	if err != nil {
		return nil, &PyError{Kind: "JSONDecodeError", Msg: err.Error()}
	}
	return pyjson.Plain(parsed), nil
}

// Whether the current private values of the sections differ by fingerprint from those the reference froze
func secretsMoved(ref *string, secrets *RecoveredSecrets, sections []any) (bool, error) {
	if ref != nil && !strings.HasPrefix(*ref, configSecretPrefix) {
		return false, nil
	}
	in := map[any]bool{}
	for _, name := range sections {
		in[name] = true
	}
	frozen := map[string]any{}
	if ref != nil && *ref != "" {
		source, err := referenceSource(*ref)
		if err != nil {
			return false, err
		}
		body, ok := source.(map[string]any)
		if !ok {
			return false, referenceFault("TypeError")
		}
		entries, ok := body["entries"]
		if !ok {
			return false, referenceFault("KeyError")
		}
		rows, ok := entries.([]any)
		if !ok {
			return false, referenceFault("TypeError")
		}
		for _, row := range rows {
			path, fingerprint, err := referencePair(row)
			if err != nil {
				return false, err
			}
			if in[path[0]] {
				frozen[pathKey(path)] = fingerprint
			}
		}
	}
	current := map[string]any{}
	for i, path := range secrets.paths {
		current[pathKey(path)] = secretFingerprint(secrets.values[i])
	}
	return !reflect.DeepEqual(frozen, current), nil
}

// Frozen config secrets with followed sections skipped for a following session
func recoverConfigSecrets(ctx context.Context, owner launchSecretOwner, policy *LocalSessionPolicy) (*RecoveredSecrets, error) {
	skip := map[any]bool{}
	if policy.FollowsProfile() {
		for _, name := range FollowSections {
			skip[name] = true
		}
	}
	return recoverConfigSecretsSkipping(ctx, owner, policy, skip)
}

// Frozen config secrets, entries under a skipped section neither re-read nor checked
func recoverConfigSecretsSkipping(ctx context.Context, owner launchSecretOwner, policy *LocalSessionPolicy, skip map[any]bool) (*RecoveredSecrets, error) {
	ref := ""
	if policy.ConfigSecretRef != nil {
		ref = *policy.ConfigSecretRef
	}
	slots := owner.launchSecretSlots()
	held := func() *RecoveredSecrets {
		if slots == nil {
			return nil
		}
		slots.mu.Lock()
		defer slots.mu.Unlock()
		return slots.configs[ref]
	}
	if !strings.HasPrefix(ref, configSecretPrefix) {
		values := held()
		if values == nil {
			return nil, state.NewRuntimeStoreError("launch_credentials_unavailable", nil, nil)
		}
		return values.without(skip), nil
	}
	values, err := recoverReferencedSecrets(ctx, owner, policy, ref, skip, held)
	if err == nil {
		return values, nil
	}
	if _, refused := err.(*state.RuntimeStoreError); refused {
		return nil, err
	}
	if fault, ok := err.(*PyError); ok && fault.Kind != "IndexError" || pypath.IsOSError(err) {
		kind := "OSError"
		if ok {
			kind = fault.Kind
		}
		slog.Warn(fmt.Sprintf("Frozen config reference unreadable: %s", kind))
		return nil, credentialsRefusal("config_secret_ref", "unreadable ("+kind+")")
	}
	return nil, err
}

// Reference body checked against this owner and each entry re-read from the profile config
func recoverReferencedSecrets(ctx context.Context, owner launchSecretOwner, policy *LocalSessionPolicy, ref string, skip map[any]bool, held func() *RecoveredSecrets) (*RecoveredSecrets, error) {
	parsed, err := referenceSource(ref)
	if err != nil {
		return nil, err
	}
	source, ok := parsed.(map[string]any)
	if !ok {
		return nil, referenceFault("TypeError")
	}
	home, err := launchOwnerHome(owner)
	if err != nil {
		return nil, err
	}
	if err := checkSecretScope(ctx, owner, source, policy, home, skip); err != nil {
		return nil, err
	}
	if cached := held(); cached != nil {
		return cached.without(skip), nil
	}
	rawEntries, ok := source["entries"]
	if !ok {
		return nil, referenceFault("KeyError")
	}
	rows, ok := rawEntries.([]any)
	if !ok {
		return nil, referenceFault("TypeError")
	}
	var paths []secretPath
	var fingerprints []any
	needsConfig := false
	for _, row := range rows {
		path, fingerprint, err := referencePair(row)
		if err != nil {
			return nil, err
		}
		if skipped(path, skip) {
			continue
		}
		paths = append(paths, path)
		fingerprints = append(fingerprints, fingerprint)
		needsConfig = needsConfig || path[0] != nil
	}
	config := map[string]any{}
	if needsConfig {
		config = loadGatewayConfig(ctx, filepath.Join(home, "config.yaml"))
	}
	var route map[string]bool
	var frozen map[string]any
	values := &RecoveredSecrets{session: source["session"]}
	for i, path := range paths {
		var root any = config
		lookup := []any(path)
		if index, isInt := pathIndex(pathAt(path, 1)); isSection(path[0], "custom_providers") && len(path) > 2 && isInt {
			if frozen == nil {
				frozen = policyObject(policy.ConfigJSON)
			}
			lookup = currentCustomPath(frozen, config, path, index)
		}
		if path[0] == nil {
			terminal, err := tools.BuildProfileTerminalScope(ctx, home, nil)
			if err != nil {
				return nil, err
			}
			root, lookup = terminal, path[1:]
		}
		value, present := definedAt(root, lookup)
		var actual any
		if present {
			actual = secretFingerprint(value)
		}
		match := false
		if actual != nil {
			if match, err = digestMatches(actual.(string), fingerprints[i]); err != nil {
				return nil, err
			}
		}
		if !match {
			if route == nil {
				route = routeSecretPaths(ctx, policy)
			}
			if route[pathKey(path)] {
				how := "changed in"
				if !present {
					how = "removed from"
				}
				return nil, credentialsRefusal(secretField(path), how+" config.yaml since this session started")
			}
			slog.Warn(fmt.Sprintf("Session %s: frozen config secret %s no longer matches the profile config; skipping it",
				pyrepr.Str(source["session"]), pyrepr.Str([]any(path))))
			values.dropped = append(values.dropped, path)
			continue
		}
		values.Set(path, value)
	}
	return values, nil
}

// Path element at an index, nil past the end
func pathAt(path []any, i int) any {
	if i < len(path) {
		return path[i]
	}
	return nil
}

// Canonical identity of a custom provider entry, false for an entry with no name
func customEntryIdentity(entry any) (string, bool) {
	m, ok := entry.(map[string]any)
	if !ok || !(pyrepr.Truthy(m["name"]) || pyrepr.Truthy(m["provider_key"])) {
		return "", false
	}
	return provider.CustomProviderSlug(routeStr(m["name"]), routeStr(m["provider_key"])), true
}

// Custom provider path repointed at the profile entry of the same canonical identity wherever it moved
func currentCustomPath(frozen, config map[string]any, path secretPath, index int) []any {
	held, _ := frozen["custom_providers"].([]any)
	mine, named := "", false
	if held != nil && index >= 0 && index < len(held) {
		mine, named = customEntryIdentity(held[index])
	}
	current, isList := config["custom_providers"].([]any)
	if !named || !isList {
		return path
	}
	if index >= 0 && index < len(current) {
		if identity, ok := customEntryIdentity(current[index]); ok && identity == mine {
			return path
		}
	}
	repointed := func(i int) []any {
		return append([]any{path[0], i}, path[2:]...)
	}
	for i, entry := range current {
		if identity, ok := customEntryIdentity(entry); ok && identity == mine {
			return repointed(i)
		}
	}
	if index >= 0 && index < len(current) {
		if entry, ok := current[index].(map[string]any); ok {
			was, _ := held[index].(map[string]any)
			if provider.NormalizeBaseURLForMatch(provider.EntryURL(entry)) == provider.NormalizeBaseURLForMatch(provider.EntryURL(was)) {
				return path
			}
		}
	}
	return repointed(len(current))
}

// Providers entry a fallback entry authenticates through
func entryOwnerPath(ctx context.Context, entry, config map[string]any) secretPath {
	providerName := strings.TrimSpace(routeStr(entry["provider"]))
	if strings.ToLower(providerName) == "custom" {
		return configPathTuple(provider.CustomProviderConfigPathForURL(entry["base_url"], config))
	}
	if providerName == "" {
		return nil
	}
	return configPathTuple(provider.NamedCustomProviderPath(ctx, providerName, config))
}

// Field of a fallback entry for the removal warning, a question mark for a non mapping entry
func fallbackLabel(entry any, key string) string {
	if m, ok := entry.(map[string]any); ok {
		return pyrepr.Str(m[key])
	}
	return "?"
}

// Config with what used a dropped secret disabled, fallbacks removed and auxiliary tasks on the primary route
func disableDroppedSecrets(ctx context.Context, config map[string]any, dropped []secretPath, session any) map[string]any {
	if len(dropped) == 0 {
		return config
	}
	auxiliary, _ := config["auxiliary"].(map[string]any)
	tasks := map[string]any{}
	for _, path := range dropped {
		if isSection(path[0], "auxiliary") && len(path) > 2 {
			tasks[pyrepr.Str(path[1])] = path[1]
		}
	}
	order := make([]string, 0, len(tasks))
	for label := range tasks {
		order = append(order, label)
	}
	sort.Strings(order)
	for _, label := range order {
		task, isStr := tasks[label].(string)
		if !isStr || auxiliary == nil {
			continue
		}
		block, ok := auxiliary[task].(map[string]any)
		if !ok {
			continue
		}
		replaced := map[string]any{}
		for k, v := range block {
			if !provider.AuxRouteKeys[k] && k != "fallback_chain" {
				replaced[k] = v
			}
		}
		replaced["provider"] = "main"
		auxiliary[task] = replaced
		slog.Warn(fmt.Sprintf("Session %s: auxiliary task %s returned to the primary route: its credential changed in "+
			"config.yaml since this session started", pyrepr.Str(session), task))
	}
	owners := map[string]bool{}
	for _, path := range dropped {
		if head, ok := path[0].(string); ok && entrySections[head] && len(path) > 2 {
			owners[pathKey(path[:2])] = true
		}
	}
	for _, section := range []string{"fallback_providers", "fallback_model"} {
		var entries []any
		single := false
		switch raw := config[section].(type) {
		case map[string]any:
			entries, single = []any{raw}, true
		case []any:
			entries = append([]any{}, raw...)
		default:
			continue
		}
		kept := []any{}
		for index, entry := range entries {
			prefix := []any{section}
			if !single {
				prefix = []any{section, index}
			}
			own := false
			for _, path := range dropped {
				if len(path) >= len(prefix) && pathEqual(path[:len(prefix)], prefix) {
					own = true
					break
				}
			}
			var owner secretPath
			if m, isMap := entry.(map[string]any); isMap && len(owners) > 0 {
				owner = entryOwnerPath(ctx, m, config)
			}
			if own || (owner != nil && owners[pathKey(owner)]) {
				slog.Warn(fmt.Sprintf("Session %s: fallback %s/%s removed: its credential changed in config.yaml since this "+
					"session started", pyrepr.Str(session), fallbackLabel(entry, "provider"), fallbackLabel(entry, "model")))
				continue
			}
			kept = append(kept, entry)
		}
		if single {
			if len(kept) > 0 {
				config[section] = kept[0]
			} else {
				delete(config, section)
			}
		} else {
			config[section] = kept
		}
	}
	return config
}
