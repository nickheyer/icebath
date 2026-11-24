package gatewayapi

import (
	"net/http"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Prefix of every session header the API server reads and writes
	headerPrefix = "X-Icebath-"
	// Prefix an older icebath peer sends and older replay records store
	legacyHeaderPrefix = "X-Hermes-"
)

// Copies each old prefix request header onto its icebath name unless the request already sends that name
func acceptLegacyRequestHeaders(h http.Header) {
	var keys []string
	for key := range h {
		if strings.HasPrefix(key, legacyHeaderPrefix) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		name := headerPrefix + strings.TrimPrefix(key, legacyHeaderPrefix)
		if _, set := h[name]; !set {
			h[name] = append([]string(nil), h[key]...)
		}
	}
}

// Headers of a settled replay record under their icebath names, old prefix names of older records renamed
func storedHeaders(record *jsonutil.Object) map[string]string {
	headers := map[string]string{}
	if record == nil {
		return headers
	}
	for _, key := range record.Keys() {
		value, _ := record.Get(key)
		if rest, legacy := strings.CutPrefix(key, legacyHeaderPrefix); legacy {
			key = headerPrefix + rest
		}
		headers[key] = anyutil.String(value)
	}
	return headers
}
