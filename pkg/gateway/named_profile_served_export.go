package gateway

import "context"

// Whether a live default multiplexer serves the named profile, the current profile when the name is nil
func NamedProfileServedByRunningMultiplexerFor(ctx context.Context, profileName *string) (bool, error) {
	return namedProfileServedByRunningMultiplexer(ctx, profileName)
}
