package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
)

// DebugSession records tool calls to a JSON log when the tool's env var is true
type DebugSession struct {
	toolName  string
	enabled   bool
	sessionID string
	logDir    string
	start     string
	mu        sync.Mutex
	calls     []map[string]any
}

// NewDebugSession enables recording when envVar is true
func NewDebugSession(env *Env, toolName, envVar string) *DebugSession {
	s := &DebugSession{toolName: toolName, enabled: strings.ToLower(os.Getenv(envVar)) == "true"}
	if env != nil && env.Home != "" {
		s.logDir = home.LogsDir(env.Home)
	} else {
		s.logDir = home.LogsDir(home.Process())
	}
	if s.enabled {
		s.sessionID = uuid.NewString()
		s.start = time.Now().Format(time.RFC3339Nano)
		os.MkdirAll(s.logDir, 0o755)
		envLog(env).Debug("debug mode enabled", "tool", toolName, "session", s.sessionID)
	}
	return s
}

// Active reports whether calls are being recorded
func (s *DebugSession) Active() bool { return s.enabled }

// LogCall appends a tool call entry to the in memory log
func (s *DebugSession) LogCall(callName string, callData map[string]any) {
	if !s.enabled {
		return
	}
	entry := map[string]any{"timestamp": time.Now().Format(time.RFC3339Nano), "tool_name": callName}
	for k, v := range callData {
		entry[k] = v
	}
	s.mu.Lock()
	s.calls = append(s.calls, entry)
	s.mu.Unlock()
}

// Save flushes the log to a JSON file in the logs directory
func (s *DebugSession) Save(env *Env) {
	if !s.enabled {
		return
	}
	s.mu.Lock()
	calls := append([]map[string]any{}, s.calls...)
	s.mu.Unlock()
	payload := map[string]any{"session_id": s.sessionID, "start_time": s.start, "end_time": time.Now().Format(time.RFC3339Nano),
		"debug_enabled": true, "total_calls": len(calls), "tool_calls": calls}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		envLog(env).Error("Error saving debug log", "tool", s.toolName, "error", err)
		return
	}
	path := filepath.Join(s.logDir, s.toolName+"_debug_"+s.sessionID+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		envLog(env).Error("Error saving debug log", "tool", s.toolName, "error", err)
		return
	}
	envLog(env).Debug("debug log saved", "tool", s.toolName, "path", path)
}
