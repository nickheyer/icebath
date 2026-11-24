package tuiserver

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Message of a raised or returned error, the type name when an exception carries no text
func errorMessage(err any) string {
	if e, ok := err.(error); ok {
		if msg := e.Error(); msg != "" {
			return msg
		}
		return pyrepr.TypeName(e)
	}
	return pyrepr.Str(err)
}
