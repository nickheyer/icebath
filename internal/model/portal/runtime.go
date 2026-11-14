package portal

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	notLoggedIn         = "icebath is not logged into Icebath Portal."
	resolveTokenMemoTTL = 5 * time.Second
)

type ResolveOptions struct {
	Timeout          time.Duration
	Insecure         *bool
	CABundle         string
	ForceRefresh     bool
	StaleAccessToken auth.Secret
}

type Credentials struct {
	Provider  string
	BaseURL   string
	APIKey    auth.Secret
	KeyID     string
	ExpiresAt string
	ExpiresIn int64
	Source    string
	AuthPath  string
	StatePath string
}

func (a *Account) txState(tx *auth.Tx) *object {
	raw, ok := tx.State()
	if !ok {
		return newObject()
	}
	state := objectOrEmpty(raw)
	migrateStalePortalURL(state)
	return state
}

func (a *Account) currentState(ctx context.Context) (*object, error) {
	raw, ok, err := a.store.State(a.bind(ctx), Provider)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	state, ok := parseObject(raw)
	if !ok {
		return nil, nil
	}
	migrateStalePortalURL(state)
	return state, nil
}

func (a *Account) quietState(ctx context.Context) *object {
	state, err := a.currentState(ctx)
	if err != nil {
		slog.Debug("guest: auth store unreadable", "error", err)
		return nil
	}
	return state
}

func (a *Account) syncPool(ctx context.Context) {
	if _, err := a.store.Pool(a.bind(ctx), Provider); err != nil {
		slog.Debug("Failed to sync Icebath Portal credential pool from auth store", "error", err)
	}
}

func sequenceID() string {
	id := uuid.New()
	return hex.EncodeToString(id[:6])
}

type runtimeResolve struct {
	a            *Account
	tx           *auth.Tx
	ctx          context.Context
	state        *object
	force        bool
	stale        string
	timeout      time.Duration
	sequence     string
	persisted    *object
	persistedAny bool
	accessToken  any
	refreshToken any
	route        routing
}

func (r *runtimeResolve) reloadRouting() {
	r.route = r.a.effectiveRouting(r.ctx, r.state)
}

func (r *runtimeResolve) persist(reason string) error {
	if sameValue(effectiveState(r.state), effectiveState(r.persisted)) {
		oauthTrace("portal_state_persist_skipped", r.sequence, map[string]any{"reason": reason})
		return nil
	}
	if err := r.tx.Save(r.state.raw()); err != nil {
		oauthTrace("portal_state_persist_failed", r.sequence, map[string]any{"reason": reason, "error_type": fmt.Sprintf("%T", err)})
		return err
	}
	oauthTrace("portal_state_persisted", r.sequence, map[string]any{"reason": reason,
		"refresh_token_fp": tokenFingerprint(r.state.get("refresh_token")), "access_token_fp": tokenFingerprint(r.state.get("access_token"))})
	r.persisted = r.state.clone()
	r.persistedAny = true
	r.a.writeShared(r.ctx, r.state, true)
	return nil
}

func (r *runtimeResolve) hasAccessToken() bool {
	s, ok := r.accessToken.(string)
	return ok && s != ""
}

func (r *runtimeResolve) invokeJWTStatus() string {
	return stateInvokeJWTStatus(r.state, r.accessToken)
}

func (r *runtimeResolve) mergeShared(ctx context.Context) bool {
	if !r.a.mergeShared(ctx, r.state) {
		return false
	}
	r.accessToken = r.state.get("access_token")
	r.refreshToken = r.state.get("refresh_token")
	r.reloadRouting()
	return true
}

func (r *runtimeResolve) skipRefreshIfPeerRotated() {
	token, _ := r.accessToken.(string)
	if r.force && r.stale != "" && token != "" && token != r.stale && r.invokeJWTStatus() == "" {
		oauthTrace("refresh_skipped_peer_rotated", r.sequence, map[string]any{"access_token_fp": tokenFingerprint(token)})
		r.force = false
	}
}

func (r *runtimeResolve) refresh(ctx context.Context, client *http.Client, status string) error {
	refreshToken, _ := r.refreshToken.(string)
	if refreshToken == "" {
		if status == "" {
			status = "force_refresh"
		}
		return unusableInvokeJWTError(status, true)
	}
	reason := "force_refresh"
	if !r.force {
		reason = status
		if reason == "" {
			reason = "access_unusable"
		}
	}
	oauthTrace("refresh_start", r.sequence, map[string]any{"reason": reason, "refresh_token_fp": tokenFingerprint(refreshToken)})
	refreshed, err := r.a.refreshOrQuarantine(ctx, r.tx, client, r.state, r.route.portalURL, r.route.clientID, refreshToken,
		"runtime_access_refresh_failure", r.timeout, func() error { return r.persist("terminal_runtime_access_refresh_failure") })
	if err != nil {
		return err
	}
	r.route.storedInference = r.a.healedInferenceURL(ctx, refreshed)
	r.route.inference = r.a.inferenceOverride(ctx)
	if r.route.inference == "" {
		r.route.inference = r.route.storedInference
	}
	applyRefreshedTokens(r.state, refreshed, refreshToken, &r.route.storedInference)
	r.accessToken = r.state.get("access_token")
	r.refreshToken = r.state.get("refresh_token")
	oauthTrace("refresh_success", r.sequence, map[string]any{"reason": reason,
		"previous_refresh_token_fp": tokenFingerprint(refreshToken), "new_refresh_token_fp": tokenFingerprint(r.refreshToken)})
	return r.persist("post_refresh_access_token")
}

func (r *runtimeResolve) ensureUsableAccessToken(client *http.Client) error {
	if isGuestState(r.state) {
		if r.force || r.invokeJWTStatus() != "" {
			if _, err := r.a.refreshGuestState(r.ctx, r.state, client, purposeInference, r.timeout); err != nil {
				return err
			}
			r.accessToken = r.state.get("access_token")
			if v := r.state.get("inference_base_url"); truthy(v) {
				r.route.storedInference = textual(v)
			}
			r.route.inference = r.a.inferenceOverride(r.ctx)
			if r.route.inference == "" {
				r.route.inference = r.route.storedInference
			}
			return r.persist("guest_exchange")
		}
		return nil
	}
	lockTimeout := sharedLockTimeout(r.timeout)
	if !r.hasAccessToken() {
		lctx, unlock, err := r.a.lockShared(r.ctx, lockTimeout)
		if err != nil {
			return err
		}
		merged := r.mergeShared(lctx)
		var perr error
		if merged {
			perr = r.persist("runtime_shared_merge_missing_access_token")
		}
		unlock()
		if perr != nil {
			return perr
		}
	}
	if !r.hasAccessToken() {
		return portalErr("No access token found for Icebath Portal login.", "portal_auth_missing_access_token", true)
	}
	status := r.invokeJWTStatus()
	r.skipRefreshIfPeerRotated()
	if !(r.force || status != "") {
		return nil
	}
	lctx, unlock, err := r.a.lockShared(r.ctx, lockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	if r.mergeShared(lctx) {
		status = r.invokeJWTStatus()
		if err := r.persist("post_shared_merge_access_unusable"); err != nil {
			return err
		}
		r.skipRefreshIfPeerRotated()
	}
	if r.force || status != "" {
		return r.refresh(lctx, client, status)
	}
	return nil
}

func (a *Account) RuntimeCredentials(ctx context.Context, o ResolveOptions) (Credentials, error) {
	ctx = a.bind(ctx)
	creds, err := runWithChallenge(ctx, a, func(ctx context.Context) (Credentials, error) { return a.resolveRuntime(ctx, o) })
	var dead *CredentialDeadError
	if errors.As(err, &dead) {
		state, serr := a.currentState(ctx)
		if serr != nil {
			return Credentials{}, serr
		}
		code := dead.Err.Code
		if code == "" {
			code = AnonCredentialDead
		}
		if cerr := a.clearDeadGuest(ctx, code, state.str("anon_token")); cerr != nil {
			return Credentials{}, cerr
		}
	}
	return creds, err
}

func (a *Account) resolveRuntime(ctx context.Context, o ResolveOptions) (Credentials, error) {
	var (
		run       *runtimeResolve
		state     *object
		statePath string
	)
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	err := a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		tctx := tx.Context()
		state = a.txState(tx)
		if state.size() == 0 {
			return portalErr(notLoggedIn, "portal_auth_missing", true)
		}
		if a.guestWithoutFreeTier(tctx, state) {
			return portalErr(notLoggedIn, "portal_auth_missing", false)
		}
		run = &runtimeResolve{a: a, tx: tx, ctx: tctx, state: state, force: o.ForceRefresh, stale: o.StaleAccessToken.Reveal(),
			timeout: timeout, sequence: sequenceID(), persisted: state.clone(),
			accessToken: state.get("access_token"), refreshToken: state.get("refresh_token")}
		run.reloadRouting()
		client, tls, err := a.verifiedClient(tctx, o.Insecure, o.CABundle, state)
		if err != nil {
			return err
		}
		oauthTrace("portal_runtime_credentials_start", run.sequence, map[string]any{"refresh_token_fp": tokenFingerprint(state.get("refresh_token"))})
		if err := run.ensureUsableAccessToken(client); err != nil {
			return err
		}
		if err := assertInferenceJWTUsable(state, run.accessToken); err != nil {
			return err
		}
		selectInvokeJWT(state, run.accessToken, run.sequence)
		state.set("portal_base_url", run.route.portalURL)
		state.set("client_id", run.route.clientID)
		state.set("inference_base_url", run.route.storedInference)
		state.set("tls", tlsObject(tls))
		statePath = tx.Source()
		return run.persist("resolve_portal_runtime_credentials_final")
	})
	if err != nil {
		return Credentials{}, err
	}
	if run.persistedAny {
		a.syncPool(ctx)
	}
	apiKey, _ := state.get("agent_key").(string)
	if apiKey == "" {
		return Credentials{}, portalErr("Failed to resolve an Icebath Portal inference API key", "server_error", false)
	}
	if statePath == "" {
		statePath = a.store.Path()
	}
	expiresAt := state.get("agent_key_expires_at")
	return Credentials{
		Provider: Provider, BaseURL: run.route.inference, APIKey: auth.NewSecret(apiKey), KeyID: textualOrEmpty(state.get("agent_key_id")),
		ExpiresAt: textualOrEmpty(expiresAt), ExpiresIn: remainingTTL(expiresAt, state.get("agent_key_expires_in")),
		Source: auth.PortalAuthPathInvokeJWT, AuthPath: auth.PortalAuthPathInvokeJWT, StatePath: statePath,
	}, nil
}

type AccessTokenOptions struct {
	Timeout     time.Duration
	RefreshSkew time.Duration
}

func (a *Account) AccessToken(ctx context.Context, o AccessTokenOptions) (auth.Secret, error) {
	ctx = a.bind(ctx)
	skew := o.RefreshSkew
	if skew <= 0 {
		skew = auth.AccessTokenRefreshSkew
	}
	token, err := runWithChallenge(ctx, a, func(ctx context.Context) (string, error) { return a.resolveAccessToken(ctx, o.Timeout, skew) })
	if err != nil {
		return auth.Secret{}, err
	}
	return auth.NewSecret(token), nil
}

func (a *Account) dropTokenMemo() {
	a.mu.Lock()
	a.tokenMemo = tokenMemo{}
	a.mu.Unlock()
}

func (a *Account) resolveAccessToken(ctx context.Context, timeout, skew time.Duration) (string, error) {
	a.mu.Lock()
	memo := a.tokenMemo
	a.mu.Unlock()
	if memo.set && time.Since(memo.at) < resolveTokenMemoTTL {
		return memo.token, nil
	}
	httpTimeout := timeout
	if httpTimeout <= 0 {
		httpTimeout = defaultTimeout
	}
	token := ""
	err := a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		tctx := tx.Context()
		state := a.txState(tx)
		if state.size() == 0 {
			return portalErr(notLoggedIn, "portal_auth_missing", true)
		}
		portalBaseURL := a.portalBaseURL(tctx, state)
		clientID := auth.DefaultPortalClientID
		if v := state.get("client_id"); truthy(v) {
			clientID = textual(v)
		}
		client, tls, err := a.verifiedClient(tctx, nil, "", state)
		if err != nil {
			return err
		}
		persist := func() error { return tx.Save(state.raw()) }
		lctx, unlock, err := a.lockShared(tctx, sharedLockTimeout(httpTimeout))
		if err != nil {
			return err
		}
		defer unlock()
		if isGuestState(state) {
			if held := heldConnectorsToken(state, &skew); held != "" {
				token = held
				return nil
			}
			minted, err := a.refreshGuestState(lctx, state, client, purposeConnectors, httpTimeout)
			if err != nil {
				return err
			}
			if err := persist(); err != nil {
				return err
			}
			a.writeShared(lctx, state, true)
			token = minted
			return nil
		}
		merged := a.mergeShared(lctx, state)
		accessToken, _ := state.get("access_token").(string)
		refreshToken, _ := state.get("refresh_token").(string)
		if accessToken == "" {
			return portalErr("No access token found for Icebath Portal login.", "portal_auth_missing_access_token", true)
		}
		if !isExpiring(state.get("expires_at"), skew) {
			if merged {
				if err := persist(); err != nil {
					return err
				}
			}
			token = accessToken
			return nil
		}
		if refreshToken == "" {
			return portalErr("Session expired and no refresh token is available.", "portal_auth_missing_refresh_token", true)
		}
		refreshed, err := a.refreshOrQuarantine(lctx, tx, client, state, portalBaseURL, clientID, refreshToken,
			"managed_access_token_refresh_failure", httpTimeout, persist)
		if err != nil {
			return err
		}
		applyRefreshedTokens(state, refreshed, refreshToken, nil)
		state.set("portal_base_url", portalBaseURL)
		state.set("client_id", clientID)
		state.set("tls", tlsObject(tls))
		if err := persist(); err != nil {
			return err
		}
		a.writeShared(lctx, state, true)
		token = state.str("access_token")
		return nil
	})
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.tokenMemo = tokenMemo{at: time.Now(), token: token, set: true}
	a.mu.Unlock()
	return token, nil
}
