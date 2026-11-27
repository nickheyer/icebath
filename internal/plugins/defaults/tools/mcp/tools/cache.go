package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Schema cache of a profile: the tools each server listed last, so a lazy server registers them without starting
func cachePath(dir string) string { return filepath.Join(dir, "cache", "mcp_schema_cache.json") }

var cacheMu sync.Mutex

type cacheEntry struct {
	Fingerprint string      `json:"fingerprint"`
	Tools       []cacheTool `json:"tools"`
	// Utility tools the server offered, by handler
	Utility []string `json:"utility_tools"`
	TTLMs   *float64 `json:"ttl_ms,omitempty"`
	// Seconds since the epoch it was written, for its TTL
	Written    float64 `json:"written_at,omitempty"`
	CacheScope string  `json:"cache_scope,omitempty"`
}

type cacheTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations struct {
		ReadOnly bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// Hash of the parts of a server's settings that decide what it serves
func fingerprint(s *spec) string {
	include, exclude := slices.Clone(s.Include), slices.Clone(s.Exclude)
	slices.Sort(include)
	slices.Sort(exclude)
	data, _ := json.Marshal(map[string]any{"command": s.Command, "args": s.Args, "url": s.URL, "transport": s.Transport, "tools_include": include,
		"tools_exclude": exclude})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func (p *pool) readCache() map[string]cacheEntry {
	entries := map[string]cacheEntry{}
	data, err := os.ReadFile(cachePath(p.dir()))
	if err != nil {
		return entries
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Debug("MCP schema cache unreadable", "path", cachePath(p.dir()), "error", err)
		return map[string]cacheEntry{}
	}
	return entries
}

// Tools and utility tools a lazy server listed last, false when the cache has none for its present settings or their time passed
func (p *pool) cached(s *spec) ([]*mcp.Tool, []string, bool) {
	cacheMu.Lock()
	entry, ok := p.readCache()[s.Name]
	cacheMu.Unlock()
	if !ok || entry.Fingerprint != fingerprint(s) {
		return nil, nil, false
	}
	if entry.TTLMs != nil && entry.Written > 0 && (float64(time.Now().UnixNano())/1e9-entry.Written)*1000 >= *entry.TTLMs {
		return nil, nil, false
	}
	out := make([]*mcp.Tool, 0, len(entry.Tools))
	for _, t := range entry.Tools {
		if t.Name == "" {
			continue
		}
		readOnly := t.Annotations.ReadOnly
		out = append(out, &mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &readOnly}})
	}
	return out, entry.Utility, true
}

// Keeps the tools a server listed, its filter applied, for its next lazy start
func (p *pool) writeCache(srv *server) {
	s := srv.currentSpec()
	srv.mu.Lock()
	listed, hints, result := srv.tools, srv.hints, srv.result
	srv.mu.Unlock()
	entry := cacheEntry{Fingerprint: fingerprint(s), Tools: []cacheTool{}, Utility: utilityKeys(s, result), TTLMs: hints.TTLMs, CacheScope: hints.CacheScope}
	if hints.TTLMs != nil {
		entry.Written = float64(time.Now().UnixNano()) / 1e9
	}
	for _, t := range listed {
		if t == nil || !s.allows(t.Name) {
			continue
		}
		cached := cacheTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
		cached.Annotations.ReadOnly = t.ReadOnly()
		entry.Tools = append(entry.Tools, cached)
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entries := p.readCache()
	entries[s.Name] = entry
	data, err := json.MarshalIndent(entries, "", "  ")
	if err == nil {
		err = fsx.Write(cachePath(p.dir()), data, 0o600)
	}
	if err != nil {
		slog.Debug("MCP schema cache write failed", "server", s.Name, "error", err)
	}
}
