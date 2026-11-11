package tuiserver

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var (
	wakeMu             sync.Mutex
	wakeOwnerTransport tuigateway.Transport
	wakeOwnerSurface   string
)

// Logger the voice handlers write through
func voiceLogger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.methods_voice") }

// Gateway wake lease owner transport and the surface it named, read under the wake lock
func wakeOwnerSnapshot() (tuigateway.Transport, string) {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	return wakeOwnerTransport, wakeOwnerSurface
}

// Releases the wake lease and stops the detector when the transport is the current gateway owner
func releaseWakeForTransport(transport tuigateway.Transport) bool {
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
				voiceLogger().Debug(fmt.Sprintf("wake stop failed: %s", panicMessage(r)))
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
