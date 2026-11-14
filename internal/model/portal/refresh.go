package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

var oauthGrantDeadCodes = []string{"invalid_grant", "invalid_token", "refresh_token_reused"}

const refreshReuseDescription = "Icebath Portal detected refresh-token reuse and revoked this session.\n" +
	"This usually means an external process (monitoring script, " +
	"custom self-heal hook, or another icebath install sharing " +
	"~/.icebath/auth.json) called POST /api/oauth/token with icebath's " +
	"refresh token without persisting the rotated token back.\n" +
	"Icebath Portal refresh tokens are single-use — only icebath may call the " +
	"refresh endpoint. For health checks, use `icebath auth status` " +
	"instead.\n" +
	"Re-authenticate with: icebath auth add portal"

func (a *Account) verifiedClient(ctx context.Context, insecure *bool, caBundle string, state *object) (*http.Client, auth.TLSState, error) {
	stored := auth.TLSState{}
	if state != nil {
		stored = auth.TLSStateFrom(state.raw())
	}
	return a.store.VerifiedClient(a.bind(ctx), insecure, caBundle, stored)
}

func tlsObject(t auth.TLSState) *object {
	return objectOrEmpty(auth.TLSStateJSON(t))
}

func refreshAccessToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, refreshToken string, timeout time.Duration) (*object, error) {
	form := "grant_type=refresh_token&client_id=" + url.QueryEscape(clientID)
	r, err := exchange(ctx, client, http.MethodPost, portalBaseURL+"/api/oauth/token",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "x-portal-refresh-token": refreshToken}, []byte(form), timeout)
	if err != nil {
		return nil, err
	}
	if r.status == 200 {
		parsed, err := parseValue(r.body)
		if err != nil {
			return nil, fmt.Errorf("portal: token endpoint answered 200 with an unreadable body: %w", err)
		}
		payload, _ := parsed.(*object)
		if payload == nil {
			if items, ok := parsed.([]any); ok && !slices.Contains(items, any("access_token")) {
				return nil, portalErr("Refresh response missing access_token", "invalid_token", true)
			}
			return nil, errors.New("portal: token endpoint answered 200 without a JSON object")
		}
		if !payload.has("access_token") {
			return nil, portalErr("Refresh response missing access_token", "invalid_token", true)
		}
		return payload, nil
	}
	if r.status >= 500 && r.status <= 599 {
		return nil, &auth.Error{Message: fmt.Sprintf("Icebath Portal is temporarily unavailable (HTTP %d).", r.status),
			Provider: Provider, Code: "temporarily_unavailable", Retryable: boolPtr(true)}
	}
	if r.status == 403 || r.status == 429 {
		if mitigated := r.header.Get("x-vercel-mitigated"); mitigated != "" {
			wait, _ := httpclient.RetryAfter(r.header, time.Now())
			return nil, &auth.Error{Message: fmt.Sprintf("Icebath Portal's edge firewall challenged the token refresh (HTTP %d, "+
				"x-vercel-mitigated=%s). Credentials kept; try again shortly.", r.status, mitigated),
				Provider: Provider, Code: "upstream_blocked", Retryable: boolPtr(true), RetryAfter: wait}
		}
	}
	payload := r.object()
	rawCode := payload.get("error")
	if rawCode == nil && (r.status == 401 || r.status == 403) {
		rawCode = "invalid_grant"
	}
	code := ""
	if rawCode != nil {
		code = textual(rawCode)
	}
	description := "Refresh token exchange failed"
	if v := payload.get("error_description"); truthy(v) {
		description = textual(v)
	}
	relogin := rawCode != nil && slices.Contains(oauthGrantDeadCodes, code)
	if code == "refresh_token_reused" || strings.Contains(strings.ToLower(description), "reuse") {
		description, relogin = refreshReuseDescription, true
	}
	return nil, portalErr(description, code, relogin)
}

func (a *Account) quarantineForensics(state *object, cause *auth.Error, reason string) map[string]any {
	forensic := map[string]any{
		"reason": reason, "error_code": nil, "client_id": plainValue(state.get("client_id")),
		"agent_key_id": plainValue(state.get("agent_key_id")), "refresh_token_fp": tokenFingerprint(state.get("refresh_token")),
	}
	if cause.Code != "" {
		forensic["error_code"] = cause.Code
	}
	authPath := a.store.Path()
	forensic["auth_json_path"] = authPath
	if fi, err := os.Stat(authPath); err == nil {
		forensic["auth_json_size"] = fi.Size()
		forensic["auth_json_mtime"] = epochOf(fi.ModTime())
		forensic["auth_json_exists"] = true
	} else if errors.Is(err, fs.ErrNotExist) {
		forensic["auth_json_exists"] = false
	} else {
		forensic["auth_json_stat_error"] = err.Error()
	}
	var alreadyExpired any
	if raw := state.str("expires_at"); raw != "" {
		if when, ok := parseISOAssumingUTC(raw); ok {
			alreadyExpired = when.Before(time.Now())
		}
	}
	forensic["token_already_expired"] = alreadyExpired
	return forensic
}

func parseISOAssumingUTC(raw string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return auth.ParseISOTimestamp(raw)
}

func (a *Account) quarantineState(ctx context.Context, state *object, cause *auth.Error, reason string) {
	slog.Warn("Icebath Portal OAuth state quarantined (terminal auth death): " + sortedSpacedJSON(a.quarantineForensics(state, cause, reason)))
	for _, k := range flatOAuthTokenKeys {
		state.del(k)
	}
	for _, k := range emptyAgentKeyFields {
		state.del(k)
	}
	state.set("last_auth_error", objectOrEmpty(auth.LastAuthErrorMarker(Provider, cause, reason, "")))
	a.clearShared(ctx, reason)
	a.InvalidateStatusCache()
}

func quarantinePoolEntries(tx *auth.Tx, cause *auth.Error, reason string) bool {
	removed := tx.DropPoolSources(Provider, auth.PortalDeviceCodeSource, auth.SourceManualDeviceCode)
	if removed {
		var code any
		if cause.Code != "" {
			code = cause.Code
		}
		oauthTrace("portal_pool_device_code_quarantined", "", map[string]any{"reason": reason, "error_code": code})
	}
	return removed
}

func (a *Account) QuarantineState(ctx context.Context, state json.RawMessage, cause error, reason string) error {
	ae, ok := authErrorOf(cause)
	if !ok {
		return fmt.Errorf("portal: quarantine needs the auth error that ended the grant, got %T", cause)
	}
	ctx = a.bind(ctx)
	working := objectOrEmpty(state)
	a.quarantineState(ctx, working, ae, reason)
	err := a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		quarantinePoolEntries(tx, ae, reason)
		return tx.SaveLocal(working.raw())
	})
	if err != nil {
		return err
	}
	a.writeShared(ctx, working, true)
	return nil
}

func (a *Account) refreshOrQuarantine(ctx context.Context, tx *auth.Tx, client *http.Client, state *object, portalBaseURL, clientID, refreshToken, reason string, timeout time.Duration, persist func() error) (*object, error) {
	refreshed, err := refreshAccessToken(ctx, client, portalBaseURL, clientID, refreshToken, timeout)
	if err == nil {
		return refreshed, nil
	}
	if ae, ok := authErrorOf(err); ok && auth.IsTerminalRefreshError(err, Provider) {
		a.quarantineState(ctx, state, ae, reason)
		quarantinePoolEntries(tx, ae, reason)
		if perr := persist(); perr != nil {
			return nil, perr
		}
	}
	return nil, err
}

func applyRefreshedTokens(state, refreshed *object, refreshToken string, inferenceBaseURL *string) {
	now := time.Now()
	ttl := coerceTTL(refreshed.get("expires_in"))
	state.set("access_token", refreshed.get("access_token"))
	if v := refreshed.get("refresh_token"); truthy(v) {
		state.set("refresh_token", v)
	} else {
		state.set("refresh_token", refreshToken)
	}
	switch {
	case truthy(refreshed.get("token_type")):
		state.set("token_type", refreshed.get("token_type"))
	case truthy(state.get("token_type")):
		state.set("token_type", state.get("token_type"))
	default:
		state.set("token_type", "Bearer")
	}
	if v := refreshed.get("scope"); truthy(v) {
		state.set("scope", v)
	} else {
		state.set("scope", state.get("scope"))
	}
	if inferenceBaseURL != nil {
		state.set("inference_base_url", *inferenceBaseURL)
	}
	state.set("obtained_at", isoUTC(now))
	state.set("expires_in", ttl)
	state.set("expires_at", isoAfter(now, ttl))
}

func (a *Account) healedInferenceURL(ctx context.Context, refreshed *object) string {
	if u := a.validateInferenceURL(ctx, refreshed.get("inference_base_url")); u != "" {
		return u
	}
	return auth.DefaultPortalInferenceURL
}

func modelRank(id string) int {
	low := strings.ToLower(id)
	switch {
	case strings.Contains(low, "opus"):
		return 0
	case strings.Contains(low, "pro") && !strings.Contains(low, "sonnet"):
		return 1
	case strings.Contains(low, "sonnet"):
		return 3
	}
	return 2
}

const brandModelNeedle = "icebath"

func (a *Account) FetchModels(ctx context.Context, inferenceBaseURL string, apiKey auth.Secret, timeout time.Duration) ([]string, error) {
	client, _, err := a.verifiedClient(ctx, nil, "", nil)
	if err != nil {
		return nil, err
	}
	r, err := exchange(ctx, client, http.MethodGet, strings.TrimRight(inferenceBaseURL, "/")+"/models",
		map[string]string{"Authorization": "Bearer " + apiKey.Reveal()}, nil, timeout)
	if err != nil {
		return nil, err
	}
	if r.status != 200 {
		description := fmt.Sprintf("/models request failed with status %d", r.status)
		if payload, ok := parseObject(r.body); ok {
			if v := payload.get("error_description"); truthy(v) {
				description = textual(v)
			} else if v := payload.get("error"); truthy(v) {
				description = textual(v)
			}
		} else {
			slog.Debug("Could not parse error response JSON")
		}
		return nil, portalErr(description, "models_fetch_failed", false)
	}
	payload, ok := parseObject(r.body)
	if !ok {
		return nil, errors.New("portal: /models answered 200 without a JSON object")
	}
	ids := []string{}
	for _, item := range payload.list("data") {
		entry, _ := item.(*object)
		id := entry.get("id")
		if nonEmpty(id) && !strings.Contains(strings.ToLower(id.(string)), brandModelNeedle) {
			ids = append(ids, strings.TrimSpace(id.(string)))
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		ri, rj := modelRank(ids[i]), modelRank(ids[j])
		if ri != rj {
			return ri < rj
		}
		return ids[i] < ids[j]
	})
	return slices.Compact(ids), nil
}

type RefreshOptions struct {
	Timeout       time.Duration
	ForceRefresh  bool
	OnStateUpdate func(ctx context.Context, state json.RawMessage, reason string)
}

func (a *Account) RefreshState(ctx context.Context, src json.RawMessage, o RefreshOptions) (json.RawMessage, error) {
	state, err := a.refreshFromState(ctx, objectOrEmpty(src), o.Timeout, o.ForceRefresh, func(ctx context.Context, updated *object, reason string) {
		if o.OnStateUpdate != nil {
			o.OnStateUpdate(ctx, updated.raw(), reason)
		}
	})
	if err != nil {
		return nil, err
	}
	return state.raw(), nil
}

func (a *Account) refreshFromState(ctx context.Context, src *object, timeout time.Duration, force bool, onUpdate func(ctx context.Context, state *object, reason string)) (*object, error) {
	tls := src.obj("tls")
	var insecure *bool
	if v := tls.get("insecure"); v != nil {
		insecure = boolPtr(truthy(v))
	}
	caBundle := tls.str("ca_bundle")
	orDefault := func(key, def string) any {
		if v := src.get(key); truthy(v) {
			return v
		}
		return def
	}
	trimmed := func(key, def string) string {
		return strings.TrimRight(textual(orDefault(key, def)), "/")
	}
	orEmpty := func(key string) any {
		if src.has(key) {
			return src.get(key)
		}
		return ""
	}
	state := pairs(
		"access_token", orEmpty("access_token"), "refresh_token", orEmpty("refresh_token"),
		"client_id", orDefault("client_id", auth.DefaultPortalClientID),
		"portal_base_url", trimmed("portal_base_url", auth.DefaultPortalURL),
		"inference_base_url", trimmed("inference_base_url", auth.DefaultPortalInferenceURL),
		"token_type", orDefault("token_type", "Bearer"),
		"scope", orDefault("scope", auth.DefaultPortalScope),
		"obtained_at", src.get("obtained_at"), "expires_at", src.get("expires_at"),
		"agent_key", src.get("agent_key"), "agent_key_expires_at", src.get("agent_key_expires_at"),
		"tls", pairs("insecure", insecure != nil && *insecure, "ca_bundle", tls.get("ca_bundle")),
	)
	client, _, err := a.verifiedClient(ctx, insecure, caBundle, state)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	status := stateInvokeJWTStatus(state, state.get("access_token"))
	if force || status != "" {
		refreshToken, _ := state.get("refresh_token").(string)
		if refreshToken == "" {
			if status != "" {
				return nil, unusableInvokeJWTError(status, true)
			}
			return nil, portalErr("No refresh token is available for Icebath Portal.", "portal_auth_missing_refresh_token", true)
		}
		refreshed, err := refreshAccessToken(ctx, client, state.str("portal_base_url"), textual(state.get("client_id")), refreshToken, timeout)
		if err != nil {
			return nil, err
		}
		healed := a.healedInferenceURL(ctx, refreshed)
		applyRefreshedTokens(state, refreshed, refreshToken, &healed)
		if onUpdate != nil {
			onUpdate(ctx, state.clone(), "post_refresh_access_token")
		}
	}
	if err := assertInferenceJWTUsable(state, state.get("access_token")); err != nil {
		return nil, err
	}
	selectInvokeJWT(state, nil, "")
	return state, nil
}

func (a *Account) tryImportShared(ctx context.Context, timeout time.Duration) *object {
	refreshed, err := func() (*object, error) {
		lctx, unlock, err := a.lockShared(ctx, sharedLockTimeout(timeout))
		if err != nil {
			return nil, err
		}
		defer unlock()
		shared := a.readShared()
		if shared == nil {
			return nil, nil
		}
		state := sharedShape(shared)
		state.set("agent_key", nil)
		state.set("agent_key_expires_at", nil)
		state.set("tls", pairs("insecure", false, "ca_bundle", nil))
		refreshed, err := a.refreshFromState(lctx, state, timeout, true, func(ctx context.Context, updated *object, _ string) {
			a.writeShared(ctx, updated, true)
		})
		if err != nil {
			return nil, err
		}
		a.writeShared(lctx, refreshed, true)
		return refreshed, nil
	}()
	if err != nil {
		fields := map[string]any{"error_type": fmt.Sprintf("%T", err)}
		ae, isAuth := authErrorOf(err)
		if isAuth {
			fields["error_code"] = ae.Code
		}
		oauthTrace("portal_shared_import_failed", "", fields)
		if isAuth && auth.IsTerminalRefreshError(err, Provider) {
			a.clearShared(ctx, "shared_import_terminal_refresh_failure")
		}
		slog.Debug("Shared Icebath Portal import failed", "error", httpclient.Message(err))
		return nil
	}
	return refreshed
}
