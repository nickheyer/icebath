package terminal

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

const (
	// Receipts of ended jobs are kept this long, and at most this many per profile
	receiptAge = 7 * 24 * time.Hour
	receiptMax = 64
)

// Receipt of an ended job, kept in the profile so a resumed session can still read what its job did
type receipt struct {
	ID       string    `json:"id"`
	Command  string    `json:"command"`
	Cwd      string    `json:"cwd,omitempty"`
	Session  string    `json:"session"`
	Started  time.Time `json:"started_at"`
	Ended    time.Time `json:"exited_at"`
	ExitCode *int      `json:"exit_code"`
	Reason   string    `json:"completion_reason"`
	Source   string    `json:"termination_source,omitempty"`
	Notify   bool      `json:"notify_on_complete,omitempty"`
	Output   string    `json:"output"`
}

var receiptName = regexp.MustCompile(`^proc_\w+$`)

func receiptsDir(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "logs", "process-results")
}

// Writes the receipt of an ended job
func (r *Jobs) saveReceipt(j *Job) {
	if j.Config == nil {
		return
	}
	j.mu.Lock()
	record := receipt{ID: j.ID, Command: RedactCommand(j.Config, j.Command, true), Cwd: j.Cwd, Session: j.Session, Started: j.StartedAt,
		Ended: j.exitedAt, ExitCode: j.code, Reason: j.reason, Source: j.source, Notify: j.notify, Output: term.StripANSI(j.output)}
	j.mu.Unlock()
	record.Output = Redact(j.Config, record.Output, j.Command, true)
	dir := receiptsDir(j.Config)
	data, err := json.Marshal(record)
	if err == nil {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			err = fsx.Write(filepath.Join(dir, j.ID+".json"), data, 0o600)
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not retain completed process result %s", j.ID), "error", err)
		return
	}
	receiptPaths(dir)
}

// Paths of the receipts kept, newest first, those past their age or count removed
func receiptPaths(dir string) []string {
	type kept struct {
		modified time.Time
		path     string
	}
	var receipts []kept
	matches, _ := filepath.Glob(filepath.Join(dir, "proc_*.json"))
	for _, path := range matches {
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if time.Since(st.ModTime()) > receiptAge {
			os.Remove(path)
			continue
		}
		receipts = append(receipts, kept{st.ModTime(), path})
	}
	slices.SortFunc(receipts, func(a, b kept) int {
		if c := b.modified.Compare(a.modified); c != 0 {
			return c
		}
		return strings.Compare(filepath.Base(b.path), filepath.Base(a.path))
	})
	var out []string
	for i, receipt := range receipts {
		if i >= receiptMax {
			os.Remove(receipt.path)
			continue
		}
		out = append(out, receipt.path)
	}
	return out
}

// Ended jobs of the session read back from their receipts, newest first, whose id starts with prefix
func (r *Jobs) receiptList(cfg *config.Config, session, prefix string) []*Job {
	if session == "" {
		return nil
	}
	var jobs []*Job
	for _, path := range receiptPaths(receiptsDir(cfg)) {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if !strings.HasPrefix(id, prefix) || !receiptName.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var record receipt
		if err := json.Unmarshal(data, &record); err != nil || record.ID != id || record.Session != session {
			continue
		}
		j := newJob(id, record.Command)
		j.Cwd, j.Session, j.Config, j.StartedAt = record.Cwd, record.Session, cfg, record.Started
		j.exited, j.exitedAt, j.code, j.reason, j.source = true, record.Ended, record.ExitCode, record.Reason, record.Source
		j.notify, j.output = record.Notify, record.Output
		j.completed.set()
		jobs = append(jobs, j)
	}
	return jobs
}

func (r *Jobs) receipts(cfg *config.Config, session, prefix string) map[string]*Job {
	out := map[string]*Job{}
	for _, j := range r.receiptList(cfg, session, prefix) {
		out[j.ID] = j
	}
	return out
}

// Entry of a running job in the profile's checkpoint, what finds it again after icebath restarts
type checkpointEntry struct {
	ID        string    `json:"session_id"`
	Command   string    `json:"command"`
	Pid       int       `json:"pid"`
	Host      bool      `json:"host_pid"`
	Start     int64     `json:"host_start_time,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Started   time.Time `json:"started_at"`
	Session   string    `json:"session"`
	Notify    bool      `json:"notify_on_complete,omitempty"`
	Chars     int       `json:"completion_output_chars,omitempty"`
	Patterns  []string  `json:"watch_patterns,omitempty"`
	Heartbeat int       `json:"heartbeat_seconds,omitempty"`
	Persist   bool      `json:"persist_on_release,omitempty"`
	Unit      string    `json:"systemd_unit,omitempty"`
}

func checkpointPath(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "processes.json")
}

// Writes the running jobs of the profile to its checkpoint
func (r *Jobs) checkpoint(cfg *config.Config) {
	if cfg == nil {
		return
	}
	var entries []checkpointEntry
	r.mu.Lock()
	for _, j := range ordered(r.running, func(j *Job) int64 { return j.runSeq }) {
		if j.Config == nil || j.Config.Profile() != cfg.Profile() {
			continue
		}
		j.mu.Lock()
		if !j.exited {
			entries = append(entries, checkpointEntry{ID: j.ID, Command: j.Command, Pid: j.Pid, Host: j.hostPid, Start: j.start, Cwd: j.Cwd,
				Started: j.StartedAt, Session: j.Session, Notify: j.notify, Chars: j.completionChars, Patterns: j.patterns,
				Heartbeat: j.heartbeat, Persist: j.Persist, Unit: j.unit})
		}
		j.mu.Unlock()
	}
	entries = append(entries, r.unresolved[cfg.Profile()]...)
	r.mu.Unlock()
	path := checkpointPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Debug("Failed to write checkpoint file", "error", err)
		return
	}
	unlock, err := fsx.Lock(context.Background(), path+".lock", 10*time.Second)
	if err != nil {
		slog.Debug("Failed to write checkpoint file", "error", err)
		return
	}
	defer unlock()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Debug("Failed to write checkpoint file", "error", err)
	}
}

// Background jobs outlive icebath from now on, and those a previous run left running are taken up again as recovered jobs, their
// handles gone: what icebath does while it serves other processes
func (t *Terminal) Outlive() {
	t.Jobs.outlive = true
	entries, err := os.ReadDir(filepath.Dir(t.config.ProfileDir("default")))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && config.Named(entry.Name()) {
			t.Jobs.recover(t.config.In(entry.Name()))
		}
	}
}

// Takes up the jobs the profile's checkpoint names whose process still runs
func (r *Jobs) recover(cfg *config.Config) {
	data, err := os.ReadFile(checkpointPath(cfg))
	if err != nil {
		return
	}
	var entries []checkpointEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("The process checkpoint is unreadable, so no background process is recovered", "profile", cfg.Profile(), "error", err)
		return
	}
	var unresolved []checkpointEntry
	for _, entry := range entries {
		if entry.Pid == 0 || entry.ID == "" {
			continue
		}
		r.mu.Lock()
		_, tracked := r.running[entry.ID]
		r.mu.Unlock()
		if tracked {
			continue
		}
		command := cmp.Or(entry.Command, "unknown")
		if !entry.Host {
			slog.Info(fmt.Sprintf("Skipping recovery for non-host process: %s (pid=%d, scope=sandbox)", strs.Head(command, 60), entry.Pid))
			continue
		}
		switch fate(entry.Pid, entry.Start) {
		case "reused":
			slog.Info(fmt.Sprintf("Not recovering session %s: pid %d is alive but its start time no longer matches — PID was recycled onto an "+
				"unrelated process; refusing to adopt or signal it.", entry.ID, entry.Pid))
			continue
		case "gone":
			if entry.Unit != "" && !stopUnit(entry.Unit) {
				slog.Warn(fmt.Sprintf("Could not reap persisted scope %s for dead wrapper pid %d; retaining checkpoint entry for the next startup",
					entry.Unit, entry.Pid))
				unresolved = append(unresolved, entry)
			}
			continue
		}
		j := newJob(entry.ID, command)
		j.Pid, j.hostPid, j.start, j.Cwd, j.StartedAt, j.unit = entry.Pid, true, entry.Start, entry.Cwd, entry.Started, entry.Unit
		j.Session, j.Config, j.Detached, j.Persist = entry.Session, cfg, true, entry.Persist
		j.notify, j.completionChars, j.patterns, j.heartbeat = entry.Notify, entry.Chars, entry.Patterns, entry.Heartbeat
		if j.StartedAt.IsZero() {
			j.StartedAt = time.Now()
		}
		r.mu.Lock()
		r.insertRunning(j)
		r.mu.Unlock()
		slog.Info(fmt.Sprintf("Recovered detached process: %s (pid=%d)", strs.Head(command, 60), entry.Pid))
	}
	r.mu.Lock()
	r.unresolved[cfg.Profile()] = unresolved
	r.mu.Unlock()
	r.checkpoint(cfg)
}
