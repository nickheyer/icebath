package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	visionconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
	"github.com/nickheyer/icebath/internal/tools"
)

// Record of every analysis while the debug log is on, written whole after each one to a file of this run under the logs folder
type journal struct {
	mu    sync.Mutex
	id    string
	start time.Time
	calls []map[string]any
}

func (j *journal) record(call *tools.Call, tool string, entry map[string]any) {
	if !call.Env.Config.GetBool(visionconfig.Debug) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.id == "" {
		j.id, j.start = uuid.NewString(), time.Now()
	}
	entry["timestamp"], entry["tool_name"] = time.Now().Format(time.RFC3339Nano), tool
	j.calls = append(j.calls, entry)
	body, err := json.MarshalIndent(map[string]any{"session_id": j.id, "start_time": j.start.Format(time.RFC3339Nano), "end_time": time.Now().Format(time.RFC3339Nano),
		"debug_enabled": true, "total_calls": len(j.calls), "tool_calls": j.calls}, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Join(call.Env.Config.Dir(), "logs")
	if os.MkdirAll(dir, 0o700) == nil {
		os.WriteFile(filepath.Join(dir, "vision_tools_debug_"+j.id+".json"), body, 0o600)
	}
}
