package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Default redaction of text under the redaction switch of the context profile, its vault secrets always scrubbed
func RedactSensitiveTextInProfile(ctx context.Context, text string) string {
	if text == "" {
		return text
	}
	scoped := &Env{Home: home.Dir(ctx)}
	if !redactEnabledFor(ctx, liveSecretScope) {
		return redactRegisteredVaultValues(scoped, text)
	}
	return RedactSensitiveTextWith(scoped, text, RedactOptions{Force: true})
}
