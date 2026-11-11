package gateway

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Value of a stored row field, nil when the row or the field is absent
func rowField(row *pyjson.Object, key string) any {
	if row == nil {
		return nil
	}
	value, _ := row.Get(key)
	return value
}

// Physical history id resolved to its canonical local session without silently adopting corruption
func resolveLocalTarget(ctx context.Context, a *SessionAuthority, actor Principal, sid string) (SessionRef, error) {
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return SessionRef{}, err
	}
	if row == nil {
		return SessionRef{}, runtimeRefusal("not_found")
	}
	chatID := routeStr(rowField(row, "chat_id"))
	key := chatID
	if key == "" {
		key = sid
	}
	bound := false
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var one int
		scanErr := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key IN (?,?)",
			state.LocalPolicyPrefix+key, state.LocalLegacyPrefix+key).Scan(&one)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		bound = scanErr == nil
		if bound && chatID != "" {
			owner, err := state.LocalLineageOwner(ctx, conn, sid)
			if err != nil {
				return err
			}
			if owner != chatID {
				return runtimeRefusal("storage_unavailable")
			}
		}
		return nil
	})
	if err != nil {
		return SessionRef{}, err
	}
	if bound || strings.HasPrefix(chatID, "local-") {
		ref := SessionRef{ProfileID: a.ProfileID, SessionID: chatID}
		if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
			return SessionRef{}, err
		}
		if bound {
			return restoreLocalSession(ctx, a, ref.SessionID)
		}
		return ref, nil
	}
	return adoptLegacySession(ctx, a, actor, row)
}

// Create params and api mode a legacy row resumes on, its saved route and runtime choices preserved
func legacyLaunchParams(row *pyjson.Object) (map[string]any, any, error) {
	params := map[string]any{}
	for _, key := range []string{"cwd", "model"} {
		if value := rowField(row, key); pyrepr.Truthy(value) {
			params[key] = value
		}
	}
	saved := state.ParseModelConfig(rowField(row, "model_config"))
	route := map[string]any{}
	if runtime := state.SessionGatewayRuntime(row); runtime != nil {
		for _, key := range runtime.Keys {
			value, _ := runtime.Get(key)
			if text, ok := value.(string); ok && pystr.Strip(text) != "" {
				route[key] = pystr.Strip(text)
			}
		}
	}
	if _, has := route["base_url"]; !has {
		if billing, ok := rowField(row, "billing_base_url").(string); ok && pystr.Strip(billing) != "" {
			route["base_url"] = pystr.Strip(billing)
			if _, has := route["provider"]; !has {
				route["provider"] = "custom"
			}
		}
	}
	source, present := row.Get("source")
	if !present {
		return nil, nil, &pyKeyError{"source"}
	}
	if _, hasCwd := params["cwd"]; source == "acp" && !hasCwd && filepath.IsAbs(routeStr(saved["cwd"])) {
		params["cwd"] = saved["cwd"]
	}
	for _, key := range []string{"provider", "base_url"} {
		if value, has := route[key]; has {
			params[key] = value
		}
	}
	reasoning := saved["reasoning_config"]
	if block, ok := reasoning.(map[string]any); ok {
		if block["enabled"] == false {
			reasoning = "none"
		} else {
			reasoning = block["effort"]
		}
	}
	if level, ok := reasoning.(string); ok && config.ParseReasoningEffort(level) != nil {
		params["reasoning"] = level
	}
	if tier, ok := saved["service_tier"].(string); ok {
		if _, err := provider.ParseExactServiceTier(tier); err == nil {
			params["service_tier"] = tier
		}
	}
	return params, route["api_mode"], nil
}

// Main's Bot Chat rule for a stored row, the follow marker or an exact Bot Chat title
func botChatRow(row *pyjson.Object) bool {
	saved := state.ParseModelConfig(rowField(row, "model_config"))
	return saved["follow_profile_config"] == true || pystr.Strip(routeStr(rowField(row, "title"))) == "Bot Chat"
}

// Whether a row's composer pick still holds because the profile targets what it diverged from
func composerPickHolds(row *pyjson.Object, cfg map[string]any) bool {
	composer, ok := state.ParseModelConfig(rowField(row, "model_config"))["composer_override_profile"].(map[string]any)
	if !ok {
		return false
	}
	model, providerName := profileTarget(cfg)
	return pystr.Strip(routeStr(composer["model"])) == model && pystr.Strip(routeStr(composer["provider"])) == providerName
}

// Params and api mode for adoption, a Bot Chat following its profile unless its composer pick holds
func legacyBotChatLaunch(row *pyjson.Object, params map[string]any, apiMode any, cfg map[string]any) (map[string]any, any) {
	if !botChatRow(row) {
		return params, apiMode
	}
	_, hasModel := params["model"]
	if !(hasModel && composerPickHolds(row, cfg)) {
		kept := map[string]any{}
		for _, key := range []string{"cwd", "source"} {
			if value, has := params[key]; has {
				kept[key] = value
			}
		}
		params, apiMode = kept, nil
	}
	followed := make(map[string]any, len(params)+1)
	for key, value := range params {
		followed[key] = value
	}
	followed["follow_profile_config"] = true
	return followed, apiMode
}

// Whether the row's only route record is the custom billing usage stamp
func routeIsUsageStamp(row *pyjson.Object) bool {
	saved := state.ParseModelConfig(rowField(row, "model_config"))
	if runtime, ok := saved["gateway_runtime"].(map[string]any); ok && pyrepr.Truthy(runtime["provider"]) {
		return false
	}
	for _, key := range []string{"provider", "base_url"} {
		if text, ok := saved[key].(string); ok && pystr.Strip(text) != "" {
			return false
		}
	}
	return pystr.Lower(pystr.Strip(routeStr(rowField(row, "billing_provider")))) == "custom"
}

// Configured named entry serving the saved URL, empty when none does
func configuredOwner(ctx context.Context, saved, cfg map[string]any) (string, error) {
	var names []any
	if entries, ok := cfg["providers"].(map[string]any); ok {
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			names = append(names, key)
		}
	}
	if legacy, ok := cfg["custom_providers"].([]any); ok {
		for _, entry := range legacy {
			if fields, isDict := entry.(map[string]any); isDict {
				names = append(names, fields["name"])
			}
		}
	}
	for _, name := range names {
		owner, url, err := ResolvedRoute(ctx, map[string]any{"provider": routeStr(name)}, cfg)
		if err != nil {
			return "", err
		}
		if url == "" {
			continue
		}
		same, err := SameRoute(ctx, saved, map[string]any{"provider": owner, "base_url": url}, cfg)
		if err != nil {
			return "", err
		}
		if same {
			return owner, nil
		}
	}
	return "", nil
}

// Provider whose credential may serve the saved endpoint, refusing by name rather than leak a moved entry's key
func endpointOwner(ctx context.Context, saved, cfg map[string]any) (any, error) {
	identity, entryURL, err := ResolvedRoute(ctx, map[string]any{"provider": saved["provider"]}, cfg)
	if err != nil {
		return nil, err
	}
	if entryURL == "" {
		return saved["provider"], nil
	}
	same, err := SameRoute(ctx, saved, map[string]any{"provider": identity, "base_url": entryURL}, cfg)
	if err != nil {
		return nil, err
	}
	if same {
		return saved["provider"], nil
	}
	owner, err := configuredOwner(ctx, saved, cfg)
	if err != nil {
		return nil, err
	}
	if owner != "" {
		return owner, nil
	}
	field, detail := "provider", identity+" no longer serves the saved endpoint; select a model to continue"
	return nil, state.NewRuntimeStoreError("launch_credentials_unavailable", &field, &detail)
}

// Adopts a legacy local row as a canonical local session committed under the liveness guard
func adoptLegacySession(ctx context.Context, a *SessionAuthority, actor Principal, row *pyjson.Object) (SessionRef, error) {
	if actor.ProfileID != a.ProfileID {
		return SessionRef{}, runtimeRefusal("profile_mismatch")
	}
	a.mu.Lock()
	transport, known := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	if !known || !samePrincipal(transport, actor) || !actor.Capabilities["session:read"] || !actor.Capabilities["session:create"] {
		return SessionRef{}, runtimeRefusal("not_found")
	}
	if err := a.DB.ValidateLegacyRow(row, actor.Subject); err != nil {
		return SessionRef{}, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return SessionRef{}, err
	}
	sid := routeStr(rowField(row, "id"))
	requestID := "legacy:" + sid
	params, apiMode, err := legacyLaunchParams(row)
	if err != nil {
		return SessionRef{}, err
	}
	if params["source"], err = state.LegacyCreationSource(row); err != nil {
		return SessionRef{}, err
	}
	private := &RecoveredSecrets{}
	cfg := loadGatewayConfig(ctx, "")
	params, apiMode = legacyBotChatLaunch(row, params, apiMode, cfg)
	if modelCfg, ok := cfg["model"].(map[string]any); ok && (hasKey(params, "provider") || hasKey(params, "base_url")) {
		savedProvider, has := params["provider"]
		if !has {
			savedProvider = modelCfg["provider"]
		}
		saved := map[string]any{"provider": savedProvider, "base_url": params["base_url"]}
		profileID, _, err := ResolvedRoute(ctx, modelCfg, cfg)
		if err != nil {
			return SessionRef{}, err
		}
		savedID, _, err := ResolvedRoute(ctx, saved, cfg)
		if err != nil {
			return SessionRef{}, err
		}
		same := false
		if profileID != "auto" && savedID != "auto" {
			if same, err = SameRoute(ctx, saved, modelCfg, cfg); err != nil {
				return SessionRef{}, err
			}
		}
		owner := ""
		bareCustom := false
		if !same && routeIsUsageStamp(row) && pyrepr.Truthy(saved["base_url"]) && savedID == "custom" {
			if owner, err = configuredOwner(ctx, saved, cfg); err != nil {
				return SessionRef{}, err
			}
			bareCustom = owner == ""
		}
		switch {
		case same || bareCustom:
			delete(params, "provider")
			delete(params, "base_url")
		default:
			kept := map[string]any{}
			for key, value := range modelCfg {
				if key == "provider" || !containsString(RouteBoundKeys, key) {
					kept[key] = value
				}
			}
			cfg["model"] = kept
			if pyrepr.Truthy(saved["base_url"]) {
				if params["provider"], err = endpointOwner(ctx, saved, cfg); err != nil {
					return SessionRef{}, err
				}
			}
		}
	}
	policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		return SessionRef{}, err
	}
	if pyrepr.Truthy(apiMode) {
		frozen, err := loadsObject(policy.ConfigJSON)
		if err != nil {
			return SessionRef{}, err
		}
		raw, present := frozen.Get("model")
		if !present {
			raw = pyjson.NewObject()
			frozen.Set("model", raw)
		}
		model, ok := raw.(*pyjson.Object)
		if !ok {
			return SessionRef{}, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(raw) + "' object does not support item assignment"}
		}
		model.Set("api_mode", apiMode)
		text, err := pyjson.Dumps(frozen)
		if err != nil {
			return SessionRef{}, err
		}
		replaced := *policy
		replaced.ConfigJSON = text
		policy = &replaced
	}
	named := ""
	if policy.Model != nil {
		named = pystr.Strip(*policy.Model)
	}
	if named == "" {
		named = pystr.Strip(resolveGatewayModel(ctx, policy.frozenConfig()))
	}
	if named == "" {
		named = pystr.Strip(routeStr(rowField(row, "model")))
	}
	if named == "" {
		field, detail := "model", "the profile names no model and the saved session has none"
		return SessionRef{}, state.NewRuntimeStoreError("invalid_params", &field, &detail)
	}
	if policy.Model == nil || *policy.Model != named {
		replaced := *policy
		replaced.Model = &named
		policy = &replaced
	}
	if policy, err = bindLaunchKey(ctx, a, sid, policy, nil, private); err != nil {
		return SessionRef{}, err
	}
	source, err := localSource(ctx, a, sid, actor.Subject)
	if err != nil {
		return SessionRef{}, err
	}
	route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
	if err != nil {
		return SessionRef{}, err
	}
	started, ok := pyFloatOf(rowField(row, "started_at"))
	if !ok {
		return SessionRef{}, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(rowField(row, "started_at")) + "' object cannot be interpreted as a timestamp"}
	}
	now := pyDatetime{T: time.Unix(0, int64(started*1e9))}
	entry := NewSessionEntry(route, sid, now, now)
	entry.Origin, entry.Platform = source, PlatformLocal
	receipt := pyjson.ObjectOf("profile_id", a.ProfileID, "principal_id", actor.Subject, "request_id", requestID,
		"session_id", sid, "route", route, "entry", entry.ToDict(), "policy", localPolicyAsDict(policy),
		"legacy_session_id", sid, "lineage", []any{sid})
	registryHome := filepath.Dir(a.DB.Path())
	err = activesessions.LivenessGuard(ctx, sid, registryHome, nil, func(active bool) error {
		if active {
			return runtimeRefusal("runtime_coordination_required")
		}
		_, err := a.DB.CommitLocalSession(ctx, a.Epoch, state.LocalCreateReceipt{Receipt: receipt})
		return err
	})
	var registryErr *activesessions.RegistryError
	if errors.As(err, &registryErr) {
		return SessionRef{}, runtimeRefusal("runtime_coordination_required")
	}
	if err != nil {
		return SessionRef{}, err
	}
	return restoreLocalSession(ctx, a, sid)
}

// Whether a params mapping holds key
func hasKey(params map[string]any, key string) bool {
	_, has := params[key]
	return has
}
