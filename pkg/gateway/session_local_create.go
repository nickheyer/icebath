package gateway

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Trusted inputs and client proofs of one local session creation
type localCreateOptions struct {
	TrustedPolicy  *LocalSessionPolicy
	TrustedSecrets *RecoveredSecrets
	SkillsPrompt   *string
	ClientLocal    bool
	DesktopLocal   bool
}

// Frozen snapshot of a bypass launch, code defaults plus the explicit launch options and never the profile
func bypassPolicy(ctx context.Context, params map[string]any, privateSecrets *RecoveredSecrets, clientLocal bool) (*LocalSessionPolicy, error) {
	if !pyTruthy(params["model"]) {
		return nil, invalidParams("model", "a bypass launch has no profile to inherit a model from")
	}
	defaults := map[string]any(config.Defaults())
	switch defaults["model"].(type) {
	case map[string]any, config.Tree:
	default:
		defaults["model"] = map[string]any{}
	}
	return buildPolicy(ctx, params, defaults, buildPolicyOptions{PrivateSecrets: privateSecrets, DefaultTerminal: true, ClientLocal: clientLocal})
}

// Effective config.yaml of a profile for a new session's policy, its variables expanded against the profile's current env file
func loadProfileConfig(ctx context.Context, profileHome string) map[string]any {
	path := ""
	if profileHome != "" {
		path = filepath.Join(profileHome, "config.yaml")
	}
	return loadGatewayConfig(profileEnvScope(ctx, profileHome), path)
}

// Context bound to the profile's current env file unless a secret scope is already bound
func profileEnvScope(ctx context.Context, profileHome string) context.Context {
	if provider.SecretScopeBound(ctx) {
		return ctx
	}
	if profileHome == "" {
		profileHome = home.Dir(ctx)
	}
	return provider.WithSecretScope(ctx, provider.BuildProfileSecretScope(profileHome), profileHome)
}

// Notice for a session whose proven local client opted into project plugins in a folder that has some
func projectPluginsWarning(policy *LocalSessionPolicy) *string {
	if policy == nil || policy.ClientEnvRemote {
		return nil
	}
	env := policy.ClientEnv()
	if len(env) == 0 || !provider.IsTruthyValue(env["ICEBATH_ENABLE_PROJECT_PLUGINS"]) {
		return nil
	}
	folder := filepath.Join(policy.Cwd, ".icebath", "plugins")
	if !isDir(folder) {
		return nil
	}
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) == 0 {
		return nil
	}
	text := "project plugins in " + folder + " are not loaded: the gateway that runs Hermes sessions does not " +
		"load ./.icebath/plugins, so ICEBATH_ENABLE_PROJECT_PLUGINS has no effect here."
	return &text
}

// Stages, commits and publishes a local session inline, an exact retry answering with the existing session
func createLocalSession(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any, opts localCreateOptions) (SessionRef, error) {
	opts.DesktopLocal = false
	ref, staged, err := stageLocalSession(ctx, a, actor, params, opts)
	if err != nil {
		return SessionRef{}, err
	}
	if staged == nil {
		return *ref, nil
	}
	if _, err := a.DB.CommitLocalSession(ctx, a.Epoch, *staged); err != nil {
		return SessionRef{}, err
	}
	sid, err := receiptField(staged.Receipt, "session_id")
	if err != nil {
		return SessionRef{}, err
	}
	return restoreLocalSession(ctx, a, pyStrOf(sid))
}

// True unless an older build committed the request id's session without following the profile
func stagedCreationFollows(ctx context.Context, a *SessionAuthority, actor Principal, requestID any) (bool, error) {
	text, ok := requestID.(string)
	if !ok || text == "" {
		return true, nil
	}
	sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, text)
	if err != nil {
		return false, err
	}
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return false, err
	}
	if row == nil {
		return true, nil
	}
	request, err := func() (any, error) {
		receipt, err := a.DB.LocalReceipt(ctx, sid)
		if err != nil {
			return nil, err
		}
		rawPolicy, err := receiptField(receipt, "policy")
		if err != nil {
			return nil, err
		}
		policy, isObject := rawPolicy.(*pyjson.Object)
		if !isObject {
			return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(rawPolicy) + "' object is not subscriptable"}
		}
		rawRequest, err := receiptField(policy, "request_json")
		if err != nil {
			return nil, err
		}
		requestJSON, isText := rawRequest.(string)
		if !isText {
			return nil, &PyError{Kind: "TypeError", Msg: "the JSON object must be str, bytes or bytearray, not " + jsonValueTypeName(rawRequest)}
		}
		return pyjson.Loads(requestJSON)
	}()
	if err != nil {
		if receiptCheckFailure(err) && checkErrorClass(err) != "AttributeError" {
			return true, nil
		}
		return false, err
	}
	object, isObject := request.(*pyjson.Object)
	if !isObject {
		return false, nil
	}
	follows, _ := object.Get("follow_profile_config")
	return follows == any(true), nil
}

// Request params as an ordered object, keys sorted
func paramsObject(params map[string]any) *pyjson.Object {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := pyjson.NewObject()
	for _, key := range keys {
		out.Set(key, params[key])
	}
	return out
}

// Everything before the creation commit, the existing session's ref on an exact retry else the receipt to commit
func stageLocalSession(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any, opts localCreateOptions) (*SessionRef, *state.LocalCreateReceipt, error) {
	if actor.ProfileID != a.ProfileID {
		return nil, nil, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:create"] {
		return nil, nil, runtimeRefusal("permission_denied")
	}
	privateSecrets := &RecoveredSecrets{}
	if opts.TrustedSecrets != nil {
		privateSecrets = opts.TrustedSecrets.without(nil)
	}
	clientKey := params["api_key"]
	var policy *LocalSessionPolicy
	bypass := false
	for _, name := range BypassFields {
		bypass = bypass || params[name] == any(true)
	}
	switch {
	case opts.TrustedPolicy != nil:
		policy = opts.TrustedPolicy
	case bypass:
		built, err := bypassPolicy(ctx, params, privateSecrets, opts.ClientLocal)
		if err != nil {
			return nil, nil, err
		}
		policy = built
	default:
		rawModel, sent := params["model"], params
		scoped := profileEnvScope(ctx, "")
		cfg := loadProfileConfig(scoped, "")
		resolved, changed, err := resolveLaunchRoute(scoped, params, cfg)
		if err != nil {
			return nil, nil, err
		}
		params = resolved
		built, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{PrivateSecrets: privateSecrets, WorkspacePosture: true, ClientLocal: opts.ClientLocal})
		if err != nil {
			return nil, nil, err
		}
		policy = built
		if changed {
			derived := []any{}
			for _, key := range []string{"base_url", "provider"} {
				_, now := params[key]
				_, before := sent[key]
				if now && !before {
					derived = append(derived, key)
				}
			}
			request, err := loadsObject(policy.RequestJSON)
			if err != nil {
				return nil, nil, err
			}
			request.Set("model_requested", rawModel)
			request.Set("route_derived", derived)
			text, err := pyjson.Dumps(request, pyjson.SortKeys(true))
			if err != nil {
				return nil, nil, err
			}
			replaced := *policy
			replaced.RequestJSON = text
			policy = &replaced
		}
		marks := pyjson.NewObject()
		if opts.DesktopLocal && policy.Source == "gui" {
			marks.Set(LocalDesktopChat, true)
		}
		if policy.Model == nil {
			effective, err := policy.config(ctx, nil, true)
			if err != nil {
				return nil, nil, err
			}
			replaced := *policy
			if model := resolveGatewayModel(ctx, effective); model != "" {
				replaced.Model = &model
			}
			policy = &replaced
			if policy.Model != nil && *policy.Model == provider.GuestModel {
				marks.Set(FollowAdopted, true)
			}
		}
		if marks.Len() > 0 {
			frozen, err := loadsObject(policy.ConfigJSON)
			if err != nil {
				return nil, nil, err
			}
			for _, key := range marks.Keys {
				value, _ := marks.Get(key)
				frozen.Set(key, value)
			}
			text, err := pyjson.Dumps(frozen)
			if err != nil {
				return nil, nil, err
			}
			replaced := *policy
			replaced.ConfigJSON = text
			policy = &replaced
		}
	}
	var requestID string
	if raw, has := params["request_id"]; has {
		text, isText := raw.(string)
		if !isText || text == "" || pystr.Len(text) > 256 {
			return nil, nil, invalidParams("request_id", "must be a non-empty string of at most 256 characters")
		}
		requestID = text
	} else {
		requestID = uuid4HexString()
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, nil, err
	}
	sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID)
	if err != nil {
		return nil, nil, err
	}
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return nil, nil, err
	}
	if row != nil {
		saved, err := a.DB.LocalReceipt(ctx, sid)
		if err != nil {
			return nil, nil, err
		}
		rawPolicy, err := receiptField(saved, "policy")
		if err != nil {
			return nil, nil, err
		}
		original, err := restorePolicy(rawPolicy)
		if err != nil {
			return nil, nil, err
		}
		savedPolicy, isObject := rawPolicy.(*pyjson.Object)
		if !isObject {
			return nil, nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(rawPolicy) + "' object is not subscriptable"}
		}
		same, err := state.SameCreateRetry(savedPolicy, policy.RequestJSON, paramsObject(params))
		if err != nil {
			return nil, nil, err
		}
		if !same {
			return nil, nil, invalidParams("request_id", "a resumed request_id must carry the same frozen request")
		}
		frozen, err := loadsObject(original.RequestJSON)
		if err != nil {
			return nil, nil, err
		}
		if _, resolved := frozen.Get("model_requested"); resolved {
			if clientKey != nil {
				return nil, nil, runtimeRefusal("admission_conflict")
			}
		} else if original.CredentialRef != nil || params["api_key"] != nil {
			key, err := launchKey(a, original)
			if err != nil {
				return nil, nil, err
			}
			offered := ""
			if raw := params["api_key"]; pyTruthy(raw) {
				text, isText := raw.(string)
				if !isText {
					return nil, nil, &PyError{Kind: "TypeError", Msg: "unsupported operand types(s) or combination of types: 'str' and '" + jsonValueTypeName(raw) + "'"}
				}
				offered = text
			}
			if key == nil || subtle.ConstantTimeCompare([]byte(*key), []byte(offered)) != 1 {
				return nil, nil, runtimeRefusal("admission_conflict")
			}
		}
		ref, err := restoreLocalSession(ctx, a, sid)
		if err != nil {
			return nil, nil, err
		}
		return &ref, nil, nil
	}
	skills, err := launchSkills(params)
	if err != nil {
		return nil, nil, err
	}
	if len(skills) > 0 && opts.SkillsPrompt == nil {
		return nil, nil, invalidParams("skills", "the caller must render -s skills before minting the session")
	}
	withSkills := *policy
	withSkills.SkillsPrompt = opts.SkillsPrompt
	policy = &withSkills
	if warning := projectPluginsWarning(policy); warning != nil {
		slog.Warn(fmt.Sprintf("session %s: %s", sid, *warning))
	}
	clientRequest := pyjson.NewObject()
	if env, has := params["client_env"]; has {
		clientRequest.Set("client_env", env)
	}
	var apiKey *string
	if text, isText := params["api_key"].(string); isText {
		apiKey = &text
	}
	policy, err = bindLaunchKeyFor(ctx, a, sid, policy, apiKey, privateSecrets, clientKey == nil, clientRequest)
	if err != nil {
		return nil, nil, err
	}
	source, err := localSource(ctx, a, sid, actor.Subject)
	if err != nil {
		return nil, nil, err
	}
	route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
	if err != nil {
		return nil, nil, err
	}
	now := sessionNow()
	entry := NewSessionEntry(route, sid, now, now)
	entry.Origin, entry.Platform = source, PlatformLocal
	staged, err := stageSessionPrefill(ctx, a, policy, pyjson.ObjectOf(
		"profile_id", a.ProfileID, "principal_id", actor.Subject, "request_id", requestID,
		"session_id", sid, "route", route, "entry", entry.ToDict(), "policy", localPolicyAsDict(policy)))
	if err != nil {
		return nil, nil, err
	}
	staged.ClientRequest = clientRequest
	return nil, staged, nil
}

// Republishes the stored policy of a live local session onto its adapter, evicting the cached agent when asked
func publishLocalPolicy(ctx context.Context, a *SessionAuthority, sessionID string, evict bool) error {
	live, ok := a.live(sessionID)
	if !ok {
		return &pyKeyError{key: sessionID}
	}
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return err
	}
	adapter, held := adapters.Get(PlatformLocal)
	if !held {
		return &pyKeyError{key: string(PlatformLocal)}
	}
	local, isLocal := adapter.(*LocalSessionAdapter)
	if !isLocal {
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute 'policies'", adapter)}
	}
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		return err
	}
	rawPolicy, err := receiptField(receipt, "policy")
	if err != nil {
		return err
	}
	policy, err := restorePolicy(rawPolicy)
	if err != nil {
		return err
	}
	if live.Source == nil {
		return &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'chat_id'"}
	}
	local.setPolicy(live.Source.ChatID, policy)
	if !evict {
		return nil
	}
	evicting, can := a.Runner.(agentEvictingRunner)
	if !can {
		return &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_evict_cached_agent'", a.Runner)}
	}
	evicting.EvictCachedAgent(live.Route)
	return nil
}
