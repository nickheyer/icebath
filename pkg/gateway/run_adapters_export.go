package gateway

import (
	"context"
	"errors"
	"fmt"
)

// Identity pinned on the source, else one resolved against the primary home, nil when the route is unresolved
func CanonicalIdentity(ctx context.Context, source *SessionSource, runner TransportOwnerRunner, adapter PlatformAdapter, transportProfile, primaryHome string) (*RoutingIdentity, error) {
	if identity := IdentityOf(source); identity != nil {
		return identity, nil
	}
	identity, err := resolveIdentity(ctx, source, runner, adapter, transportProfile, primaryHome)
	var unresolved *IdentityUnresolved
	if errors.As(err, &unresolved) {
		pkgLog().Debug(fmt.Sprintf("identity unresolved: %s", unresolved.Reason))
		return nil, nil
	}
	return identity, err
}

// Adapter exposing its Python attributes by name, the getattr reads of the runner's credential and listener claims
type AttrAdapter interface {
	AdapterAttr(name string) any
}

// Pins the adapter that built a source
func SetTransportAdapterRef(source *SessionSource, adapter PlatformAdapter) {
	source.transportAdapterRef = adapter
}
