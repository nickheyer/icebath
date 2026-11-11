package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Pairing store of the source profile when the runner serves one, else the global store
func (r *GatewayRunner) pairingStoreFor(source *gateway.SessionSource) *gateway.PairingStore {
	if source != nil && source.Profile != nil && *source.Profile != "" {
		if store, ok := r.pairingStores[*source.Profile]; ok {
			return store
		}
	}
	return r.pairingStore
}
