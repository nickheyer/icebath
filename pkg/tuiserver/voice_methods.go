package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Wake word and voice method handlers by name, bound by the extras tier that carries voice and empty in a build without it
var voiceHandlers = map[string]Handler{}

var voiceRPCRegistry = &handlerRegistry{}

// Registers every wake word and voice method, refused with the build line when voice is absent
func init() {
	for _, name := range []string{"wake.start", "wake.stop", "wake.resume", "wake.status", "voice.toggle", "voice.record", "voice.playback"} {
		voiceRPCRegistry.method(name, func(ctx context.Context, rid any, params map[string]any) map[string]any {
			h := voiceHandlers[name]
			if h == nil {
				return rpcErr(rid, 4015, linktier.Missing("voice mode and the wake word", linktier.Extras), nil)
			}
			return h(ctx, rid, params)
		})
	}
	voiceRPCRegistry.install()
}
