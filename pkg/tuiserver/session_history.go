package tuiserver

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Message of a raised or returned error, the type name when an exception carries no text
func errorMessage(err any) string {
	if e, ok := err.(error); ok {
		if msg := e.Error(); msg != "" {
			return msg
		}
		return anyutil.Kind(e)
	}
	return anyutil.String(err)
}
