package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const auxManagedLocalStateTTL = 15 * time.Second

var (
	auxManagedLocalMu      sync.Mutex
	auxManagedLocalCacheAt time.Time
	auxManagedLocalCached  string
	auxNow                 = time.Now
)

// Lowercased host and port of the managed llama-server from the supervisor state file, cached briefly
func auxManagedLocalNetloc() string {
	auxManagedLocalMu.Lock()
	defer auxManagedLocalMu.Unlock()
	now := auxNow()
	if !auxManagedLocalCacheAt.IsZero() && now.Sub(auxManagedLocalCacheAt) < auxManagedLocalStateTTL {
		return auxManagedLocalCached
	}
	auxManagedLocalCacheAt, auxManagedLocalCached = now, auxReadManagedLocalNetloc()
	return auxManagedLocalCached
}

// ManagedLocalNetloc is the cached host and port of the managed llama-server, empty without one
func ManagedLocalNetloc() string { return auxManagedLocalNetloc() }

// Netloc of the state file base_url, empty for a missing file, bad JSON, or a non object document
func auxReadManagedLocalNetloc() string {
	raw, err := os.ReadFile(localRuntimeStatePath())
	if err != nil {
		return ""
	}
	var doc any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &doc) != nil {
		return ""
	}
	if !auxTruthy(doc) {
		return ""
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return ""
	}
	base := ""
	if v, has := obj["base_url"]; has {
		if s, isStr := v.(string); isStr {
			base = s
		} else {
			base = auxPyStr(v)
		}
	}
	parts, err := pyURLParse(base, "")
	if err != nil {
		return ""
	}
	return strings.ToLower(parts.netloc)
}

// Python str of a decoded JSON scalar
func auxPyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

// True when the base URL targets the llama-server this install manages
func auxIsManagedLocalEndpoint(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	managed := auxManagedLocalNetloc()
	if managed == "" {
		return false
	}
	parts, err := pyURLParse(baseURL, "")
	if err != nil {
		return false
	}
	return strings.ToLower(parts.netloc) == managed
}

// IsManagedLocalEndpoint reports whether a base URL targets the llama-server this install manages
func IsManagedLocalEndpoint(baseURL string) bool { return auxIsManagedLocalEndpoint(baseURL) }

// Drops the cached managed endpoint so the next read consults the state file
func auxResetManagedLocalCache() {
	auxManagedLocalMu.Lock()
	auxManagedLocalCacheAt, auxManagedLocalCached = time.Time{}, ""
	auxManagedLocalMu.Unlock()
}
