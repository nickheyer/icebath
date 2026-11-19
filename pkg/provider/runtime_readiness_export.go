package provider

import (
	"context"
	"strings"
)

// A space led hint from the provider's profile for a missing credentials message, else empty
func MissingCredentialsSuffix(ctx context.Context, provider string) string {
	return missingCredentialsSuffix(ctx, provider)
}

// RuntimeHasCredentials reports a resolved runtime carrying a credential a request can use, a usable secret, a token source, an SDK or keyless placeholder or an external process command
func RuntimeHasCredentials(rt *Runtime) bool {
	if rt.TokenSource != nil || rt.Command != "" {
		return true
	}
	text := strings.TrimSpace(rt.APIKey)
	return text == "aws-sdk" || text == NoKeyPlaceholder || HasUsableSecret(text, 4)
}
