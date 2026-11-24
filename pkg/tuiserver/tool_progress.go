package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Session event emitter of the callback sinks, swapped where a case records the emitted events
var emitEvent = tuihost.Emit
