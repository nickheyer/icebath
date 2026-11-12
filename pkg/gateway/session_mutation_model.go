package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Provider, model, endpoint and key of the route a session runs now
type switchRoute struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
}

// Runner that resolves the model and runtime a session's agent runs
type agentRuntimeResolver interface {
	ResolveSessionAgentRuntime(ctx context.Context, source *SessionSource, sessionKey string, userConfig map[string]any, fallback bool) (string, map[string]any, error)
}

// Runtime resolver of the authority's runner, an AttributeError when it has none
func runtimeResolver(a *SessionAuthority) (agentRuntimeResolver, error) {
	resolver, ok := a.Runner.(agentRuntimeResolver)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_resolve_session_agent_runtime'", a.Runner)}
	}
	return resolver, nil
}

// String field of a mapping, empty when absent or falsy
func mapText(m map[string]any, key string) string {
	if v, ok := m[key]; ok && pyTruthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Config and route a session runs now, both from one read of the owning profile
func currentModelRoute(ctx context.Context, a *SessionAuthority, live *LiveSession, old *LocalSessionPolicy) (map[string]any, switchRoute, error) {
	scoped, err := profileReadScope(ctx, old, a)
	if err != nil {
		return nil, switchRoute{}, err
	}
	cfg, err := old.config(scoped, a, true)
	if err != nil {
		return nil, switchRoute{}, err
	}
	block, _ := cfg["model"].(map[string]any)
	resolver, err := runtimeResolver(a)
	if err != nil {
		return nil, switchRoute{}, err
	}
	model, runtime, err := resolver.ResolveSessionAgentRuntime(scoped, live.Source, live.Route, nil, false)
	if err != nil {
		var authErr *provider.AuthError
		if !errors.As(err, &authErr) {
			return nil, switchRoute{}, err
		}
		current, err := old.currentModel(scoped, a)
		if err != nil {
			return nil, switchRoute{}, err
		}
		route := switchRoute{Provider: mapText(block, "provider"), BaseURL: mapText(block, "base_url")}
		if current != nil {
			route.Model = *current
		}
		return cfg, route, nil
	}
	route := switchRoute{Provider: mapText(block, "provider"), Model: model, BaseURL: mapText(runtime, "base_url"), APIKey: mapText(runtime, "api_key")}
	if route.Provider == "" {
		route.Provider = mapText(runtime, "provider")
	}
	return cfg, route, nil
}

// Directory holding the authority's state database, its profile home
func authorityHome(a *SessionAuthority) string {
	path := a.DB.Path()
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Dir(path)
}

// Freezes an explicit model pick into the policy, resolved like every surface's model command
func prepareModel(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	cfg, current, err := currentModelRoute(ctx, a, live, old)
	if err != nil {
		return nil, err
	}
	reset, err := isModelReset(payload)
	if err != nil {
		return nil, err
	}
	if reset {
		if payload, err = resetModelPayload(ctx, a, old, current); err != nil {
			return nil, err
		}
		if _, has := payload.Get("status"); has {
			return payload, nil
		}
	}
	resolution := cfg
	if !old.FollowsProfile() {
		resolution = providerResolutionConfig(ctx, a, cfg, old.IgnoreUserConfig)
	}
	userProviders, _ := resolution["providers"].(map[string]any)
	var customProviders []any
	for _, entry := range provider.CompatibleCustomProviders(resolution) {
		customProviders = append(customProviders, entry)
	}
	explicit := ""
	if value, has := payload.Get("provider"); has {
		explicit = pyrepr.Str(value)
	}
	raw, _ := payload.Get("model")
	result, err := provider.SwitchModel(ctx, provider.SwitchModelOptions{RawInput: pyrepr.Str(raw), CurrentProvider: current.Provider,
		CurrentBaseURL: current.BaseURL, CurrentAPIKey: current.APIKey, ExplicitProvider: explicit, UserProviders: userProviders, CustomProviders: customProviders})
	if err != nil {
		return nil, err
	}
	credential, err := aliasCredential(ctx, a, result)
	if err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, runtimeRefusal("model_resolution_failed")
	}
	if !reset {
		snapshot, _ := prepared.Get("snapshot")
		refusal, err := selectionGuardRefusal(ctx, a, live, payload, snapshot, current, result)
		if err != nil || refusal != nil {
			return refusal, err
		}
	}
	frozen, err := old.config(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	providerSecrets, retired, moved := &RecoveredSecrets{}, map[string]bool{}, map[string]secretPath{}
	if old.FollowsProfile() {
		followed, _ := cfg["model"].(map[string]any)
		held, err := boundRouteHolds(ctx, frozen["model"], frozen, followed, cfg)
		if err != nil {
			return nil, err
		}
		credentialKeys := map[string]bool{"api_key": true, "api": true, "api_key_from": true}
		routeKeys := map[string]bool{}
		for _, key := range RouteBoundKeys {
			if !held || !credentialKeys[key] {
				routeKeys[key] = true
			}
		}
		block := map[string]any{}
		if prior, ok := frozen["model"].(map[string]any); ok {
			for k, v := range prior {
				if !routeKeys[k] {
					block[k] = v
				}
			}
		}
		for _, key := range RouteBoundKeys {
			if value, has := followed[key]; has && routeKeys[key] && !credentialKeys[key] {
				block[key] = value
			}
		}
		frozen["model"] = block
	} else if providerSecrets, retired, moved, err = freezeSelectedProvider(ctx, resolution, frozen, result.TargetProvider); err != nil {
		return nil, err
	}
	model := provider.ApplyModelSelection(frozen["model"], result, false)
	frozen["model"] = model
	if effort, has := payload.Get("reasoning"); has {
		if err := pinReasoningEffort(frozen, effort, true); err != nil {
			return nil, err
		}
	}
	if old.FollowsProfile() {
		profile := loadGatewayConfig(ctx, filepath.Join(authorityHome(a), "config.yaml"))
		name, target := profileTarget(presentSections(profile))
		model["selection_profile"] = []any{name, target}
	}
	keyEnv := credential.keyEnv
	if keyEnv == "" {
		keyEnv = selectedProviderKeyEnv(resolution, result.TargetProvider)
	}
	if keyEnv != "" {
		model["key_env"] = keyEnv
	}
	if credential.source != nil {
		model["api_key"], model["api_key_from"] = nil, []any(credential.source)
	} else if _, has := model["api_key"]; !has {
		delete(model, "api_key_from")
	}
	configJSON, err := pyjson.Dumps(frozen)
	if err != nil {
		return nil, err
	}
	policy := *old
	policy.Model, policy.ConfigJSON = &result.NewModel, configJSON
	secrets := &RecoveredSecrets{}
	if old.ConfigSecretRef != nil {
		recovered, err := recoverConfigSecrets(ctx, a, old)
		if err != nil {
			return nil, err
		}
		for i, path := range recovered.paths {
			head := path
			if len(head) > 2 {
				head = head[:2]
			}
			if target, ok := moved[secretPathKey(head)]; ok {
				path = append(append(secretPath{}, target...), path[len(head):]...)
				head = target
			}
			if retired[secretPathKey(head)] {
				continue
			}
			secrets.Set(path, recovered.values[i])
		}
	}
	for i, path := range providerSecrets.paths {
		secrets.Set(path, providerSecrets.values[i])
	}
	filtered := &RecoveredSecrets{}
	for i, path := range secrets.paths {
		if len(path) == 2 && path[0] == "model" {
			if _, has := model[pyrepr.Str(path[1])]; !has {
				continue
			}
		}
		filtered.Set(path, secrets.values[i])
	}
	secrets = filtered
	if credential.source != nil {
		secrets = secrets.dropping(secretPath{"model", "api_key"})
		secrets.Set(credential.source, credential.literal)
	}
	key, err := routeLaunchKey(ctx, a, old, cfg)
	if err != nil {
		return nil, err
	}
	holds := false
	if key != nil {
		if holds, err = boundRouteHolds(ctx, cfg["model"], cfg, model, cfg); err != nil {
			return nil, err
		}
	}
	if key == nil || !holds {
		policy.CredentialRef, key = nil, nil
	}
	sid, err := preparedSessionID(prepared)
	if err != nil {
		return nil, err
	}
	bound, err := bindLaunchKey(ctx, a, sid, &policy, key, secrets)
	if err != nil {
		return nil, err
	}
	return preparedWith(prepared, "policy", bound.asDict()), nil
}

// Copy of the secrets without one path
func (r *RecoveredSecrets) dropping(path secretPath) *RecoveredSecrets {
	out := &RecoveredSecrets{session: r.session}
	for i, held := range r.paths {
		if !pathEqual(held, path) {
			out.Set(held, r.values[i])
		}
	}
	return out
}

// Comparable key of a secret path prefix
func secretPathKey(path secretPath) string {
	parts := make([]string, len(path))
	for i, part := range path {
		if n, ok := pathIndex(part); ok {
			parts[i] = fmt.Sprintf("%d", n)
			continue
		}
		parts[i] = pyrepr.Repr(part)
	}
	return strings.Join(parts, "\x1f")
}

// Whether a model payload is the reset token, refusing it beside any other selection field
func isModelReset(payload *pyjson.Object) (bool, error) {
	raw, err := pyItem(payload, "model")
	if err != nil {
		return false, err
	}
	if strings.ToLower(strings.TrimSpace(pyrepr.Str(raw))) != provider.ModelSwitchResetToken {
		return false, nil
	}
	for _, key := range payload.Keys {
		if key != "model" && key != "confirm" {
			field, detail := "payload", provider.ModelSwitchErrorText[provider.ModelSwitchErrResetWithFlags]
			return false, state.NewRuntimeStoreError("invalid_params", &field, &detail)
		}
	}
	return true, nil
}

// Model and provider the owning profile's config.yaml names now, auto reading as no provider
func configuredDefault(ctx context.Context, a *SessionAuthority) (string, string, error) {
	profile, err := profileConfig(ctx, a)
	if err != nil {
		return "", "", err
	}
	block := profile["model"]
	if !pyTruthy(block) {
		return "", "", nil
	}
	name := ""
	if fields, ok := block.(map[string]any); ok {
		name = mapText(fields, "provider")
	}
	if strings.ToLower(name) == "auto" {
		name = ""
	}
	return resolveGatewayModel(ctx, profile), name, nil
}

// Pick of the profile's configured model for a reset, a noop report when the session already runs it
func resetModelPayload(ctx context.Context, a *SessionAuthority, old *LocalSessionPolicy, current switchRoute) (*pyjson.Object, error) {
	model, name := "", ""
	if !old.IgnoreUserConfig {
		var err error
		if model, name, err = configuredDefault(ctx, a); err != nil {
			return nil, err
		}
	}
	if model == "" {
		field, detail := "payload.model", i18n.T(ctx, "cli.model.reset_no_default", "", nil)
		return nil, state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	if model == current.Model && (name == "" || name == current.Provider) {
		return pyjson.ObjectOf("status", "noop", "model", model, "note", i18n.T(ctx, "cli.model.reset_none", "", map[string]any{"model": model})), nil
	}
	out := pyjson.ObjectOf("model", model)
	if name != "" {
		out.Set("provider", name)
	}
	return out, nil
}

// Config with the owning profile's current named providers over its frozen ones, used only to resolve a pick
func providerResolutionConfig(ctx context.Context, a *SessionAuthority, cfg map[string]any, bypass bool) map[string]any {
	if bypass {
		return cfg
	}
	current := loadProfileConfig(ctx, authorityHome(a))
	if current == nil {
		return cfg
	}
	resolved := make(map[string]any, len(cfg)+2)
	for k, v := range cfg {
		resolved[k] = v
	}
	frozenProviders, frozenIsMap := cfg["providers"].(map[string]any)
	currentProviders, currentIsMap := current["providers"].(map[string]any)
	if frozenIsMap || currentIsMap {
		slugs := map[string]bool{}
		for key := range currentProviders {
			slugs[providerKeySlug(key)] = true
		}
		merged := map[string]any{}
		for key, entry := range frozenProviders {
			if !slugs[providerKeySlug(key)] {
				merged[key] = entry
			}
		}
		for key, entry := range currentProviders {
			merged[key] = entry
		}
		resolved["providers"] = merged
	}
	frozenCustom, frozenIsList := cfg["custom_providers"].([]any)
	currentCustom, currentIsList := current["custom_providers"].([]any)
	if frozenIsList || currentIsList {
		names := map[string]bool{}
		for _, entry := range currentCustom {
			if fields, ok := entry.(map[string]any); ok {
				names[customProviderName(fields)] = true
			}
		}
		merged := append([]any{}, currentCustom...)
		for _, entry := range frozenCustom {
			if fields, ok := entry.(map[string]any); !ok || !names[customProviderName(fields)] {
				merged = append(merged, entry)
			}
		}
		resolved["custom_providers"] = merged
		resolved["_profile_custom_count"] = len(currentCustom)
	}
	return resolved
}

// Canonical identity of a custom provider entry, display and key spellings folded together
func customProviderName(entry map[string]any) string {
	return provider.CustomProviderSlug(mapText(entry, "name"), mapText(entry, "provider_key"))
}

// Credential pointer a selected named provider declares, never its secret value
func selectedProviderKeyEnv(cfg map[string]any, name string) string {
	providers, _ := cfg["providers"].(map[string]any)
	entry, ok := providers[name].(map[string]any)
	if !ok {
		return ""
	}
	pointer := mapText(entry, "key_env")
	if pointer == "" {
		pointer = mapText(entry, "api_key_env")
	}
	return strings.TrimSpace(pointer)
}

// Canonical identity of a providers key, its spellings folded together
func providerKeySlug(key any) string {
	return provider.CustomProviderSlug("", pyrepr.Str(key))
}

// Copies only the picked named provider into the frozen route, returning its private values, retired paths and moved entries
func freezeSelectedProvider(ctx context.Context, cfg, frozen map[string]any, name string) (*RecoveredSecrets, map[string]bool, map[string]secretPath, error) {
	secrets, retired, moved := &RecoveredSecrets{}, map[string]bool{}, map[string]secretPath{}
	path := provider.NamedCustomProviderPath(ctx, name, cfg)
	if path == nil {
		return secrets, retired, moved, nil
	}
	section := path.Section
	var index any = path.Key
	var copied map[string]any
	switch entries := cfg[section].(type) {
	case map[string]any:
		entry, ok := entries[path.Key].(map[string]any)
		if !ok {
			return secrets, retired, moved, nil
		}
		copied = pyjson.Clone(entry).(map[string]any)
	case []any:
		index = path.Index
		entry, ok := entries[path.Index].(map[string]any)
		if !ok {
			return secrets, retired, moved, nil
		}
		copied = pyjson.Clone(entry).(map[string]any)
	default:
		return secrets, retired, moved, nil
	}
	retired[secretPathKey(secretPath{section, index})] = true
	if section == "providers" {
		previous, _ := frozen[section].(map[string]any)
		next := map[string]any{}
		for key, value := range previous {
			if providerKeySlug(key) == providerKeySlug(path.Key) {
				retired[secretPathKey(secretPath{section, key})] = true
				continue
			}
			next[key] = value
		}
		next[path.Key] = copied
		frozen[section] = next
	} else {
		var entries []any
		if list, ok := frozen[section].([]any); ok {
			entries = append(entries, list...)
		}
		entryName := customProviderName(copied)
		var same []int
		for i, entry := range entries {
			if fields, ok := entry.(map[string]any); ok && customProviderName(fields) == entryName {
				same = append(same, i)
			}
		}
		position := path.Index
		profileCount := len(cfg[section].([]any))
		if count, ok := cfg["_profile_custom_count"].(int); ok {
			profileCount = count
		}
		if position >= profileCount {
			if len(same) == 0 {
				return nil, nil, nil, &PyError{Kind: "IndexError", Msg: "list index out of range"}
			}
			position = same[0]
		}
		for len(entries) < position+1 {
			entries = append(entries, map[string]any{})
		}
		if displaced, ok := entries[position].(map[string]any); ok && len(displaced) > 0 && customProviderName(displaced) != entryName {
			moved[secretPathKey(secretPath{section, position})] = secretPath{section, len(entries)}
			entries = append(entries, displaced)
		}
		for _, i := range same {
			entries[i] = map[string]any{}
			retired[secretPathKey(secretPath{section, i})] = true
		}
		entries[position] = copied
		index = position
		retired[secretPathKey(secretPath{section, position})] = true
		frozen[section] = entries
	}
	if err := extractConfigSecrets(copied, secrets, secretPath{section, index}); err != nil {
		return nil, nil, nil, err
	}
	return secrets, retired, moved, nil
}

// Credential a direct alias declares for its own endpoint read from this owner's config.yaml
type aliasKey struct {
	keyEnv  string
	source  secretPath
	literal any
}

// Key pointer, private source path and literal key of the alias a switch resolved, all empty when it declares none
func aliasCredential(ctx context.Context, a *SessionAuthority, result provider.ModelSwitchResult) (aliasKey, error) {
	name := ""
	if result.Success {
		name = result.ResolvedViaAlias
	}
	if name == "" {
		return aliasKey{}, nil
	}
	path := filepath.Join(authorityHome(a), "config.yaml")
	raw, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		return aliasKey{}, err
	}
	for _, section := range [][]string{{"model_aliases"}, {"model", "aliases"}} {
		var entries map[string]any
		if len(section) == 1 {
			entries, _ = raw[section[0]].(map[string]any)
		} else if model, ok := raw["model"].(map[string]any); ok {
			entries, _ = model["aliases"].(map[string]any)
		}
		written := make([]string, 0, len(entries))
		for key := range entries {
			written = append(written, key)
		}
		sort.Strings(written)
		for _, alias := range written {
			entry, ok := entries[alias].(map[string]any)
			if !ok || !pyTruthy(entry["model"]) || strings.ToLower(strings.TrimSpace(alias)) != name {
				continue
			}
			key := strings.TrimSpace(mapText(entry, "api_key"))
			if strings.HasPrefix(key, "${") && strings.HasSuffix(key, "}") && len(key) >= 3 {
				if pointer := config.EnvRefVarName(key[2 : len(key)-1]); pointer != "" {
					return aliasKey{keyEnv: pointer}, nil
				}
			}
			if key == "" {
				return aliasKey{keyEnv: strings.TrimSpace(mapText(entry, "key_env"))}, nil
			}
			var value any = loadGatewayConfig(ctx, path)
			source := secretPath{}
			for _, part := range append(append([]string{}, section...), alias, "api_key") {
				source = append(source, part)
				if fields, ok := value.(map[string]any); ok {
					value = fields[part]
				} else {
					value = nil
				}
			}
			if !pyTruthy(value) {
				return aliasKey{}, runtimeRefusal("model_resolution_failed")
			}
			return aliasKey{source: source, literal: value}, nil
		}
	}
	return aliasKey{}, nil
}

// Large context guard facts of a session without a resident agent, read from its persisted usage anchor
func persistedSelectionContext(ctx context.Context, a *SessionAuthority, snapshot any, current switchRoute) (*provider.SelectionContext, error) {
	target, _ := pyItem(snapshot, "target")
	if !pyTruthy(target) {
		var err error
		if target, err = pyItem(snapshot, "receipt", "session_id"); err != nil {
			return nil, err
		}
	}
	sid := pyrepr.Str(target)
	rows, err := a.DB.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{})
	if err != nil {
		return nil, err
	}
	messages := make([]*state.Message, len(rows))
	for i, row := range rows {
		messages[i] = state.MessageFromDict(row)
	}
	tokens, ok := provider.PersistedAnchorTokens(ctx, a.DB, sid, messages, true)
	if !ok || tokens == 0 {
		return nil, nil
	}
	return &provider.SelectionContext{ContextTokens: tokens, CurrentModel: current.Model}, nil
}

// Agent whose context engine measured the last billed prompt
type selectionContextAgent interface {
	Model() string
	ContextLastPromptTokens() int
}

// Resident agent of a live session through the runner's resident or cached agent lookup
func residentAgent(a *SessionAuthority, live *LiveSession) any {
	if live == nil {
		return nil
	}
	if resident, ok := a.Runner.(residentAgentRunner); ok {
		return resident.ResidentAgentFor(live.Route)
	}
	if cached, ok := a.Runner.(cachedAgentRunner); ok {
		return cached.CachedAgentFor(live.Route)
	}
	return nil
}

// Confirmation request of a guarded target bound to the resolved target, replaced selection and revision, nil once confirmed
func selectionGuardRefusal(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, snapshot any, current switchRoute, result provider.ModelSwitchResult) (*pyjson.Object, error) {
	var selection *provider.SelectionContext
	if agent, ok := residentAgent(a, live).(selectionContextAgent); ok && agent != nil {
		selection = provider.SelectionContextFor(agent.ContextLastPromptTokens(), agent.Model())
	}
	if selection == nil {
		var err error
		if selection, err = persistedSelectionContext(ctx, a, snapshot, current); err != nil {
			return nil, err
		}
	}
	baseURL, apiKey := result.BaseURL, result.APIKey
	if baseURL == "" {
		baseURL = current.BaseURL
	}
	if apiKey == "" {
		apiKey = current.APIKey
	}
	warnings := provider.SelectionWarnings(ctx, provider.SelectionRequest{Model: result.NewModel, Provider: result.TargetProvider,
		BaseURL: baseURL, APIKey: apiKey, ModelInfo: result.ModelInfo, Context: selection})
	if len(warnings) == 0 {
		return nil, nil
	}
	kinds := make([]string, len(warnings))
	for i, w := range warnings {
		kinds[i] = w.Kind
	}
	sort.Strings(kinds)
	guards := make([]any, len(kinds))
	for i, kind := range kinds {
		guards[i] = kind
	}
	target, err := pyItem(snapshot, "target")
	if err != nil {
		return nil, err
	}
	revision, err := pyItem(snapshot, "target_revision")
	if err != nil {
		return nil, err
	}
	binding := map[string]any{
		"target":  []any{result.NewModel, result.TargetProvider, provider.NormalizeRouteBaseURL(result.BaseURL), result.APIMode},
		"current": []any{current.Model, current.Provider, provider.NormalizeRouteBaseURL(current.BaseURL)},
		"session": []any{target, revision}, "guards": guards,
	}
	text, err := pyjson.Dumps(binding, pyjson.SortKeys(true))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(text))
	token := hex.EncodeToString(sum[:])
	if confirm, has := payload.Get("confirm"); has && confirm == token {
		return nil, nil
	}
	listed := make([]any, len(warnings))
	for i, w := range warnings {
		listed[i] = pyjson.ObjectOf("kind", w.Kind, "title", w.Title, "message", w.Message)
	}
	return pyjson.ObjectOf("status", "confirmation_required", "confirm_required", true, "confirm", token,
		"target_model", result.NewModel, "target_provider", result.TargetProvider,
		"confirm_message", provider.CombinedSelectionMessage(warnings), "warnings", listed), nil
}
