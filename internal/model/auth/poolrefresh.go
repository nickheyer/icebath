package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

var portalExtraStateKeys = []string{
	"obtained_at", "expires_in", "agent_key_id", "agent_key_expires_in", "agent_key_reused", "agent_key_obtained_at",
	"auth_method", "account_tier", "anon_token", "user_id", "org_id",
}

var tokensSingletonDisplay = map[string]string{ProviderCodex: "Codex", ProviderXAIOAuth: "xAI"}

func isTokensSingleton(provider string) bool { _, ok := tokensSingletonDisplay[provider]; return ok }

func (p *Pool) lockedState(ctx context.Context, provider string) (*doc, error) {
	_, unlock, err := p.s.lock(ctx, p.s.authPath, LockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return p.s.providerState(ctx, provider)
}

func (p *Pool) syncAnthropicFromCredentialsFile(ctx context.Context, e *Entry) *Entry {
	if p.provider != ProviderAnthropic || e.Source != sourceClaudeCode {
		return e
	}
	creds := p.s.readClaudeCodeCredentials(ctx)
	if creds == nil {
		return e
	}
	access, refresh := creds.accessToken, creds.refreshToken
	if (access != "" || refresh != "") && ((access != "" && access != e.AccessToken.Reveal()) || (refresh != "" && refresh != e.RefreshToken.Reveal())) {
		slog.Debug(fmt.Sprintf("Pool entry %s: syncing tokens from credentials file (tokens changed)", e.ID))
		adopted, err := p.adopt(ctx, e, func(u *Entry) {
			if access != "" {
				u.AccessToken = NewSecret(access)
			}
			if refresh != "" {
				u.RefreshToken = NewSecret(refresh)
			}
			if creds.expiresAt != 0 {
				u.ExpiresAtMS = creds.expiresAt
			}
			clearStatus(u)
		})
		if err != nil {
			slog.Debug("Failed to sync from credentials file", "error", err)
			return e
		}
		return adopted
	}
	return e
}

func (p *Pool) pluginRefresher() Refresher {
	if p.provider == ProviderAnthropic || p.provider == ProviderPortal || isTokensSingleton(p.provider) {
		return nil
	}
	if p.spec.Refresh != nil {
		return p.spec.Refresh
	}
	return p.s.refresher(p.provider)
}

func (p *Pool) syncFromPoolStore(ctx context.Context, e *Entry) *Entry {
	if p.provider != ProviderAnthropic && p.provider != ProviderXAIOAuth && p.pluginRefresher() == nil {
		return e
	}
	if p.provider == ProviderAnthropic && IsBorrowedSource(e.Source, p.provider) {
		return e
	}
	rows, err := p.s.readPool(ctx, p.provider)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s OAuth entry from credential pool", p.provider), "error", err)
		return e
	}
	var persisted *doc
	for _, row := range rows {
		if rowID(row) == e.ID {
			persisted = row
			break
		}
	}
	if persisted == nil {
		return e
	}
	func() {
		_, unlock := p.lock(ctx)
		defer unlock()
		p.persistedPairs[e.ID] = credentialTokenPair(persisted)
	}()
	stored := entryFromDoc(p.provider, persisted)
	if p.provider != ProviderXAIOAuth && strings.TrimSpace(stored.AccessToken.Reveal()) == "" && strings.TrimSpace(stored.RefreshToken.Reveal()) == "" {
		return e
	}
	if stored.AccessToken != e.AccessToken || stored.RefreshToken != e.RefreshToken {
		slog.Debug(fmt.Sprintf("Pool entry %s: adopting %s OAuth tokens rotated by another pool instance", e.ID, p.provider))
		p.replaceEntry(ctx, e, stored)
		return stored
	}
	return e
}

func codexEntryTracksSingleton(e *Entry, tokens *doc) bool {
	if e.Source == "device_code" {
		return true
	}
	if e.Source != SourceManualDeviceCode {
		return false
	}
	account, subject, ok := codexPrincipalIdentity(e.AccessToken.Reveal())
	otherAccount, otherSubject, otherOK := codexPrincipalIdentity(tokens.str("access_token"))
	return ok && otherOK && account == otherAccount && subject == otherSubject
}

func singletonPredatesEntry(state *doc, e *Entry) bool {
	entryTS, ok := parseAbsolute(e.LastRefresh)
	if !ok {
		return false
	}
	stateTS, ok := parseAbsolute(state.value("last_refresh"))
	return ok && stateTS < entryTS
}

func (p *Pool) syncFromAuthStore(ctx context.Context, e *Entry) *Entry {
	display, ok := tokensSingletonDisplay[p.provider]
	if !ok {
		return e
	}
	codex := p.provider == ProviderCodex
	if e.Source != "device_code" && !(codex && e.Source == SourceManualDeviceCode) {
		return e
	}
	state, err := p.lockedState(ctx, p.provider)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s entry from auth.json", display), "error", err)
		return e
	}
	tokens := state.obj("tokens")
	if tokens == nil || (codex && !codexEntryTracksSingleton(e, tokens)) {
		return e
	}
	storeAccess, storeRefresh := textOf(tokens.value("access_token")), textOf(tokens.value("refresh_token"))
	entryRefresh := e.RefreshToken.Reveal()
	adopt := storeAccess != "" && (storeAccess != e.AccessToken.Reveal() || (storeRefresh != "" && storeRefresh != entryRefresh))
	if !adopt && codex && storeRefresh != "" && storeRefresh != entryRefresh && storeAccess == "" {
		slog.Info(fmt.Sprintf("Pool entry %s: auth.json has newer refresh_token but no access_token; adopting refresh_token to avoid replaying consumed token", e.ID))
		adopt = true
	}
	if adopt && singletonPredatesEntry(state, e) {
		slog.Info(fmt.Sprintf("Pool entry %s: auth.json singleton predates this entry's rotation (last_refresh %s < %s); "+
			"keeping pool chain to avoid replaying the consumed refresh token", e.ID, state.str("last_refresh"), e.LastRefresh))
		adopt = false
	}
	if !adopt {
		return e
	}
	slog.Debug(fmt.Sprintf("Pool entry %s: syncing %s tokens from auth.json (refreshed by another process)", e.ID, display))
	adopted, err := p.adopt(ctx, e, func(u *Entry) {
		if storeAccess != "" {
			u.AccessToken = NewSecret(storeAccess)
		}
		if storeRefresh != "" {
			u.RefreshToken = NewSecret(storeRefresh)
		}
		clearStatus(u)
		if last := state.str("last_refresh"); last != "" {
			u.LastRefresh = last
		}
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s entry from auth.json", display), "error", err)
		return e
	}
	return adopted
}

func (p *Pool) syncPortalFromAuthStore(ctx context.Context, e *Entry) *Entry {
	if p.provider != ProviderPortal || e.Source != "device_code" {
		return e
	}
	state, err := p.lockedState(ctx, ProviderPortal)
	if err != nil || state.size() == 0 {
		return e
	}
	current := map[string]string{
		"access_token": e.AccessToken.Reveal(), "refresh_token": e.RefreshToken.Reveal(), "expires_at": e.ExpiresAt,
		"agent_key": e.AgentKey.Reveal(), "agent_key_expires_at": e.AgentKeyExpiresAt, "inference_base_url": e.InferenceBaseURL,
	}
	differs := false
	for k, mine := range current {
		if v := state.value(k); v != nil && v != "" && textOf(v) != mine {
			differs = true
		}
	}
	if !differs {
		return e
	}
	slog.Debug(fmt.Sprintf("Pool entry %s: syncing Icebath Portal state from auth.json", e.ID))
	adopted, err := p.adopt(ctx, e, func(u *Entry) {
		clearStatus(u)
		if v := state.str("access_token"); v != "" {
			u.AccessToken = NewSecret(v)
		}
		if v := state.str("refresh_token"); v != "" {
			u.RefreshToken = NewSecret(v)
		}
		if v := state.str("expires_at"); v != "" {
			u.ExpiresAt = v
		}
		if v := state.str("agent_key"); v != "" {
			u.AgentKey = NewSecret(v)
		}
		if v := state.str("agent_key_expires_at"); v != "" {
			u.AgentKeyExpiresAt = v
		}
		if v := state.str("inference_base_url"); v != "" {
			u.InferenceBaseURL = v
		}
		for _, k := range portalExtraStateKeys {
			if v := state.value(k); v != nil {
				u.setExtra(k, deepCopy(v))
			}
		}
	})
	if err != nil {
		slog.Debug("Failed to sync Icebath Portal entry from auth.json", "error", err)
		return e
	}
	return adopted
}

func (p *Pool) applyEntryToSingletonState(e *Entry, state *doc) bool {
	if p.provider == ProviderPortal {
		state.set("access_token", e.AccessToken.Reveal())
		for _, kv := range [][2]string{{"refresh_token", e.RefreshToken.Reveal()}, {"expires_at", e.ExpiresAt}, {"agent_key", e.AgentKey.Reveal()}, {"agent_key_expires_at", e.AgentKeyExpiresAt}} {
			if kv[1] != "" {
				state.set(kv[0], kv[1])
			}
		}
		for _, k := range portalExtraStateKeys {
			if v := e.extra.value(k); v != nil {
				state.set(k, deepCopy(v))
			}
		}
		if e.InferenceBaseURL != "" {
			state.set("inference_base_url", e.InferenceBaseURL)
		}
		return true
	}
	tokens := state.obj("tokens")
	if tokens == nil {
		return false
	}
	tokens.set("access_token", e.AccessToken.Reveal())
	if !e.RefreshToken.Empty() {
		tokens.set("refresh_token", e.RefreshToken.Reveal())
	}
	if e.LastRefresh != "" {
		state.set("last_refresh", e.LastRefresh)
	}
	return true
}

func (p *Pool) syncDeviceCodeToAuthStore(ctx context.Context, e *Entry) {
	if e.Source != "device_code" || (p.provider != ProviderPortal && !isTokensSingleton(p.provider)) {
		return
	}
	lctx, unlock, err := p.s.lock(ctx, p.s.authPath, LockTimeout)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s pool entry back to auth store", p.provider), "error", err)
		return
	}
	defer unlock()
	store, err := p.s.load(p.s.authPath)
	if err != nil {
		return
	}
	state, source := p.s.stateWithSource(store, p.provider)
	if state == nil || !p.applyEntryToSingletonState(e, state) {
		return
	}
	if source != "" && p.s.rootAuth != "" && samePath(source, p.s.rootAuth) {
		p.s.writeThroughToRoot(lctx, p.provider, state)
		return
	}
	if err := p.s.storeProviderState(lctx, store, p.provider, state, false); err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s pool entry back to auth store", p.provider), "error", err)
		return
	}
	if err := p.s.save(lctx, store, p.s.authPath); err != nil {
		slog.Debug(fmt.Sprintf("Failed to sync %s pool entry back to auth store", p.provider), "error", err)
	}
}

func (p *Pool) singleUseRefreshLockTimeout(ctx context.Context) time.Duration {
	name := "ICEBATH_ANTHROPIC_REFRESH_TIMEOUT_SECONDS"
	switch p.provider {
	case ProviderCodex:
		name = "ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS"
	case ProviderXAIOAuth:
		name = "ICEBATH_XAI_REFRESH_TIMEOUT_SECONDS"
	}
	return max(LockTimeout, seconds(p.s.envFloat(ctx, name, 20))+5*time.Second)
}

func (p *Pool) refreshEntry(ctx context.Context, e *Entry, force bool, failedKey string) (*Entry, error) {
	if e.AuthType != AuthTypeOAuth || e.RefreshToken.Empty() {
		if force {
			if _, err := p.markExhausted(ctx, e, Failure{}, true); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	singleUse := p.provider == ProviderCodex || p.provider == ProviderXAIOAuth || p.provider == ProviderAnthropic
	if !singleUse && p.pluginRefresher() == nil {
		return p.refreshEntryImpl(ctx, e, force, failedKey)
	}
	timeout := p.singleUseRefreshLockTimeout(ctx)
	lctx, unlock, err := p.s.lock(ctx, p.s.authPath, timeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if p.provider == ProviderCodex {
		synced := p.syncFromAuthStore(lctx, e)
		if synced != e && !force && !p.entryNeedsRefresh(synced) {
			return synced, nil
		}
		return p.refreshEntryImpl(lctx, synced, force, "")
	}
	synced := p.syncFromPoolStore(lctx, e)
	if p.provider == ProviderAnthropic && synced.Source == sourceClaudeCode {
		cctx, unlockFile, err := p.s.lock(lctx, p.s.claudeCodeCredentialsPath(lctx), timeout)
		if err != nil {
			return nil, err
		}
		defer unlockFile()
		synced = p.syncAnthropicFromCredentialsFile(cctx, synced)
		if synced.RefreshToken != e.RefreshToken {
			return synced, nil
		}
		return p.refreshEntryImpl(cctx, synced, force, "")
	}
	if synced.AccessToken != e.AccessToken || synced.RefreshToken != e.RefreshToken {
		return synced, nil
	}
	return p.refreshEntryImpl(lctx, synced, force, "")
}

func (p *Pool) failClosedUnpersistedRotation(ctx context.Context, e *Entry, cause error, store string) error {
	slog.Error(fmt.Sprintf("Anthropic %s refresh rotated the single-use token but could not commit it to %s (%v) — "+
		"failing closed and quarantining the credential; re-authenticate to recover", e.Source, store, cause))
	p.s.markRotationConsumedUncommitted(ctx, p.s.spentRotationSourcePath(ctx, e.Source), e.AccessToken.Reveal(), e.RefreshToken.Reveal())
	_, err := p.markExhausted(ctx, e, Failure{ErrorReason: ReasonPersistFailed,
		Message: fmt.Sprintf("rotated credential was not durably written to %s: %v", store, cause)}, true)
	return err
}

func (p *Pool) commitAnthropicRotation(ctx context.Context, e *Entry, tokens AnthropicTokens) (bool, error) {
	var err error
	store := ""
	switch e.Source {
	case sourceClaudeCode:
		store = "~/.claude/.credentials.json"
		err = p.s.writeClaudeCodeCredentials(ctx, tokens.AccessToken.Reveal(), tokens.RefreshToken.Reveal(), tokens.ExpiresAtMS, nil, e.RefreshToken.Reveal())
	case sourceIcebathPKCE:
		store = "~/.icebath/.anthropic_oauth.json"
		target := ""
		if slices.Contains(p.borrowedRootIDs, e.ID) {
			target = p.s.rootAnthropicOAuthFile()
		}
		err = p.s.writeAnthropicOAuthFile(ctx, tokens.AccessToken.Reveal(), tokens.RefreshToken.Reveal(), intNum(tokens.ExpiresAtMS), target)
	default:
		return false, nil
	}
	if err != nil {
		if perr := p.failClosedUnpersistedRotation(ctx, e, err, store); perr != nil {
			return false, perr
		}
		return true, nil
	}
	return false, nil
}

func (p *Pool) refreshAnthropic(ctx context.Context, e *Entry) (updated *Entry, done bool, err error) {
	sourcePath := p.s.spentRotationSourcePath(ctx, e.Source)
	if p.s.isRotationConsumedUncommitted(e.RefreshToken.Reveal(), sourcePath) || p.s.isRotationConsumedUncommitted(e.AccessToken.Reveal(), sourcePath) {
		store := sourcePath
		if store == "" {
			store = "credential store"
		}
		if perr := p.failClosedUnpersistedRotation(ctx, e, errors.New("credential pair was rotated by another process but the rotation never committed (spent-rotation sidecar verdict)"), store); perr != nil {
			return nil, false, perr
		}
		return nil, true, nil
	}
	tokens, err := p.s.refreshAnthropicPure(ctx, e.RefreshToken.Reveal(), strings.HasSuffix(e.Source, sourceIcebathPKCE))
	if err != nil {
		return nil, false, err
	}
	updated = e.clone()
	updated.AccessToken, updated.RefreshToken, updated.ExpiresAtMS = tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAtMS
	if done, err := p.commitAnthropicRotation(ctx, e, tokens); done || err != nil {
		return nil, done, err
	}
	return updated, false, nil
}

func (p *Pool) postTokensRefresh(ctx context.Context, e *Entry) (*Entry, error) {
	var pair rotatedPair
	if p.provider == ProviderCodex {
		rotated, err := p.s.refreshCodexPure(ctx, e.RefreshToken.Reveal(), 20*time.Second)
		if err != nil {
			return nil, err
		}
		pair = rotated
	} else {
		tokens, lastRefresh, err := p.s.refreshXAIPure(ctx, e.RefreshToken.Reveal(), "", 20*time.Second)
		if err != nil {
			return nil, err
		}
		pair = rotatedPair{access: tokens.str("access_token"), refresh: tokens.str("refresh_token"), lastRefresh: lastRefresh}
	}
	updated := e.clone()
	updated.AccessToken, updated.RefreshToken, updated.LastRefresh = NewSecret(pair.access), NewSecret(pair.refresh), pair.lastRefresh
	return updated, nil
}

func (p *Pool) refreshEntryImpl(ctx context.Context, e *Entry, force bool, failedKey string) (*Entry, error) {
	var updated *Entry
	var err error
	plugin := p.pluginRefresher()
	switch {
	case p.provider == ProviderAnthropic:
		var done bool
		if updated, done, err = p.refreshAnthropic(ctx, e); done {
			return nil, nil
		}
	case isTokensSingleton(p.provider):
		e = p.syncFromAuthStore(ctx, e)
		updated, err = p.postTokensRefresh(ctx, e)
	case plugin != nil:
		var rotated Entry
		if rotated, err = plugin.Refresh(ctx, RefreshRequest{Entry: *e.clone(), Force: force, FailedKey: NewSecret(failedKey)}); err == nil {
			if rotated.AccessToken.Empty() && rotated.RefreshToken.Empty() {
				err = errors.New("provider refresh returned no rotated fields")
			} else {
				updated = rotated.clone()
				updated.Provider, updated.ID = p.provider, e.ID
			}
		}
	case p.provider == ProviderPortal:
		refresher := p.s.refresher(ProviderPortal)
		if refresher == nil {
			return e, nil
		}
		stale := failedKey
		for _, candidate := range []string{e.RuntimeAPIKey().Reveal(), e.AgentKey.Reveal(), e.AccessToken.Reveal()} {
			if stale == "" {
				stale = candidate
			}
		}
		if synced := p.syncPortalFromAuthStore(ctx, e); synced != e {
			e = synced
			if key := e.RuntimeAPIKey().Reveal(); force && key != "" && key != stale {
				slog.Debug(fmt.Sprintf("Icebath Portal entry %s: adopting peer-rotated token, not refreshing", e.ID))
				return e, nil
			}
		}
		if _, err = refresher.Refresh(ctx, RefreshRequest{Entry: *e.clone(), Force: force, FailedKey: NewSecret(stale)}); err == nil {
			updated = p.syncPortalFromAuthStore(ctx, e)
		}
	default:
		return e, nil
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Credential refresh failed for %s/%s", p.provider, e.ID), "error", transportFailure(err))
		return p.recoverFailedRefresh(ctx, e, err)
	}
	updated = updated.clone()
	markOK(updated)
	p.replaceEntry(ctx, e, updated)
	if err := p.persist(ctx, nil, []string{updated.ID}); err != nil {
		return nil, err
	}
	p.syncDeviceCodeToAuthStore(ctx, updated)
	return updated, nil
}

func isTerminalPluginRefreshError(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && (ae.Relogin || slices.Contains(oauthGrantDeadCodes, ae.Code))
}

func (p *Pool) retryClaudeCodeRefresh(ctx context.Context, synced *Entry) (*Entry, bool, error) {
	tokens, err := p.s.refreshAnthropicPure(ctx, synced.RefreshToken.Reveal(), strings.HasSuffix(synced.Source, sourceIcebathPKCE))
	if err != nil {
		return nil, false, err
	}
	done, err := p.commitAnthropicRotation(ctx, synced, tokens)
	if err != nil || done {
		return nil, done, err
	}
	adopted, err := p.adopt(ctx, synced, func(u *Entry) {
		u.AccessToken, u.RefreshToken, u.ExpiresAtMS = tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAtMS
		u.LastStatus, u.LastStatusAt, u.statusAt, u.LastErrorCode = StatusOK, time.Time{}, 0, 0
	})
	if err != nil {
		return nil, false, err
	}
	return adopted, true, nil
}

func (p *Pool) recoverFailedRefresh(ctx context.Context, e *Entry, cause error) (*Entry, error) {
	switch {
	case p.provider == ProviderAnthropic:
		if e.Source == sourceClaudeCode {
			synced := p.syncAnthropicFromCredentialsFile(ctx, e)
			if synced.RefreshToken != e.RefreshToken {
				slog.Debug("Retrying refresh with synced token from credentials file")
				result, done, err := p.retryClaudeCodeRefresh(ctx, synced)
				if done {
					return result, nil
				}
				slog.Debug("Retry refresh also failed", "error", transportFailure(err))
			} else if !p.entryNeedsRefresh(synced) {
				slog.Debug("Credentials file has valid token, using without refresh")
				return synced, nil
			}
		} else if synced := p.syncFromPoolStore(ctx, e); synced.RefreshToken != e.RefreshToken {
			slog.Debug("Anthropic OAuth refresh failed but pool store has newer tokens — adopting")
			return p.adopt(ctx, synced, markOK)
		}
		if IsTerminalAnthropicRefreshError(cause) {
			slog.Warn(fmt.Sprintf("Anthropic OAuth refresh token for %s is terminally invalid (%v); the credential "+
				"leaves rotation. Re-run 'icebath auth add anthropic' to sign in again.", labelOf(e), cause))
			return nil, p.markDeadRefreshGrant(ctx, e, cause)
		}
	case isTokensSingleton(p.provider):
		display := tokensSingletonDisplay[p.provider]
		if synced := p.syncFromAuthStore(ctx, e); synced.RefreshToken != e.RefreshToken {
			slog.Debug(fmt.Sprintf("%s OAuth refresh failed but auth.json has newer tokens — adopting", display))
			return p.adopt(ctx, synced, markOK)
		}
		if IsTerminalRefreshError(cause, p.provider) {
			slog.Warn(fmt.Sprintf("%s OAuth refresh token is terminally invalid (%v); clearing local token state. "+
				"Re-run 'icebath auth add %s' to sign in again.", display, cause, p.provider))
			p.clearTerminalTokensState(ctx, e, cause)
			if err := p.quarantineSources(ctx, e, []string{"device_code"}); err != nil {
				return nil, err
			}
			return nil, p.markDeadRefreshGrant(ctx, e, cause)
		}
	case p.provider == ProviderPortal:
		if synced := p.syncPortalFromAuthStore(ctx, e); synced.RefreshToken != e.RefreshToken {
			slog.Debug("Icebath Portal refresh failed but auth.json has newer tokens — adopting")
			updated, err := p.adopt(ctx, synced, markOK)
			if err != nil {
				return nil, err
			}
			p.syncDeviceCodeToAuthStore(ctx, updated)
			return updated, nil
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			slog.Debug("Icebath Portal refresh not attempted: auth store lock busy; not benching entry")
			return e, nil
		}
		if IsTerminalRefreshError(cause, ProviderPortal) {
			slog.Warn(fmt.Sprintf("Icebath Portal refresh token is terminally invalid (%v); clearing local token state. "+
				"Re-run 'icebath auth add portal' to sign in again.", cause))
			if q, ok := p.s.refresher(ProviderPortal).(Quarantiner); ok {
				q.Quarantine(ctx, *e.clone(), cause)
			}
			if err := p.quarantineSources(ctx, e, []string{PortalDeviceCodeSource, "manual:" + PortalDeviceCodeSource}); err != nil {
				return nil, err
			}
			return nil, p.markDeadRefreshGrant(ctx, e, cause)
		}
	case p.pluginRefresher() != nil:
		if synced := p.syncFromPoolStore(ctx, e); synced.RefreshToken != e.RefreshToken && strings.TrimSpace(synced.AccessToken.Reveal()) != "" {
			slog.Debug(fmt.Sprintf("%s refresh failed but the pool store has newer tokens — adopting", p.provider))
			return p.adopt(ctx, synced, markOK)
		}
		if isTerminalPluginRefreshError(cause) {
			slog.Warn(fmt.Sprintf("%s refresh token for %s is terminally invalid (%v); the credential leaves rotation. "+
				"Re-run 'icebath auth add %s' to sign in again.", p.provider, labelOf(e), cause, p.provider))
			return nil, p.markDeadRefreshGrant(ctx, e, cause)
		}
	}
	_, err := p.markExhausted(ctx, e, Failure{}, true)
	return nil, err
}

func errorCode(err error) string {
	var ae *Error
	if errors.As(err, &ae) && ae.Code != "" {
		return ae.Code
	}
	var oe *AnthropicOAuthError
	if errors.As(err, &oe) && oe.Code != "" {
		return oe.Code
	}
	return ""
}

func (p *Pool) markDeadRefreshGrant(ctx context.Context, e *Entry, cause error) error {
	ctx, unlock := p.lock(ctx)
	defer unlock()
	current := p.byID(e.ID)
	if current == nil || current.LastStatus == StatusDead {
		return nil
	}
	reason := errorCode(cause)
	if reason == "" {
		reason = "invalid_grant"
	}
	_, err := p.adopt(ctx, current, func(u *Entry) {
		stampStatus(u, StatusDead)
		u.LastErrorCode, u.LastErrorReason, u.LastErrorMessage = 0, reason, cause.Error()
	})
	return err
}

func (p *Pool) clearTerminalTokensState(ctx context.Context, e *Entry, cause error) {
	lctx, unlock, err := p.s.lock(ctx, p.s.authPath, LockTimeout)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to clear terminal %s OAuth state", p.provider), "error", err)
		return
	}
	defer unlock()
	store, err := p.s.load(p.s.authPath)
	if err != nil {
		return
	}
	state := p.s.loadState(store, p.provider)
	if state == nil {
		state = newDoc()
	}
	tokens := state.obj("tokens").clone()
	if tokens == nil {
		tokens = newDoc()
	}
	storeRefresh := stripped(tokens.value("refresh_token"))
	if storeRefresh != "" && storeRefresh != strings.TrimSpace(e.RefreshToken.Reveal()) {
		return
	}
	tokens.del("access_token")
	tokens.del("refresh_token")
	state.set("tokens", tokens)
	marker := newDoc()
	marker.set("provider", p.provider)
	code := errorCode(cause)
	if code == "" {
		code = "unknown"
	}
	marker.set("code", code)
	marker.set("message", cause.Error())
	marker.set("reason", "credential_pool_refresh_failure")
	marker.set("relogin_required", true)
	marker.set("at", isoStamp(time.Now()))
	state.set("last_auth_error", marker)
	if err := p.s.storeProviderState(lctx, store, p.provider, state, true); err != nil {
		slog.Debug(fmt.Sprintf("Failed to clear terminal %s OAuth state", p.provider), "error", err)
		return
	}
	if err := p.s.save(lctx, store, p.s.authPath); err != nil {
		slog.Debug(fmt.Sprintf("Failed to clear terminal %s OAuth state", p.provider), "error", err)
	}
}

func (p *Pool) codexQuotaRestoredUpstream(ctx context.Context, e *Entry) bool {
	if p.provider != ProviderCodex || e.LastStatus != StatusExhausted || !isCodexRateLimitShaped(e.LastErrorCode, e.LastErrorReason, e.LastErrorMessage) {
		return false
	}
	token := e.AccessToken.Reveal()
	if token == "" {
		return false
	}
	if fresh, ok := p.s.refreshExpiredCodexProbeToken(ctx, token, e.RefreshToken.Reveal()); ok {
		adopted, err := p.adopt(ctx, e, func(u *Entry) {
			u.AccessToken, u.RefreshToken = NewSecret(fresh.access), NewSecret(fresh.refresh)
			if fresh.lastRefresh != "" {
				u.LastRefresh = fresh.lastRefresh
			}
		})
		if err != nil {
			slog.Debug("Codex quota-restored probe failed", "error", err)
			return false
		}
		e = adopted
		p.syncDeviceCodeToAuthStore(ctx, e)
		if t := e.AccessToken.Reveal(); t != "" {
			token = t
		}
	}
	restored, known := p.s.probeCodexQuotaRestored(ctx, token, p.s.codexPoolRouteBaseURL(ctx, e.BaseURL))
	return known && restored
}

func (p *Pool) entryNeedsRefresh(e *Entry) bool {
	if e.AuthType != AuthTypeOAuth {
		return false
	}
	switch {
	case p.provider == ProviderAnthropic || p.pluginRefresher() != nil:
		return e.ExpiresAtMS != 0 && e.ExpiresAtMS <= time.Now().UnixMilli()+120_000
	case p.provider == ProviderCodex:
		return codexAccessTokenIsExpiring(e.AccessToken.Reveal(), CodexAccessTokenRefreshSkew)
	case p.provider == ProviderXAIOAuth:
		return xaiAccessTokenIsExpiring(e.AccessToken.Reveal(), xaiProactiveRefreshSkew(e.AccessToken.Reveal()))
	}
	return false
}

type builtinRefresher struct {
	s        *Store
	provider string
}

func (b builtinRefresher) Refresh(ctx context.Context, r RefreshRequest) (Entry, error) {
	ctx = b.s.bind(ctx)
	e := r.Entry
	switch b.provider {
	case ProviderAnthropic:
		tokens, err := b.s.refreshAnthropicPure(ctx, e.RefreshToken.Reveal(), strings.HasSuffix(e.Source, sourceIcebathPKCE))
		if err != nil {
			return Entry{}, err
		}
		e.AccessToken, e.RefreshToken, e.ExpiresAtMS = tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAtMS
	case ProviderCodex:
		pair, err := b.s.refreshCodexPure(ctx, e.RefreshToken.Reveal(), 20*time.Second)
		if err != nil {
			return Entry{}, err
		}
		e.AccessToken, e.RefreshToken, e.LastRefresh = NewSecret(pair.access), NewSecret(pair.refresh), pair.lastRefresh
	case ProviderXAIOAuth:
		tokens, lastRefresh, err := b.s.refreshXAIPure(ctx, e.RefreshToken.Reveal(), "", 20*time.Second)
		if err != nil {
			return Entry{}, err
		}
		e.AccessToken, e.RefreshToken, e.LastRefresh = NewSecret(tokens.str("access_token")), NewSecret(tokens.str("refresh_token")), lastRefresh
	}
	return e, nil
}

func (s *Store) Refresher(ctx context.Context, provider string) (Refresher, bool) {
	switch provider {
	case ProviderAnthropic, ProviderCodex, ProviderXAIOAuth:
		return builtinRefresher{s: s, provider: provider}, true
	}
	if r := s.refresher(provider); r != nil {
		return r, true
	}
	if spec, ok := s.dir.Spec(s.bind(ctx), provider); ok && spec.Refresh != nil {
		return spec.Refresh, true
	}
	return nil, false
}

func IsRefreshableOAuthProvider(provider string) bool {
	return provider == ProviderAnthropic || provider == ProviderPortal || isTokensSingleton(provider)
}
