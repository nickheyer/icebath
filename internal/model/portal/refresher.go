package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
)

type poolRefresher struct {
	a *Account
}

func (p *poolRefresher) Refresh(ctx context.Context, r auth.RefreshRequest) (auth.Entry, error) {
	_, err := p.a.RuntimeCredentials(ctx, ResolveOptions{ForceRefresh: r.Force, StaleAccessToken: r.FailedKey})
	return r.Entry, err
}

func entryExtra(e *auth.Entry, key string) any {
	raw, ok := e.Extra(key)
	if !ok {
		return nil
	}
	value, err := parseValue(raw)
	if err != nil {
		return nil
	}
	return value
}

func (p *poolRefresher) Quarantine(ctx context.Context, e auth.Entry, cause error) {
	a := p.a
	ae, ok := authErrorOf(cause)
	if !ok {
		slog.Debug("Failed to clear terminal Icebath Portal OAuth state", "error", fmt.Sprintf("%T carries no auth error code", cause))
		return
	}
	err := a.store.Transact(a.bind(ctx), Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		state := a.txState(tx)
		if state.size() == 0 {
			state = pairs(
				"client_id", entryExtra(&e, "client_id"), "portal_base_url", entryExtra(&e, "portal_base_url"),
				"inference_base_url", nullable(e.InferenceBaseURL), "token_type", entryExtra(&e, "token_type"),
				"scope", entryExtra(&e, "scope"), "tls", entryExtra(&e, "tls"),
			)
		}
		storeRefresh := strings.TrimSpace(textualOrEmpty(state.get("refresh_token")))
		if storeRefresh != "" && storeRefresh != strings.TrimSpace(e.RefreshToken.Reveal()) {
			return nil
		}
		a.quarantineState(tx.Context(), state, ae, "credential_pool_refresh_failure")
		quarantinePoolEntries(tx, ae, "credential_pool_refresh_failure")
		return tx.SaveActive(state.raw())
	})
	if err != nil {
		slog.Debug("Failed to clear terminal Icebath Portal OAuth state", "error", err)
	}
}

func (a *Account) RuntimeKey(e *auth.Entry) (auth.Secret, bool) {
	scope := entryExtra(e, "scope")
	candidates := []struct {
		token     auth.Secret
		expiresAt string
	}{{e.AgentKey, e.AgentKeyExpiresAt}, {e.AccessToken, e.ExpiresAt}}
	for _, c := range candidates {
		token := strings.TrimSpace(c.token.Reveal())
		if token != "" && invokeJWTUsable(token, scope, c.expiresAt, auth.PortalInvokeJWTMinTTL) {
			return auth.NewSecret(token), true
		}
	}
	return auth.Secret{}, false
}

func (a *Account) StateSeedable(state json.RawMessage) bool {
	parsed, _ := parseObject(state)
	return !a.guestWithoutFreeTier(a.bind(context.Background()), parsed)
}

type Credential struct {
	Accounts func(home string) (*Account, error)
}

func (c Credential) AccessToken(ctx context.Context, home string) (string, error) {
	account, err := c.Accounts(home)
	if err != nil {
		return "", err
	}
	token, err := account.AccessToken(ctx, AccessTokenOptions{})
	if err != nil {
		return "", err
	}
	return token.Reveal(), nil
}

func (c Credential) NeedsLogin(err error) bool {
	ae, ok := authErrorOf(err)
	return ok && (ae.Relogin || ae.Code == "portal_auth_missing")
}
