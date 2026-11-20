package gateway

import "context"

// Installs the busy follow up mode alone, keeping the debounce window and hard cap
func (b *BasePlatformAdapter) SetBusyTextMode(mode string) {
	b.mu.Lock()
	b.busyTextMode = mode
	b.mu.Unlock()
}

// Installs the busy follow up debounce window and hard cap, keeping the mode
func (b *BasePlatformAdapter) SetBusyTextTiming(debounceSeconds, hardCapSeconds float64) {
	b.mu.Lock()
	b.busyTextDebounceSeconds, b.busyTextHardCapSeconds = debounceSeconds, hardCapSeconds
	b.mu.Unlock()
}

// Busy follow up mode, debounce window and hard cap the adapter applies
func (b *BasePlatformAdapter) BusyTextPolicy() (mode string, debounceSeconds, hardCapSeconds float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.busyTextMode, b.busyTextDebounceSeconds, b.busyTextHardCapSeconds
}

// Installed human delay range in milliseconds, nil keeping the configured env policy
func (b *BasePlatformAdapter) HumanDelayRangeMs() *[2]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.humanDelayRangeMs
}

// Records the multiplex profile a secondary adapter serves so prune paths namespace topic bindings
func (b *BasePlatformAdapter) SetProfileName(name string) {
	b.mu.Lock()
	b.profileName = name
	b.mu.Unlock()
}

// Multiplex profile recorded on a secondary adapter, empty on the primary
func (b *BasePlatformAdapter) ProfileName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.profileName
}

// Stamps the transport home a secondary bot's event authorizes against
func SetAuthorizationProfileHome(source *SessionSource, transportHome string) {
	source.authorizationProfileHome = transportHome
}

// Logs an inbound event whose route targets an unserved profile, refusing it in channel when that profile is parked
func DropRejectedRoute(ctx context.Context, adapter PlatformAdapter, source *SessionSource) {
	dropRejectedRoute(ctx, adapter, source)
}

// Copy of the per session pending message slots
func (b *BasePlatformAdapter) PendingMessagesSnapshot() map[string]*MessageEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]*MessageEvent, len(b.pendingMessages))
	for key, event := range b.pendingMessages {
		out[key] = event
	}
	return out
}

// Home of the routing index the store owns, empty before initialization
func (s *SessionStore) RoutingHome() string { return s.routingHome }
