package media

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Module probe of the managed interpreter, find_spec semantics with a failed probe reading as absent
var safeFindSpec = func(module string) bool { return pm.AnchorImportable(context.Background(), module) }

// Import time module flag, probed once per process on first read
func newFindSpecFlag(module string) func() bool {
	return sync.OnceValue(func() bool { return safeFindSpec(module) })
}

var hasFasterWhisper = newFindSpecFlag("faster_whisper")

// Serves the faster_whisper probe to the provider tool gateway status
func init() {
	provider.FasterWhisperImportable = func() bool { return hasFasterWhisper() }
}
