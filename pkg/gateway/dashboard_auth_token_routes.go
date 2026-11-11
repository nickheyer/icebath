package gateway

import "sync"

var (
	tokenRoutesMu sync.Mutex
	tokenRoutes   = map[string]bool{}
)

// Marks an exact path as bearer token authable without making it public
func RegisterTokenRoute(path string) {
	tokenRoutesMu.Lock()
	defer tokenRoutesMu.Unlock()
	tokenRoutes[path] = true
}

// Whether the exact path was registered as token authable
func IsTokenRoute(path string) bool {
	tokenRoutesMu.Lock()
	defer tokenRoutesMu.Unlock()
	return tokenRoutes[path]
}

// Drops every registered token route
func ClearTokenRoutes() {
	tokenRoutesMu.Lock()
	defer tokenRoutesMu.Unlock()
	clear(tokenRoutes)
}
