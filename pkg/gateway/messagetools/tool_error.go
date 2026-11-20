package messagetools

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool error envelope with the bounded message and non ASCII text kept
func toolError(message string) string {
	out := pyjson.NewObject()
	out.Set("error", tools.BoundErrorText(message))
	text, err := pyjson.Dumps(out, pyjson.EnsureASCII(false))
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}
