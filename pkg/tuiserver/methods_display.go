package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

const displayErr = 5300

var (
	displayRegistry      = &handlerRegistry{}
	broadcastGlobalEvent = func(event string, payload map[string]any) { tuihost.BroadcastGlobalEvent(event, payload) }
)

// Bot desktop parts of the display card, bound by the extras tier that carries the bot desktop and nil in a build without it
var (
	displayHandlers   map[string]func(ctx context.Context, rid any, params map[string]any) map[string]any
	leaseWatcherStart func()
)

// Registers the display card methods, each refused with the build line when the bot desktop is absent
func init() {
	for _, method := range []string{"display.status", "display.start", "display.stop", "display.switchSandboxImage", "display.install"} {
		displayRegistry.scopedMethod(method, func(ctx context.Context, rid any, params map[string]any) map[string]any {
			if handler := displayHandlers[method]; handler != nil {
				return handler(ctx, rid, params)
			}
			return rpcErr(rid, displayErr, linktier.Missing("the bot desktop", linktier.Extras), nil)
		})
	}
	displayRegistry.install()
}

// Starts the bot desktop lease watcher once per process, nothing in a build without the bot desktop
func ensureLeaseWatcher() {
	if leaseWatcherStart != nil {
		leaseWatcherStart()
	}
}
