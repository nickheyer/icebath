package gatewayrun

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Attributes an adapter may hold its credential under, in lookup order
var credentialAttrs = []string{"token", "bot_token", "_token", "api_token", "_bot_token", "_project_secret", "_app_id", "_client_id", "_bot_id"}

// Python attribute of an adapter by name, nil when the adapter has none
func adapterAttr(adapter any, name string) any {
	if holder, ok := adapter.(gateway.AttrAdapter); ok {
		return holder.AdapterAttr(name)
	}
	return nil
}

// Token of the adapter's platform config, nil without a config
func adapterConfigToken(adapter any) any {
	platform, ok := adapter.(gateway.PlatformAdapter)
	if !ok || platform.Base() == nil || platform.Base().Config == nil {
		return nil
	}
	return platform.Base().Config.Token
}

// Exclusive credential resource an adapter claims, nil when no credential is discoverable
func adapterCredentialClaim(platform gateway.Platform, adapter any) *[2]string {
	fingerprint := adapterCredentialFingerprint(adapter)
	if fingerprint == "" {
		return nil
	}
	return &[2]string{string(platform), fingerprint}
}

// Exclusive Photon sidecar bind and port claim, nil for other platforms or an unusable port
func adapterListenerClaim(platform gateway.Platform, adapter any) *[4]any {
	bind, ok := adapterAttr(adapter, "_sidecar_bind").(string)
	if platform != "photon" || !ok || strings.TrimSpace(bind) == "" {
		return nil
	}
	port, err := anyutil.ToInt(adapterAttr(adapter, "_sidecar_port"))
	if err != nil {
		return nil
	}
	return &[4]any{"listener", "photon", strings.ToLower(strings.TrimSpace(bind)), port}
}

// Salt of the credential fingerprint, and the one an older icebath hashed stored connectors with
const (
	credentialFingerprintSalt       = "icebath-mux:"
	legacyCredentialFingerprintSalt = "hermes-mux:"
)

// Salted log safe hash of an adapter's credential, empty when none is discoverable
func adapterCredentialFingerprint(adapter any) string {
	return saltedCredentialFingerprint(adapter, credentialFingerprintSalt)
}

// Hash of an adapter's first discoverable credential under the salt, empty when none is discoverable
func saltedCredentialFingerprint(adapter any, salt string) string {
	values := make([]any, 0, len(credentialAttrs)+1)
	for _, name := range credentialAttrs {
		values = append(values, adapterAttr(adapter, name))
	}
	values = append(values, adapterConfigToken(adapter))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		digest := sha256.Sum256([]byte(salt + strings.TrimSpace(text)))
		return hex.EncodeToString(digest[:])[:16]
	}
	return ""
}
