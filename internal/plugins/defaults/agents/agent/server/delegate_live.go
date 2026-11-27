package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Days the live transcripts of a delegation are kept unless a stored subagent session still names them
	liveRetention = 7 * 24 * time.Hour
	// Characters of each kind of line a transcript keeps
	liveAssistantMax = 600
	liveThinkingMax  = 300
	liveArgsMax      = 220
	liveResultMax    = 400
	liveKickoffMax   = 500
	// Characters of streamed reply buffered before they are written as a line
	liveStreamFlush = 4000
	liveTimeFormat  = "2006-01-02 15:04:05"
)

// Folder the live transcripts of the profile's delegations are kept in, one folder each
func liveRoot(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "delegation", "live")
}

// Append only log of one subagent's work that its parent and the user can follow while it runs; its first failed write silences it
type transcript struct {
	path string
	mu   sync.Mutex
	ok   bool
	// Streamed reply not yet written
	stream strings.Builder
}

// Single line of the text, cut to the limit with how much was left out
func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if n := utf8.RuneCountInString(text); n > limit {
		text = strs.Head(text, limit) + fmt.Sprintf(" …(+%d chars)", n-limit)
	}
	return text
}

// Transcript of one task with its header and kickoff line written, one with no path when it could not be made
func newTranscript(cfg *config.Config, delegation string, index int, goal, context string) *transcript {
	t := &transcript{}
	dir := filepath.Join(liveRoot(cfg), delegation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Debug(fmt.Sprintf("Live transcript init (%s task %d) failed: %s", delegation, index, err))
		return t
	}
	path := filepath.Join(dir, fmt.Sprintf("task-%d.log", index))
	goalLine := oneLine(goal, liveKickoffMax)
	header := "=== icebath subagent live transcript ===\n" +
		fmt.Sprintf("delegation: %s   task: %d\n", delegation, index) +
		fmt.Sprintf("goal: %s\n", childText(cfg, goalLine, true)) +
		fmt.Sprintf("started: %s\n", time.Now().Format(liveTimeFormat)) +
		"(append-only; streams while the subagent runs — tail -f me)\n" + strings.Repeat("=", 40) + "\n"
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		slog.Debug(fmt.Sprintf("Live transcript init (%s task %d) failed: %s", delegation, index, err))
		return t
	}
	t.path, t.ok = path, true
	kickoff := "kickoff: " + goalLine
	if strings.TrimSpace(context) != "" {
		kickoff += " | context: " + oneLine(context, liveKickoffMax)
	}
	t.event(cfg, "user", kickoff)
	return t
}

// Appends one timestamped line of the role, its secrets always masked
func (t *transcript) event(cfg *config.Config, role, text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ok {
		return
	}
	line := fmt.Sprintf("%s %-9s| %s\n", time.Now().Format("15:04:05"), role, childText(cfg, text, true))
	f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, err = f.WriteString(line)
		f.Close()
	}
	if err != nil {
		t.ok = false
		slog.Debug(fmt.Sprintf("Live transcript write failed (%s): %s", t.path, err))
	}
}

func (t *transcript) line(cfg *config.Config, role, text string, limit int) {
	if text = oneLine(text, limit); text != "" {
		t.event(cfg, role, text)
	}
}

// Buffers streamed reply text, written as one line once enough of it gathered or the next step comes
func (t *transcript) streamed(cfg *config.Config, delta string) {
	if t == nil || delta == "" {
		return
	}
	t.mu.Lock()
	t.stream.WriteString(delta)
	full := t.stream.Len() >= liveStreamFlush
	t.mu.Unlock()
	if full {
		t.flush(cfg)
	}
}

func (t *transcript) flush(cfg *config.Config) {
	if t == nil {
		return
	}
	t.mu.Lock()
	text := t.stream.String()
	t.stream.Reset()
	t.mu.Unlock()
	t.line(cfg, "assistant", text, liveAssistantMax)
}

func (t *transcript) thinking(cfg *config.Config, text string) {
	t.line(cfg, "think", text, liveThinkingMax)
}

func (t *transcript) toolStart(cfg *config.Config, name, args string) {
	t.flush(cfg)
	t.event(cfg, "tool", fmt.Sprintf("-> %s(%s)", cmp.Or(name, "?"), oneLine(args, liveArgsMax)))
}

func (t *transcript) toolResult(cfg *config.Config, name, result string, took time.Duration, failed bool) {
	status := "ok"
	if failed {
		status = "ERROR"
	}
	t.event(cfg, "result", fmt.Sprintf("%s %s %ss: %s", cmp.Or(name, "?"), status, strconv.FormatFloat(took.Seconds(), 'f', 1, 64),
		oneLine(result, liveResultMax)))
}

func (t *transcript) marker(cfg *config.Config, text string) {
	t.flush(cfg)
	t.event(cfg, "final", oneLine(text, liveAssistantMax))
}

func (t *transcript) started(cfg *config.Config, goal string) {
	t.event(cfg, "start", oneLine(goal, liveKickoffMax))
}

// Completion line: status, duration and summary
func (t *transcript) completed(cfg *config.Config, status string, duration float64, summary string) {
	parts := []string{"status=" + cmp.Or(status, "?"), fmt.Sprintf("duration=%ss", strconv.FormatFloat(duration, 'f', -1, 64))}
	if summary != "" {
		parts = append(parts, "summary: "+oneLine(summary, liveResultMax))
	}
	t.marker(cfg, strings.Join(parts, " "))
}

// End line carrying the exit reason and error the completion line lacks
func (t *transcript) finalize(cfg *config.Config, e *entry) {
	parts := []string{"end status=" + cmp.Or(e.Status, "?")}
	if e.ExitReason != "" {
		parts = append(parts, "exit_reason="+e.ExitReason)
	}
	if e.ExitReason == "max_iterations" {
		parts = append(parts, "(iteration budget exhausted)")
	}
	if e.Error != "" {
		parts = append(parts, "error: "+oneLine(e.Error, liveResultMax))
	}
	t.marker(cfg, strings.Join(parts, " "))
}

// Manifest of one delegation: every task's goal, its log and how it stands
type manifest struct {
	Delegation string         `json:"delegation_id"`
	Started    string         `json:"started"`
	Completed  string         `json:"completed,omitempty"`
	Tasks      int            `json:"task_count"`
	Model      string         `json:"model,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Entries    []manifestTask `json:"tasks"`
}

type manifestTask struct {
	Index      int    `json:"index"`
	Goal       string `json:"goal"`
	Log        string `json:"log,omitempty"`
	Status     string `json:"status"`
	ExitReason string `json:"exit_reason,omitempty"`
	Model      string `json:"model,omitempty"`
}

// Eight hex characters of a random id
func shortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Transcripts of a delegation's tasks, one each, and its manifest; old delegation folders no stored subagent session names pruned first.
// The delegation id, and its writers, those that could not be made having no path
func (a *Agent) transcripts(ctx context.Context, cfg *config.Config, tasks []*task, model, provider string) (string, []*transcript) {
	a.pruneLive(ctx, cfg)
	id := "deleg_" + shortID()
	writers := make([]*transcript, len(tasks))
	made := false
	for i, t := range tasks {
		writers[i] = newTranscript(cfg, id, i, t.goal, t.context)
		made = made || writers[i].path != ""
	}
	if !made {
		return "", writers
	}
	m := manifest{Delegation: id, Started: time.Now().Format(liveTimeFormat), Tasks: len(tasks), Model: model, Provider: provider}
	for i, t := range tasks {
		m.Entries = append(m.Entries, manifestTask{Index: i, Goal: childText(cfg, strs.Head(t.goal, 500), true), Log: writers[i].path, Status: "running"})
	}
	writeManifest(cfg, m)
	return id, writers
}

func manifestPath(cfg *config.Config, delegation string) string {
	return filepath.Join(liveRoot(cfg), delegation, "manifest.json")
}

func writeManifest(cfg *config.Config, m manifest) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = fsx.Write(manifestPath(cfg, m.Delegation), data, 0o600)
	}
	if err != nil {
		slog.Debug("Live transcript manifest write failed: " + err.Error())
	}
}

// Sets the status of each task the results name, stamping the delegation completed unless it settles early
func updateManifest(cfg *config.Config, delegation string, results []*entry, completed bool) {
	if delegation == "" {
		return
	}
	data, err := os.ReadFile(manifestPath(cfg, delegation))
	if err != nil {
		slog.Debug("Live transcript manifest update failed: " + err.Error())
		return
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Debug("Live transcript manifest update failed: " + err.Error())
		return
	}
	for _, e := range results {
		for i := range m.Entries {
			if m.Entries[i].Index != e.TaskIndex {
				continue
			}
			m.Entries[i].Status = cmp.Or(e.Status, m.Entries[i].Status)
			if e.ExitReason != "" {
				m.Entries[i].ExitReason = e.ExitReason
			}
			if e.Model != "" {
				m.Entries[i].Model = e.Model
			}
		}
	}
	if m.Model == "" {
		for _, t := range m.Entries {
			if t.Model != "" {
				m.Model = t.Model
				break
			}
		}
	}
	if completed {
		m.Completed = time.Now().Format(liveTimeFormat)
	}
	writeManifest(cfg, m)
}

// Removes the delegation folders older than the retention that no stored subagent session still names
func (a *Agent) pruneLive(ctx context.Context, cfg *config.Config) {
	root := liveRoot(cfg)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-liveRetention)
	var kept map[string]bool
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !info.ModTime().Before(cutoff) {
			continue
		}
		if kept == nil {
			if kept, err = a.namedTranscripts(ctx); err != nil {
				return
			}
		}
		if kept[dir] {
			continue
		}
		os.RemoveAll(dir)
	}
}

// Delegation folders the stored subagent sessions name as theirs
func (a *Agent) namedTranscripts(ctx context.Context) (map[string]bool, error) {
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	children, _, err := store.List(ctx, subagentFilter())
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range children {
		if path := delegatedOf(s).transcript; path != "" {
			out[filepath.Dir(path)] = true
		}
	}
	return out, nil
}
