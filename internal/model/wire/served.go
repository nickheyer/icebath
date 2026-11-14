package wire

import "net/http"

// Deployment a routing proxy named in the first non-empty served-model header of a reply, empty when it named none
func ServedModel(h http.Header) string {
	for _, name := range [...]string{"x-litellm-model-id", "x-litellm-model-api-base"} {
		if value := h.Get(name); value != "" {
			return trimSpace(value)
		}
	}
	return ""
}

// Requested and served model of a turn, the proxy's answer first and else the primary a fallback route replaced
func ResultModels(active, served, primary string, fellBack bool) (requested, servedBy string) {
	if served == "" && fellBack {
		if primary = trimSpace(primary); primary != "" && primary != active {
			return primary, active
		}
	}
	return active, served
}
