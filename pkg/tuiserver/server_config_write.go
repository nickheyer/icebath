package tuiserver

import (
	"sync"
)

// Raw config cache keyed on the resolved path and the backend cache version, a save write back source
var rawCfgCache struct {
	sync.Mutex
	cfg  map[string]any
	sig  []any
	path string
}

// Forgets the cached raw config so the next raw read returns what the backend holds
func dropCfgCache() {
	rawCfgCache.Lock()
	defer rawCfgCache.Unlock()
	rawCfgCache.cfg, rawCfgCache.sig, rawCfgCache.path = nil, nil, ""
}
