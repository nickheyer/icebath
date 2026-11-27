package tools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	webconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/web/config"
)

// Record of the web tool calls of this run, written to a JSON file in the profile's logs folder while the debug setting is on
type debug struct {
	mu    sync.Mutex
	id    string
	start string
	calls []map[string]any
}

func newDebug() *debug {
	var id [16]byte
	rand.Read(id[:])
	return &debug{id: hex.EncodeToString(id[:]), start: now()}
}

func now() string {
	return time.Now().Format("2006-01-02T15:04:05.000000")
}

// Adds the call and writes the whole record again
func (d *debug) log(cfg *config.Config, tool string, data map[string]any) {
	if !cfg.GetBool(webconfig.Debug) {
		return
	}
	call := map[string]any{"timestamp": now(), "tool_name": tool}
	for key, value := range data {
		call[key] = value
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, call)
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "logs")
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	record, err := json.MarshalIndent(map[string]any{"session_id": d.id, "start_time": d.start, "end_time": now(), "debug_enabled": true,
		"total_calls": len(d.calls), "tool_calls": d.calls}, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(dir, "web_tools_debug_"+d.id+".json"), record, 0o644)
}
