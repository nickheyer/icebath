package messagetools

import (
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Tool error envelope with the bounded message and non ASCII text kept
func toolError(message string) string {
	out := jsonutil.NewObject()
	out.Set("error", tools.BoundErrorText(message))
	text, err := jsonutil.Encode(out)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return text
}
