package gateway

// LAN origin Web Access registered Nous sign in for at this port, empty when none matches
func RegisteredLANOrigin(h string, port int) string { return registeredLANOrigin(h, port) }

// Replaces the LAN address probe and returns the function that puts the previous one back
func SwapLANAddressProbe(probe func() string) (restore func()) {
	prev := wl.lanAddress
	wl.lanAddress = probe
	return func() { wl.lanAddress = prev }
}
