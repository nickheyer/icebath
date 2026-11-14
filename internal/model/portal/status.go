package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	SessionValid    = "valid"
	SessionTerminal = "terminal"
	SessionUnknown  = "unknown"

	statusCacheTTL = 15 * time.Second
)

type AuthStatus struct {
	LoggedIn                   bool
	PortalBaseURL              string
	InferenceBaseURL           string
	AccessExpiresAt            string
	AgentKeyExpiresAt          string
	HasRefreshToken            bool
	AccessToken                auth.Secret
	InferenceCredentialPresent bool
	CredentialSource           string
	Source                     string
	AccountTier                string
	FreeTier                   bool
	KeyID                      string
	Error                      string
	ReloginRequired            bool
	ErrorCode                  string
}

type statusMemo struct {
	at     time.Time
	key    string
	status AuthStatus
}

func (a *Account) snapshotPoolStatus(ctx context.Context) AuthStatus {
	pool, err := a.store.Pool(ctx, Provider)
	if err != nil || !pool.HasCredentials() {
		return AuthStatus{}
	}
	entries := pool.Entries()
	best := -1
	var bestKey [3]float64
	for i := range entries {
		e := &entries[i]
		if e.ExtraString("auth_method") == AnonAuthMethod {
			continue
		}
		key := [3]float64{stampEpoch(e.AgentKeyExpiresAt), stampEpoch(e.ExpiresAt), -float64(e.Priority)}
		if best < 0 || keyGreater(key, bestKey) {
			best, bestKey = i, key
		}
	}
	if best < 0 {
		return AuthStatus{}
	}
	entry := &entries[best]
	if entry.RuntimeAPIKey().Empty() {
		return AuthStatus{}
	}
	authType := strings.ToLower(strings.TrimSpace(entry.AuthType))
	isOAuth := !entry.AccessToken.Empty() && (strings.HasPrefix(authType, "oauth") || !entry.RefreshToken.Empty())
	inference := entry.InferenceBaseURL
	if inference == "" {
		inference = entry.RuntimeBaseURL(ctx)
	}
	if inference == "" {
		inference = entry.BaseURL
	}
	label := entry.Label
	if label == "" {
		label = "unknown"
	}
	status := AuthStatus{
		LoggedIn: isOAuth, InferenceBaseURL: inference, AccessExpiresAt: entry.ExpiresAt, AgentKeyExpiresAt: entry.AgentKeyExpiresAt,
		HasRefreshToken: !entry.RefreshToken.Empty(), InferenceCredentialPresent: true,
		CredentialSource: "pool:" + label, Source: "pool:" + label,
	}
	if isOAuth {
		status.AccessToken = entry.AccessToken
		status.PortalBaseURL = entry.ExtraString("portal_base_url")
		if status.PortalBaseURL == "" {
			status.PortalBaseURL = auth.DefaultPortalURL
		}
	}
	return status
}

func stampEpoch(value string) float64 {
	when, ok := auth.ParseISOTimestamp(value)
	if !ok {
		return 0
	}
	return epochOf(when)
}

func keyGreater(x, y [3]float64) bool {
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func statusFromState(state *object, loggedIn bool, source string) AuthStatus {
	accessToken := state.get("access_token")
	return AuthStatus{
		LoggedIn: loggedIn, PortalBaseURL: textualOrEmpty(state.get("portal_base_url")),
		InferenceBaseURL: textualOrEmpty(state.get("inference_base_url")),
		AccessExpiresAt:  textualOrEmpty(state.get("expires_at")), AgentKeyExpiresAt: textualOrEmpty(state.get("agent_key_expires_at")),
		HasRefreshToken: truthy(state.get("refresh_token")), AccessToken: auth.NewSecret(state.str("access_token")),
		InferenceCredentialPresent: truthy(accessToken) || truthy(state.get("agent_key")),
		CredentialSource:           "auth_store", Source: source,
		AccountTier: state.str("account_tier"), FreeTier: isGuestState(state),
	}
}

func firstText(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *Account) computeStatus(ctx context.Context) (AuthStatus, error) {
	state, err := a.currentState(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	if state.size() == 0 || a.guestWithoutFreeTier(ctx, state) {
		return a.snapshotPoolStatus(ctx), nil
	}
	base := statusFromState(state, truthy(state.get("access_token")), "auth_store")
	creds, err := a.RuntimeCredentials(WithBackground(ctx), ResolveOptions{})
	if err != nil {
		ae, ok := authErrorOf(err)
		if !ok {
			return AuthStatus{}, err
		}
		base.LoggedIn, base.Error, base.ReloginRequired, base.ErrorCode = false, err.Error(), ae.Relogin, ae.Code
		return base, nil
	}
	refreshed, err := a.currentState(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	if refreshed.size() == 0 {
		refreshed = state
	}
	source := creds.Source
	if source == "" {
		source = "portal"
	}
	base.LoggedIn = true
	base.PortalBaseURL = firstText(textualOrEmpty(refreshed.get("portal_base_url")), base.PortalBaseURL)
	base.InferenceBaseURL = firstText(creds.BaseURL, textualOrEmpty(refreshed.get("inference_base_url")), base.InferenceBaseURL)
	base.AccessExpiresAt = firstText(textualOrEmpty(refreshed.get("expires_at")), base.AccessExpiresAt)
	base.AgentKeyExpiresAt = firstText(creds.ExpiresAt, textualOrEmpty(refreshed.get("agent_key_expires_at")), base.AgentKeyExpiresAt)
	base.HasRefreshToken = truthy(refreshed.get("refresh_token"))
	base.InferenceCredentialPresent = true
	base.CredentialSource = "auth_store"
	base.Source = "runtime:" + source
	base.KeyID = creds.KeyID
	return base, nil
}

func (a *Account) authFileKey() string {
	path := a.store.Path()
	fi, err := os.Stat(path)
	if err != nil {
		return path
	}
	key := path + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(fi.Size(), 10)
	if time.Since(fi.ModTime()) < 2*time.Second {
		data, err := os.ReadFile(path)
		if err != nil {
			return path
		}
		sum := sha256.Sum256(data)
		key += "|" + hex.EncodeToString(sum[:])
	}
	return key
}

func (a *Account) InvalidateStatusCache() {
	a.mu.Lock()
	a.statusMemo = nil
	a.mu.Unlock()
}

func (a *Account) Status(ctx context.Context) (AuthStatus, error) {
	ctx = a.bind(ctx)
	key := a.authFileKey()
	a.mu.Lock()
	memo := a.statusMemo
	a.mu.Unlock()
	if memo != nil && memo.key == key && time.Since(memo.at) < statusCacheTTL {
		return memo.status, nil
	}
	started := time.Now()
	status, err := a.computeStatus(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	a.mu.Lock()
	a.statusMemo = &statusMemo{at: started, key: key, status: status}
	a.mu.Unlock()
	return status, nil
}

func terminalQuarantineMarker(state *object) *object {
	marker := state.obj("last_auth_error")
	if marker != nil && truthy(marker.get("relogin_required")) && !truthy(state.get("access_token")) && !truthy(state.get("refresh_token")) {
		return marker
	}
	return nil
}

func (a *Account) StatusLocal(ctx context.Context) AuthStatus {
	ctx = a.bind(ctx)
	state, err := a.currentState(ctx)
	if err != nil {
		state = nil
	}
	if state.size() == 0 || a.guestWithoutFreeTier(ctx, state) {
		return a.snapshotPoolStatus(ctx)
	}
	reason := stateInvokeJWTStatus(state, state.get("access_token"))
	marker := terminalQuarantineMarker(state)
	status := statusFromState(state, reason == "" || (truthy(state.get("refresh_token")) && marker == nil), "auth_store_local")
	if marker != nil {
		status.ReloginRequired = true
		status.ErrorCode = textualOrEmpty(marker.get("code"))
		status.Error = textualOrEmpty(marker.get("message"))
		if status.Error == "" {
			status.Error = "re-login required"
		}
	}
	return status
}

func (a *Account) SessionValidity(ctx context.Context) string {
	state, err := a.currentState(a.bind(ctx))
	if err != nil || state.size() == 0 {
		return SessionUnknown
	}
	if terminalQuarantineMarker(state) != nil {
		return SessionTerminal
	}
	if stateInvokeJWTStatus(state, state.get("access_token")) == "" {
		return SessionValid
	}
	return SessionUnknown
}

func (a *Account) storeStatus(ctx context.Context) (auth.Status, error) {
	status, err := a.Status(ctx)
	if err != nil {
		return auth.Status{}, err
	}
	return auth.Status{
		LoggedIn: status.LoggedIn, Provider: Provider, BaseURL: status.InferenceBaseURL, AuthStore: a.store.Path(),
		Source: status.Source, APIKey: status.AccessToken, Error: status.Error, ErrorCode: status.ErrorCode,
		Extra: map[string]string{
			"portal_base_url": status.PortalBaseURL, "access_expires_at": status.AccessExpiresAt,
			"agent_key_expires_at": status.AgentKeyExpiresAt, "has_refresh_token": strconv.FormatBool(status.HasRefreshToken),
			"inference_credential_present": strconv.FormatBool(status.InferenceCredentialPresent),
			"credential_source":            status.CredentialSource, "account_tier": status.AccountTier,
			"free_tier": strconv.FormatBool(status.FreeTier), "key_id": status.KeyID,
			"relogin_required": strconv.FormatBool(status.ReloginRequired),
		},
	}, nil
}
