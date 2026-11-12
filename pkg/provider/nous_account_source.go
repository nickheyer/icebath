package provider

import (
	"errors"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Nous Portal account answers the tool layer reads, each over the tool env's profile home
type nousAccountSource struct{}

// Account source the tool layer installs as tools.NousAccount
func NousAccountSource() tools.NousAccountSource { return nousAccountSource{} }

// Paid Portal access or a live free tool pool for a logged in account, false on any failure
func (nousAccountSource) ManagedToolsEnabled(env *tools.Env, forceFresh bool) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = false
		}
	}()
	info := getNousPortalAccountInfoIn(toolsPoolEnv(env), forceFresh, 60)
	return info.LoggedIn && info.ToolGatewayEntitled()
}

// Account aware entitlement guidance for a capability, empty when the account read fails
func (nousAccountSource) UnavailableMessage(env *tools.Env, capability string, forceFresh bool) (message string) {
	defer func() {
		if recover() != nil {
			message = ""
		}
	}()
	return FormatNousPortalEntitlementMessage(getNousPortalAccountInfoIn(toolsPoolEnv(env), forceFresh, 60), capability, true)
}

// Cached connectors bearer of the profile with no expiry check and no network
func (nousAccountSource) PeekAccessToken(env *tools.Env) string {
	return PeekNousAccessToken(toolsPoolEnv(env))
}

// Live connectors bearer, refreshed and a retired guest replaced once, raising a failed retirement write
func (nousAccountSource) ReadAccessToken(env *tools.Env) string {
	token, err := ReadNousAccessToken(toolsPoolEnv(env))
	if err != nil {
		panic(err)
	}
	return token
}

// Whether auth remove recorded the source as suppressed for the provider, false when the store is unreadable
func (nousAccountSource) SourceSuppressed(env *tools.Env, providerID, source string) bool {
	pe := toolsPoolEnv(env)
	if pe.Store == nil {
		return false
	}
	return pe.Store.IsSourceSuppressed(providerID, source)
}

// Runtime key else access token of the first pooled credential of the pool key, empty when none
func (nousAccountSource) PoolAPIKey(env *tools.Env, providerID string) string {
	pool := LoadPool(toolsPoolEnv(env), providerID)
	if pool == nil || !pool.HasCredentials() {
		return ""
	}
	entry, found := pool.Peek()
	if !found {
		return ""
	}
	if key := strings.TrimSpace(entry.RuntimeAPIKey()); key != "" {
		return key
	}
	return strings.TrimSpace(entry.AccessToken)
}

// Whether the profile holds a visible Nous identity and whether that identity is the guest tier
func (nousAccountSource) IdentityState(env *tools.Env) (bool, bool) {
	state := readNousProviderState(toolsPoolEnv(env))
	return state != nil, isGuestState(state)
}

// Whether nous.guest leaves guest identities on for the profile
func (nousAccountSource) GuestAllowed(env *tools.Env) bool {
	return guestAllowed(toolsPoolEnv(env))
}

// Latest guest mint failure of the profile, false when none is recorded
func (nousAccountSource) LastMintFailure(env *tools.Env) (tools.MintFailure, bool) {
	payload := lastMintFailure(toolsPoolEnv(env))
	if payload == nil {
		return tools.MintFailure{}, false
	}
	retryAfter, _ := payload["retry_after"].(int)
	code, _ := payload["error_code"].(string)
	retryable, _ := payload["retryable"].(bool)
	return tools.MintFailure{ErrorCode: code, RetryAfter: float64(retryAfter), Retryable: retryable}, true
}

// Free tier failure sentence for a code with the remaining wait, a minute when none is given
func (nousAccountSource) AnonFailureCopy(_ *tools.Env, code string, retryAfter float64) string {
	return anonFailureCopy(code, time.Duration(retryAfter*float64(time.Second)), retryAfter != 0)
}

// Errors returned when an explicit provision ends with no identity, the disabled or cooling down outcome
var errNoPortalIdentity = errors.New("no Nous identity was provisioned")

// Provisions the profile's Nous identity, a coded refusal as an auth hook error and any other failure raised
func (nousAccountSource) EnsurePortalIdentity(env *tools.Env) error {
	identity, err := EnsurePortalIdentity(toolsPoolEnv(env), PortalIdentityOptions{Explicit: true})
	if err != nil {
		if auth := anonAuthErrorOf(err); auth != nil {
			return &tools.AuthHookError{Msg: auth.Error(), Code: auth.Code}
		}
		panic(err)
	}
	if identity == nil {
		return errNoPortalIdentity
	}
	return nil
}

// Whether the Portal enabled connectors for the logged in account through the managed tools claim
func (nousAccountSource) ManagedToolsRolledOut(env *tools.Env) bool {
	info := getNousPortalAccountInfoIn(toolsPoolEnv(env), false, 60)
	return info != nil && info.LoggedIn && info.ManagedToolsRolledOut()
}
