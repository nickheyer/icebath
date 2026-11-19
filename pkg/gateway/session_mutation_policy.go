package gateway

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Value under the key path of nested dicts, a KeyError when a key is absent
func pyItem(v any, keys ...string) (any, error) {
	for _, key := range keys {
		var found bool
		switch m := v.(type) {
		case *pyjson.Object:
			v, found = m.Get(key)
		case map[string]any:
			v, found = m[key]
		}
		if !found {
			return nil, &pyKeyError{key}
		}
	}
	return v, nil
}

// Frozen policy of the receipt a prepared snapshot carries
func preparedPolicy(prepared *pyjson.Object) (*LocalSessionPolicy, error) {
	raw, err := pyItem(prepared, "snapshot", "receipt", "policy")
	if err != nil {
		return nil, err
	}
	return restorePolicy(raw)
}

// Session id of the receipt a prepared snapshot carries
func preparedSessionID(prepared *pyjson.Object) (string, error) {
	raw, err := pyItem(prepared, "snapshot", "receipt", "session_id")
	if err != nil {
		return "", err
	}
	return pyrepr.Str(raw), nil
}

// Copy of a prepared result with extra fields set over it
func preparedWith(prepared *pyjson.Object, pairs ...any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, key := range prepared.Keys {
		value, _ := prepared.Get(key)
		out.Set(key, value)
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		out.Set(pairs[i].(string), pairs[i+1])
	}
	return out
}

// Prepared result with the policy rebound to the old launch key and config secrets, extra fields beside it
func rebindPrepared(ctx context.Context, a *SessionAuthority, prepared *pyjson.Object, old, policy *LocalSessionPolicy, extra ...any) (*pyjson.Object, error) {
	secrets := &RecoveredSecrets{}
	if old.ConfigSecretRef != nil {
		recovered, err := recoverConfigSecrets(ctx, a, old)
		if err != nil {
			return nil, err
		}
		secrets = recovered
	}
	key, err := launchKey(a, old)
	if err != nil {
		return nil, err
	}
	sid, err := preparedSessionID(prepared)
	if err != nil {
		return nil, err
	}
	bound, err := bindLaunchKey(ctx, a, sid, policy, key, secrets)
	if err != nil {
		return nil, err
	}
	return preparedWith(prepared, append([]any{"policy", bound.asDict()}, extra...)...), nil
}

// Session scope reasoning level frozen into the policy, a nil effort giving the level back to the profile
func prepareReasoning(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	frozen, err := old.config(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	effort, err := pyItem(payload, "effort")
	if err != nil {
		return nil, err
	}
	if effort != nil {
		if err := pinReasoningEffort(frozen, effort, true); err != nil {
			return nil, err
		}
	} else {
		profile := map[string]any{}
		if !old.IgnoreUserConfig {
			if profile, err = profileConfig(ctx, a); err != nil {
				return nil, err
			}
		}
		if err := unpinReasoning(frozen, profile); err != nil {
			return nil, err
		}
	}
	text, err := pyjson.Dumps(frozen)
	if err != nil {
		return nil, err
	}
	policy := *old
	policy.ConfigJSON = text
	return rebindPrepared(ctx, a, prepared, old, &policy)
}

// Moves the execution cwd and TERMINAL_CWD together, recording the new checkout's branch and common root
func prepareWorkspace(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	raw, err := pyItem(payload, "cwd")
	if err != nil {
		return nil, err
	}
	cwd, err := pypath.Abspath(pypath.Expanduser(home.TranslateCwdForWSLBackend(pyrepr.Str(raw))))
	if err != nil {
		return nil, err
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return nil, state.NewRuntimeStoreError("invalid_params", nil, nil)
	}
	branch, root := tuihost.Branch(cwd), tuihost.CommonRepoRoot(cwd)
	loaded, err := pyjson.Loads(old.TerminalJSON)
	if err != nil {
		return nil, err
	}
	ordered, ok := loaded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "the workspace policy is " + errchain.Kind(loaded) + ", expected a mapping"}
	}
	ordered.Set("TERMINAL_CWD", cwd)
	text, err := pyjson.Dumps(ordered)
	if err != nil {
		return nil, err
	}
	policy := *old
	policy.Cwd, policy.TerminalJSON = cwd, text
	return rebindPrepared(ctx, a, prepared, old, &policy, "git", map[string]any{"branch": branch, "root": root})
}

// Freezes an ACP session's edit approval policy, a noop when the policy is already selected
func prepareEditPolicy(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	if old.Source != "acp" {
		field, detail := "payload.policy", "only an ACP session has an editor edit approval policy"
		return nil, state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	picked, err := pyItem(payload, "policy")
	if err != nil {
		return nil, err
	}
	if pyjson.Equal(old.EditApprovalPolicy, picked) {
		return pyjson.ObjectOf("status", "noop", "edit_approval_policy", old.EditApprovalPolicy), nil
	}
	policy := *old
	policy.EditApprovalPolicy = pyrepr.Str(picked)
	return rebindPrepared(ctx, a, prepared, old, &policy)
}

// Prepare step of a policy only mutation
type policyPreparer func(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error)

// Prepare steps of the policy only operations
var policyPreparers = map[string]policyPreparer{"reasoning": prepareReasoning, "workspace": prepareWorkspace, "edit_policy": prepareEditPolicy,
	"launch": prepareLaunch}

// Launch flags that change which endpoint the session's turns reach
var launchRouteFields = []string{"model", "provider", "base_url"}

// Lays a resume's launch flags over the frozen policy, a newly set bypass flag rebuilding it from code defaults
func prepareLaunch(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	sid, err := preparedSessionID(prepared)
	if err != nil {
		return nil, err
	}
	overrides, _ := pyjson.Plain(payload).(map[string]any)
	cwd := old.Cwd
	if raw, moved := overrides["cwd"]; moved {
		if cwd, err = pypath.Abspath(pypath.Expanduser(home.TranslateCwdForWSLBackend(pyrepr.Str(raw)))); err != nil {
			return nil, err
		}
		if !isDir(cwd) {
			return nil, refuseText("cwd", "must be an existing directory")
		}
	}
	request := policyObject(old.RequestJSON)
	routed := false
	for _, key := range launchRouteFields {
		if _, has := overrides[key]; has {
			routed = true
		}
	}
	if routed {
		delete(request, "model_requested")
		delete(request, "route_derived")
	}
	for key, value := range overrides {
		if key != "cwd" {
			request[key] = value
		}
	}
	if request["safe_mode"] == true || request["ignore_user_config"] == true {
		if !old.SafeMode && !old.IgnoreUserConfig {
			_, moved := overrides["cwd"]
			return relaunchBypass(ctx, a, prepared, old, sid, request, cwd, moved)
		}
	}
	frozen, err := old.config(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	policy := *old
	launch := map[string]any{}
	for key, value := range overrides {
		if key != "cwd" {
			launch[key] = value
		}
	}
	var key *string
	if routed {
		resolved, changed, err := resolveLaunchRoute(profileEnvScope(ctx, ""), launch, frozen)
		if err != nil {
			return nil, err
		}
		if changed {
			request["model_requested"] = launch["model"]
			derived := []any{}
			for _, field := range []string{"base_url", "provider"} {
				_, now := resolved[field]
				_, before := launch[field]
				if now && !before {
					derived = append(derived, field)
				}
			}
			request["route_derived"] = derived
			for _, field := range launchRouteFields {
				if value, has := resolved[field]; has {
					request[field] = value
				}
			}
		}
		if routedKey, has := resolved["api_key"].(string); has && routedKey != "" {
			key = &routedKey
		}
		delete(resolved, "api_key")
		launch = resolved
	}
	if err := applyLaunchOverrides(ctx, launch, frozen); err != nil {
		return nil, err
	}
	if model, has := launch["model"].(string); has {
		policy.Model = &model
	}
	if _, has := launch["toolsets"]; has {
		enabled, err := resolveToolsets(ctx, request, frozen, old.Source, old.SafeMode, old.CodingPosture, old.IgnoreUserConfig)
		if err != nil {
			return nil, err
		}
		policy.Toolsets = sortedNames(enabled)
	}
	if _, has := launch["skills"]; has {
		if policy.SkillsPrompt, err = renderLaunchSkills(ctx, request, sid); err != nil {
			return nil, err
		}
	}
	if policy.ConfigJSON, err = pyjson.Dumps(frozen); err != nil {
		return nil, err
	}
	if policy.RequestJSON, err = pyjson.Dumps(request, pyjson.SortKeys(true)); err != nil {
		return nil, err
	}
	secrets := &RecoveredSecrets{}
	if old.ConfigSecretRef != nil {
		recovered, err := recoverConfigSecrets(ctx, a, old)
		if err != nil {
			return nil, err
		}
		block, _ := frozen["model"].(map[string]any)
		for i, path := range recovered.paths {
			if len(path) == 2 && path[0] == "model" {
				if _, kept := block[pyrepr.Str(path[1])]; !kept {
					continue
				}
			}
			secrets.Set(path, recovered.values[i])
		}
	}
	_, newProvider := launch["provider"]
	_, newBase := launch["base_url"]
	if key == nil && !newProvider && !newBase {
		if key, err = launchKey(a, old); err != nil {
			return nil, err
		}
	}
	_, moved := overrides["cwd"]
	return bindRelaunched(ctx, a, prepared, &policy, sid, cwd, moved, key, secrets)
}

// Policy of a resume that newly asks for a bypass launch, built from code defaults with the session's own bindings kept
func relaunchBypass(ctx context.Context, a *SessionAuthority, prepared *pyjson.Object, old *LocalSessionPolicy, sid string, request map[string]any,
	cwd string, moved bool) (*pyjson.Object, error) {
	params := map[string]any{}
	for key, value := range request {
		if CreateFields[key] && key != "request_id" && key != "skills" {
			params[key] = value
		}
	}
	if _, has := params["model"]; !has && old.Model != nil {
		params["model"] = *old.Model
	}
	for field, value := range map[string]any{"provider": old.Provider(), "base_url": old.BaseURL()} {
		if text, ok := value.(string); ok && text != "" {
			if _, has := params[field]; !has {
				params[field] = text
			}
		}
	}
	params["source"], params["cwd"] = old.Source, cwd
	secrets := &RecoveredSecrets{}
	built, err := bypassPolicy(ctx, params, secrets, !old.ClientEnvRemote)
	if err != nil {
		return nil, err
	}
	key, err := launchKey(a, old)
	if err != nil {
		return nil, err
	}
	policy := *built
	policy.Source, policy.Platform = old.Source, old.Platform
	policy.EditorMCPJSON, policy.KanbanJSON, policy.EditApprovalPolicy = old.EditorMCPJSON, old.KanbanJSON, old.EditApprovalPolicy
	policy.ClientEnvJSON, policy.ClientEnvRemote, policy.ClientEnvRulesJSON = old.ClientEnvJSON, old.ClientEnvRemote, old.ClientEnvRulesJSON
	return bindRelaunched(ctx, a, prepared, &policy, sid, cwd, moved, key, secrets)
}

// Prepared result of a relaunched policy bound to its key and secrets, the moved cwd with its git identity
func bindRelaunched(ctx context.Context, a *SessionAuthority, prepared *pyjson.Object, policy *LocalSessionPolicy, sid, cwd string,
	moved bool, key *string, secrets *RecoveredSecrets) (*pyjson.Object, error) {
	extra := []any{}
	if moved {
		loaded, err := pyjson.Loads(policy.TerminalJSON)
		if err != nil {
			return nil, err
		}
		ordered, ok := loaded.(*pyjson.Object)
		if !ok {
			return nil, &PyError{Kind: "TypeError", Msg: "the workspace policy is " + errchain.Kind(loaded) + ", expected a mapping"}
		}
		ordered.Set("TERMINAL_CWD", cwd)
		if policy.TerminalJSON, err = pyjson.Dumps(ordered); err != nil {
			return nil, err
		}
		policy.Cwd = cwd
		extra = append(extra, "git", map[string]any{"branch": tuihost.Branch(cwd), "root": tuihost.CommonRepoRoot(cwd)})
	}
	bound, err := bindLaunchKey(ctx, a, sid, policy, key, secrets)
	if err != nil {
		return nil, err
	}
	return preparedWith(prepared, append([]any{"policy", bound.asDict()}, extra...)...), nil
}
