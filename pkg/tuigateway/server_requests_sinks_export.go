package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Request sinks bound now, for a caller that swaps them and binds the same set back
func CurrentSinks() (func(*pyjson.Object), func(string, string, map[string]any), func(string) bool, func(string) []Transport) {
	return currentSinks()
}
