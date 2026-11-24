package gateway

import (
	"context"
	"crypto/subtle"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Records consent to the profile's configured shell hooks for an accept hooks launch that reads the profile
func acceptLaunchHooks(ctx context.Context, policy *LocalSessionPolicy) error {
	request, err := loadsPolicyMap(policy.RequestJSON)
	if err != nil {
		return err
	}
	if !pyTruthy(request["accept_hooks"]) || policy.IgnoreUserConfig {
		return nil
	}
	return plugins.AcceptShellHooksForLaunch(ctx)
}

// Brings the owner's shell hooks in step with the profile's live config on a local session open
func refreshProfileHooks(ctx context.Context, policy *LocalSessionPolicy) {
	if policy == nil || policy.IgnoreUserConfig {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			pkgLog().Warn("shell-hook re-sync failed; keeping the registered hooks", "error", recovered)
		}
	}()
	plugins.SyncShellHooksFromConfig(ctx)
}

// Text equality that takes the same time wherever the texts differ
func digestEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Re-supplies a launch only key after an owner restart, only the same key a key launched session was bound with
func rebindLaunchKey(ctx context.Context, a *SessionAuthority, session SessionRef, apiKey any) error {
	key, isText := apiKey.(string)
	if !isText || strings.TrimSpace(key) == "" {
		return refuseText("api_key", "must be a non-empty string")
	}
	live, err := liveSession(a, session.SessionID)
	if err != nil {
		return err
	}
	if live.Source == nil || live.Source.Platform != PlatformLocal {
		return runtimeRefusal("admission_conflict")
	}
	receipt, err := a.DB.LocalReceipt(ctx, session.SessionID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return &PyError{Kind: "TypeError", Msg: "the launch key to rebind is missing"}
	}
	stored, hasPolicy := receipt.Get("policy")
	if !hasPolicy {
		return &pyKeyError{"policy"}
	}
	policy, err := restorePolicy(jsonutil.Plain(stored))
	if err != nil {
		return err
	}
	if policy.CredentialRef == nil {
		return runtimeRefusal("admission_conflict")
	}
	ref := *policy.CredentialRef
	a.mu.Lock()
	if a.launchSlots == nil {
		a.launchSlots = &launchSecretSlots{keys: map[string]string{}, configs: map[string]*RecoveredSecrets{}}
	}
	slots := a.launchSlots
	a.mu.Unlock()
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if held, ok := slots.keys[keySlot(ref)]; ok {
		if !digestEqual(held, key) {
			return runtimeRefusal("admission_conflict")
		}
		return nil
	}
	_, fingerprint, _ := strings.Cut(ref, "#")
	if fingerprint == "" || !digestEqual(fingerprint, provider.FingerprintSecretValue(key)) {
		return runtimeRefusal("admission_conflict")
	}
	slots.keys[keySlot(ref)] = key
	return nil
}
