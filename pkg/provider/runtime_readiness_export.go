package provider

import "context"

// A space led hint from the provider's profile for a missing credentials message, else empty
func MissingCredentialsSuffix(ctx context.Context, provider string) string {
	return missingCredentialsSuffix(ctx, provider)
}
