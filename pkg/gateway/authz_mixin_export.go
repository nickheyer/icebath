package gateway

import "context"

// Transport home ingress stamped on the source for authorization, empty when none was stamped
func AuthorizationProfileHomeOf(source *SessionSource) string { return source.authorizationProfileHome }

// Detached conversation route the source answers on, nil for a chat's live conversation
func DetachedRouteOf(source *SessionSource) *string { return detachedRouteOf(source) }

// Pins the identity of a source rebuilt from durable state from its persisted receiving bot
func RestoreIdentity(ctx context.Context, source *SessionSource, runner any, transportProfile *string) (*RoutingIdentity, error) {
	return restoreIdentity(ctx, source, runner, transportProfile)
}

// Phone, LID and JID aliases of a WhatsApp identifier that match the same principal
func ExpandWhatsAppAliases(ctx context.Context, identifier string) (map[string]bool, error) {
	return expandWhatsAppAliases(ctx, identifier)
}

// Authorization verdict of a local session source, nil for legacy and nonlocal routes
func AuthorizeLocalSource(runner any, source *SessionSource) (*bool, error) {
	return authorizeLocalSource(runner, source)
}
