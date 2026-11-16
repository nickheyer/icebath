package app

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Live work dock state of the classic CLI, subagents, processes, goal, loop and queued prompts
type subagentMonitor struct {
	cli        *CLI
	mu         sync.Mutex
	entries    []map[string]any
	processes  []map[string]any
	goal       string
	loop       string
	loopBadge  string
	loopPrompt string
	queued     []string
	selectedID any
	signature  *string
	lastPoll   float64
	app        *monitorApp
	opening    bool
	collapsed  bool
	detail     bool
	local      *localSubagents
}

// Subagent surface the dock and monitor read, the gateway session's view or this process's registry
type subagentBackend interface {
	subagentRows(busy bool) []map[string]any
	subagentTail(subagentID string, fetch bool) *string
	subagentControl(action, subagentID string, message any) (map[string]any, error)
}

// Subagent backend, the gateway session's view when its children run there, else this process's registry
func (m *subagentMonitor) subagents() subagentBackend {
	if view := gatewayProcessView(); view != nil {
		return view
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.local == nil {
		m.local = newLocalSubagents(m.cli)
	}
	return m.local
}

// Repaints the open monitor application on its loop
func (m *subagentMonitor) invalidate() {
	m.mu.Lock()
	app := m.app
	m.mu.Unlock()
	if app != nil {
		app.invalidate()
	}
}

// Origin the monitor's monotonic clock counts from
var monitorStart = time.Now()

// Steers or stops the target, killing a process for stop and refusing to steer one
func (m *subagentMonitor) control(action string, message any, target any) (map[string]any, error) {
	m.mu.Lock()
	if !pyrepr.Truthy(target) {
		target = m.selectedID
	}
	isProcess := false
	for _, r := range m.processes {
		if r["key"] == target {
			isProcess = true
		}
	}
	m.mu.Unlock()
	if isProcess {
		if action != "stop" {
			return map[string]any{"error": m.cli.t("cli.subagents.cannot_steer_process", nil)}, nil
		}
		return killDockProcess(m.cli.ctx, m.cli.processRegistry(), pyrepr.Str(target)), nil
	}
	return m.subagents().subagentControl(action, pyrepr.Str(target), message)
}

// Printable last 32KiB of a transcript file, the unavailable notice without a path or on a read error
func readTail(c *CLI, path any) string {
	p, _ := path.(string)
	if !pyrepr.Truthy(path) || p == "" {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	f, err := os.Open(p)
	if err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	defer f.Close()
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	if _, err := f.Seek(max(0, end-32768), io.SeekStart); err != nil {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	raw := make([]byte, 32768)
	n, err := io.ReadFull(f, raw)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return c.t("cli.subagents.transcript_unavailable", nil)
	}
	var b strings.Builder
	for _, r := range pystr.DecodeUTF8Replace(raw[:n]) {
		if pystr.IsPrintable(r) || r == '\n' || r == '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// This process's subagent registry behind the dock's subagent surface
type localSubagents struct {
	cli   *CLI
	mu    sync.Mutex
	paths map[string]any
}

// Registry surface of the CLI's in process agent with no transcript paths read yet
func newLocalSubagents(c *CLI) *localSubagents {
	return &localSubagents{cli: c, paths: map[string]any{}}
}

// Live child rows of the CLI agent's spawn tree with start time and latest tool
func (l *localSubagents) subagentRows(bool) []map[string]any {
	var entries []map[string]any
	if parent := l.cli.agentRef(); parent != nil {
		entries = agent.SubagentListRowsForSession(parent.SessionID())
	}
	live := map[string]map[string]any{}
	if len(entries) > 0 {
		for _, r := range agent.ListActiveSubagents() {
			live[pyrepr.Str(r["subagent_id"])] = r
		}
	}
	paths := map[string]any{}
	for _, row := range entries {
		record := live[pyrepr.Str(row["subagent_id"])]
		row["started_at"], row["last_tool"] = record["started_at"], pyrepr.Or(record["last_tool"], "")
		paths[pyrepr.Str(row["subagent_id"])] = row["live_transcript"]
	}
	l.mu.Lock()
	l.paths = paths
	l.mu.Unlock()
	return entries
}

// Transcript tail of a child from its live transcript path
func (l *localSubagents) subagentTail(subagentID string, _ bool) *string {
	l.mu.Lock()
	path := l.paths[subagentID]
	l.mu.Unlock()
	text := readTail(l.cli, path)
	return &text
}

// Runs a steer or stop control action against the CLI agent's spawn tree
func (l *localSubagents) subagentControl(action, subagentID string, message any) (map[string]any, error) {
	if parent := l.cli.agentRef(); parent != nil {
		return agent.SubagentControlForSession(action, subagentID, message, parent.SessionID())
	}
	return agent.SubagentControl(action, subagentID, message, nil)
}
