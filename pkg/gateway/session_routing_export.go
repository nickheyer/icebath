package gateway

import "context"

// The one store owning the routing index whatever scope is active, nil when it cannot be opened
func (s *SessionStore) RoutingDB(ctx context.Context) SessionStoreDB { return s.routingDB(ctx) }
