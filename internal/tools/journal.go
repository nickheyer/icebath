package tools

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
)

// Record of the calls of a family of tools this run, written whole to a JSON file in the profile's logs folder after each call while the
// family's debug setting is on
type Journal struct {
	// Family the file is named for, such as image_tools
	Name  string
	mu    sync.Mutex
	id    string
	start string
	calls []map[string]any
}

func stamp() string {
	return time.Now().Format("2006-01-02T15:04:05.000000")
}

// Adds a call of the tool and writes the record again, when the setting under key is on
func (j *Journal) Record(cfg *config.Config, key, tool string, data map[string]any) {
	if !cfg.GetBool(key) {
		return
	}
	call := maps.Clone(data)
	if call == nil {
		call = map[string]any{}
	}
	call["timestamp"], call["tool_name"] = stamp(), tool
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.id == "" {
		j.id, j.start = uuid.NewString(), stamp()
	}
	j.calls = append(j.calls, call)
	record, err := json.MarshalIndent(map[string]any{"session_id": j.id, "start_time": j.start, "end_time": stamp(), "debug_enabled": true,
		"total_calls": len(j.calls), "tool_calls": j.calls}, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "logs")
	if os.MkdirAll(dir, 0o755) == nil {
		os.WriteFile(filepath.Join(dir, j.Name+"_debug_"+j.id+".json"), record, 0o644)
	}
}
