package gateway

import "context"

// Runs the standard setup form of a platform row for the platform plugins
func SetupStandardPlatform(ctx context.Context, p SetupPlatform) error {
	return setupStandardPlatform(ctx, p)
}

// Value of an env var through the gateway facade env reader at call time
func FacadeEnvValue(ctx context.Context, key string) (string, error) {
	return gatewayFacade.getEnvValue(ctx, key)
}
