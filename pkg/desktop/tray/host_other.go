//go:build !linux

package tray

// WatchLinuxTrayHost exists only on Linux, the tray watches no host on other platforms
func WatchLinuxTrayHost(func()) (func(), error) {
	panic("the StatusNotifier tray host is watched only on Linux")
}
