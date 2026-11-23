package tuiserver

import (
	"regexp"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Server minted session key shape, a timestamp plus six hex characters
var mintedSessionKeyRE = regexp.MustCompile(`^\d{8}_\d{6}_[0-9a-f]{6}$`)

// Fields of a deferred live record beyond the common ones
type deferredRecordOptions struct {
	cols                   int
	cwd                    string
	history                []any
	lease                  *activesessions.Lease
	source                 string
	closeOnDisconnect      bool
	displayHistoryPrefix   []any
	profileHome            string
	lazy                   bool
	modelOverride          any
	resumeRuntimeOverrides map[string]any
	todoState              any
	explicitCwd            bool
}

// Options of a live session payload
type livePayloadOptions struct {
	cols         *int
	touch        bool
	transport    tuihost.Transport
	omitMessages bool
	inlineImages bool
}
