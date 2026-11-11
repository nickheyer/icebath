package gateway

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Path of the state database the launch secret references bind to
func (a *SessionAuthority) launchOwnerDBPath() string {
	if a.DB == nil {
		return ""
	}
	return a.DB.Path()
}

// Profile the launch secret references bind to
func (a *SessionAuthority) launchOwnerProfileID() string { return a.ProfileID }

// Launch keys and frozen config secrets of this authority lifetime, nil until a session bound one
func (a *SessionAuthority) launchSecretSlots() *launchSecretSlots {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.launchSlots
}

// Binds a launch key and frozen config secrets in memory only, the policy keeping their references
func bindLaunchKey(ctx context.Context, a *SessionAuthority, sid string, policy *LocalSessionPolicy, apiKey *string, configSecrets *RecoveredSecrets) (*LocalSessionPolicy, error) {
	return bindLaunchKeyFor(ctx, a, sid, policy, apiKey, configSecrets, false, nil)
}

// Binds launch secrets, an owner expanded key of the same create request taking the key its slot already holds
func bindLaunchKeyFor(ctx context.Context, a *SessionAuthority, sid string, policy *LocalSessionPolicy, apiKey *string, configSecrets *RecoveredSecrets, ownerKey bool, clientRequest *pyjson.Object) (*LocalSessionPolicy, error) {
	secrets := &RecoveredSecrets{}
	var servers any
	if configSecrets != nil {
		for i, path := range configSecrets.paths {
			if pathEqual(path, editorMCPPrivateKey) {
				servers = configSecrets.values[i]
				continue
			}
			secrets.Set(path, configSecrets.values[i])
		}
	}
	policy, err := bindEditorMCP(ctx, a, sid, policy, servers)
	if err != nil {
		return nil, err
	}
	bound := *policy
	if apiKey == nil && secrets.Len() == 0 {
		bound.CredentialRef, bound.ConfigSecretRef = nil, nil
		return &bound, nil
	}
	a.mu.Lock()
	if a.launchSlots == nil {
		a.launchSlots = &launchSecretSlots{keys: map[string]string{}, configs: map[string]*RecoveredSecrets{}}
	}
	if a.localOwnerKeyRequests == nil {
		a.localOwnerKeyRequests = map[string]*pyjson.Object{}
	}
	slots, owners := a.launchSlots, a.localOwnerKeyRequests
	a.mu.Unlock()
	ref := fmt.Sprintf("%s:%d:%s", a.InstanceID, a.Epoch, sid)
	slots.mu.Lock()
	defer slots.mu.Unlock()
	old, held := slots.keys[ref]
	if saved, owned := owners[ref]; ownerKey && apiKey != nil && held && owned {
		if clientRequest == nil {
			clientRequest = pyjson.NewObject()
		}
		same, err := state.SameCreateRetry(saved, policy.RequestJSON, clientRequest)
		if err != nil {
			return nil, err
		}
		if same {
			apiKey = &old
		}
	}
	if apiKey != nil && held && subtle.ConstantTimeCompare([]byte(old), []byte(*apiKey)) != 1 {
		return nil, runtimeRefusal("admission_conflict")
	}
	configRef := ref
	if secrets.Len() > 0 && a.DB != nil {
		if configRef, err = configReference(ctx, a, sid, policy, secrets); err != nil {
			return nil, err
		}
	}
	if existing, ok := slots.configs[configRef]; ok && !existing.equal(secrets) {
		return nil, runtimeRefusal("admission_conflict")
	}
	if secrets.Len() > 0 {
		slots.configs[configRef] = secrets.without(nil)
	}
	if apiKey != nil && !held && ownerKey {
		owners[ref] = pyjson.ObjectOf("request_json", policy.RequestJSON, "client_env_json", optStrAny(policy.ClientEnvJSON),
			"client_env_rules_json", optStrAny(policy.ClientEnvRulesJSON))
	}
	bound.CredentialRef, bound.ConfigSecretRef = nil, nil
	if apiKey != nil {
		slots.keys[ref] = *apiKey
		credential := ref + "#" + provider.FingerprintSecretValue(*apiKey)
		bound.CredentialRef = &credential
	}
	if secrets.Len() > 0 {
		bound.ConfigSecretRef = &configRef
	}
	return &bound, nil
}
