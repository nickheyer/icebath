package tuiserver

import (
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var (
	wakeMu             sync.Mutex
	wakeOwnerTransport tuihost.Transport
	wakeOwnerSurface   string
)

// Gateway wake lease owner transport and the surface it named, read under the wake lock
func wakeOwnerSnapshot() (tuihost.Transport, string) {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	return wakeOwnerTransport, wakeOwnerSurface
}

// Releases the wake lease and stops the detector when the transport is the current gateway owner
func releaseWakeForTransport(transport tuihost.Transport) bool {
	wakeMu.Lock()
	if wakeOwnerTransport != transport {
		wakeMu.Unlock()
		return false
	}
	wakeOwnerTransport, wakeOwnerSurface = nil, ""
	wakeMu.Unlock()
	func() {
		defer func() {
			if r := recover(); r != nil {
				pkgLog().Debug(fmt.Sprintf("wake stop failed: %s", panicMessage(r)))
			}
		}()
		media.StopListening(transport)
	}()
	return true
}

// Releases the wake lease of whichever transport owns it, false when nobody does
func releaseGatewayWakeOwner() bool {
	owner, _ := wakeOwnerSnapshot()
	return owner != nil && releaseWakeForTransport(owner)
}
