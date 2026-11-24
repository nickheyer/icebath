package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Local wall clock in datetime.isoformat form
func debugNow() string { return time.Now().Format(timeutil.RFC3339Micro) }

// DebugSession records tool calls to a JSON log when the tool's env var is true
type DebugSession struct {
	toolName  string
	enabled   bool
	sessionID string
	logDir    string
	start     string
	mu        sync.Mutex
	calls     []*jsonutil.Object
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
		s.start = debugNow()
		os.MkdirAll(s.logDir, 0o755)
		envLog(env).Debug("debug mode enabled", "tool", toolName, "session", s.sessionID)
	}
	return s
}

// LogCall appends a tool call entry to the in memory log
func (s *DebugSession) LogCall(callName string, callData map[string]any) {
	if !s.enabled {
		return
	}
	entry := jsonutil.NewObject()
	entry.Set("timestamp", debugNow())
	entry.Set("tool_name", callName)
	for _, k := range sortedKeys(callData) {
		entry.Set(k, callData[k])
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
	calls := append([]*jsonutil.Object{}, s.calls...)
	s.mu.Unlock()
	payload := jsonutil.NewObject()
	payload.Set("session_id", s.sessionID)
	payload.Set("start_time", s.start)
	payload.Set("end_time", debugNow())
	payload.Set("debug_enabled", true)
	payload.Set("total_calls", len(calls))
	payload.Set("tool_calls", calls)
	body, err := jsonutil.Encode(payload, jsonutil.Indent(2))
	if err != nil {
		envLog(env).Error("Error saving debug log", "tool", s.toolName, "error", err)
		return
	}
	path := filepath.Join(s.logDir, s.toolName+"_debug_"+s.sessionID+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		envLog(env).Error("Error saving debug log", "tool", s.toolName, "error", err)
		return
	}
	envLog(env).Debug("debug log saved", "tool", s.toolName, "path", path)
}
