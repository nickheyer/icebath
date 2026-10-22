package harness

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool/mcp"
	"github.com/nickheyer/icebath/pkg/tool/shell"
)

// Lines the journal keeps before dropping the oldest
const DefaultJournal = 2000

// Severity of a journal line
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// One line of the application log
type JournalLine struct {
	Time   time.Time
	Level  string
	Source string
	Text   string
}

// Application log of runs, tools, approvals, endpoints, servers, hooks and anything the app reports
type Journal struct {
	Max   int
	Clock func() time.Time
	mu    sync.Mutex
	lines []JournalLine
}

func NewJournal() *Journal { return &Journal{Clock: time.Now} }

// Appends a line, dropping the oldest past the limit
func (j *Journal) Add(level, source, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, JournalLine{Time: j.Clock(), Level: level, Source: source, Text: text})
	limit := j.Max
	if limit <= 0 {
		limit = DefaultJournal
	}
	if len(j.lines) > limit {
		j.lines = j.lines[len(j.lines)-limit:]
	}
}

// Lines oldest first
func (j *Journal) Lines() []JournalLine {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]JournalLine(nil), j.lines...)
}

// Drops every line
func (j *Journal) Clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = nil
}

// Sender that writes a line for every message worth one before passing the message on
func (j *Journal) Tap(send Sender) Sender {
	return func(msg any) {
		j.Record(msg)
		send(msg)
	}
}

// Writes the line a harness message stands for, ignoring streaming output
func (j *Journal) Record(msg any) {
	switch m := msg.(type) {
	case EventMsg:
		j.event(m)
	case RunDoneMsg:
		switch {
		case m.Err != nil:
			j.Add(LevelError, "run "+m.Run, "ended with "+m.Err.Error())
		default:
			j.Add(LevelInfo, "run "+m.Run, "finished in "+m.Duration.Round(time.Millisecond).String())
		}
	case ApprovalMsg:
		p := m.Pending
		j.Add(LevelWarn, "approval", fmt.Sprintf("%s asks to run %s %s", agentName(p.Agent), p.Request.Call.Name, clip(string(p.Request.Call.Args), 160)))
	case ApprovalDoneMsg:
		if m.Err != nil {
			j.Add(LevelWarn, "approval", fmt.Sprintf("request %d dropped, %v", m.ID, m.Err))
			return
		}
		j.Add(LevelInfo, "approval", fmt.Sprintf("request %d answered %s", m.ID, m.Verdict))
	case AttemptMsg:
		a := m.Attempt
		line := fmt.Sprintf("%s %s %d in %s", a.Method, a.URL, a.Status, a.Duration.Round(time.Millisecond))
		switch {
		case a.Err != nil:
			level := LevelError
			if a.Wait > 0 {
				level = LevelWarn
				line += ", retrying in " + a.Wait.Round(time.Millisecond).String()
			}
			j.Add(level, "endpoint "+a.Provider, line+", "+a.Err.Error())
		default:
			j.Add(LevelDebug, "endpoint "+a.Provider, line)
		}
	case FailoverMsg:
		j.Add(LevelWarn, "endpoint", fmt.Sprintf("model %d failed over, %v", m.From+1, m.Err))
	case MCPMsg:
		switch {
		case m.Err != nil:
			j.Add(LevelError, "mcp "+m.Server, string(m.State)+", "+m.Err.Error())
		case m.State == mcp.StateConnected && len(m.Tools) > 0:
			j.Add(LevelInfo, "mcp "+m.Server, fmt.Sprintf("connected with %d tools", len(m.Tools)))
		default:
			j.Add(LevelInfo, "mcp "+m.Server, string(m.State))
		}
	case MCPLogMsg:
		j.Add(serverLevel(m.Line.Level), "mcp "+m.Line.Server, m.Line.Text)
	case HookOutputMsg:
		j.Add(LevelInfo, "hook", m.Text)
	case JobMsg:
		info := m.Event.Job
		switch m.Event.Kind {
		case shell.JobStarted:
			j.Add(LevelInfo, "job "+info.ID, "started "+clip(info.Line, 160))
		case shell.JobFinished:
			switch {
			case info.Error != "":
				j.Add(LevelError, "job "+info.ID, info.Error)
			case info.TimedOut:
				j.Add(LevelWarn, "job "+info.ID, "timed out")
			default:
				j.Add(LevelInfo, "job "+info.ID, fmt.Sprintf("exited %d", info.ExitCode))
			}
		}
	case SavedMsg:
		if m.Err != nil {
			j.Add(LevelError, "session "+m.Session, "save failed, "+m.Err.Error())
		}
	case FactMsg:
		j.Add(LevelInfo, "facts", m.Change.Op+" "+m.Change.Fact.ID+" "+clip(m.Change.Fact.Title, 120))
	case MissionMsg:
		j.Add(LevelInfo, "mission "+m.Mission.ID, string(m.Op)+" "+m.Mission.Status)
	}
}

// Line of an agent event, runs, tools, steering, compaction and subagents
func (j *Journal) event(m EventMsg) {
	ev := m.Event
	src := "run " + m.Run
	switch ev.Type {
	case agent.EventRunStart:
		j.Add(LevelInfo, src, agentName(ev.Agent)+" started "+clip(protocol.Response{Content: ev.Parts}.Text(), 160))
	case agent.EventToolStart:
		j.Add(LevelDebug, src, ev.Call.Name+" "+clip(string(ev.Call.Args), 160))
	case agent.EventToolEnd:
		switch {
		case ev.Err != nil:
			j.Add(LevelWarn, src, ev.Call.Name+" refused, "+ev.Err.Error())
		case ev.Result.IsError:
			j.Add(LevelWarn, src, ev.Call.Name+" failed, "+clip(protocol.Response{Content: ev.Result.Content}.Text(), 200))
		default:
			j.Add(LevelInfo, src, ev.Call.Name+" done in "+ev.Duration.Round(time.Millisecond).String())
		}
	case agent.EventSteer:
		j.Add(LevelInfo, src, "steered "+clip(protocol.Response{Content: ev.Parts}.Text(), 160))
	case agent.EventCompactStart:
		j.Add(LevelInfo, src, "compacting the transcript")
	case agent.EventCompactEnd:
		j.Add(LevelInfo, src, "compacted the transcript")
	case agent.EventSubagentStart:
		j.Add(LevelInfo, src, "spawned "+ev.Call.Name)
	case agent.EventSubagentEnd:
		j.Add(LevelInfo, src, ev.Call.Name+" returned")
	case agent.EventInterrupt:
		j.Add(LevelWarn, src, "interrupted")
	case agent.EventHook:
		j.Add(LevelInfo, src, "hook "+ev.Text)
	}
}

// Journal level of an MCP logging level
func serverLevel(level string) string {
	switch level {
	case "debug":
		return LevelDebug
	case "info", "notice":
		return LevelInfo
	case "warning":
		return LevelWarn
	}
	return LevelError
}

func agentName(name string) string {
	if name == "" {
		return "root"
	}
	return name
}

// Text folded onto one line and cut to n runes
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
