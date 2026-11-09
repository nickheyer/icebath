package provider

import (
	"context"
	"errors"
	"os"
)

// Scoped credential reader behind the OpenRouter probe, swapped by tests that make the scope fail
var openRouterGetSecret = GetSecret

// True when OPENROUTER_API_KEY resolves through the installed scope, the plain env read only for an unscoped probe
func CheckOpenRouterAPIKey(ctx context.Context) (bool, error) {
	value, _, err := openRouterGetSecret(ctx, "OPENROUTER_API_KEY")
	if err != nil {
		var unscoped *UnscopedSecretError
		if errors.As(err, &unscoped) {
			return os.Getenv("OPENROUTER_API_KEY") != "", nil
		}
		return false, err
	}
	return value != "", nil
}
