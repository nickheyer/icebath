package gateway

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
	branch, root := tuigateway.Branch(cwd), tuigateway.CommonRepoRoot(cwd)
	loaded, err := pyjson.Loads(old.TerminalJSON)
	if err != nil {
		return nil, err
	}
	ordered, ok := loaded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(loaded) + "' object does not support item assignment"}
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
var policyPreparers = map[string]policyPreparer{"reasoning": prepareReasoning, "workspace": prepareWorkspace, "edit_policy": prepareEditPolicy}
