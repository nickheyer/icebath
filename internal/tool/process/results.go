package process

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

type resultRecord struct {
	ID                string  `json:"id"`
	Command           string  `json:"command"`
	Cwd               *string `json:"cwd"`
	TaskID            string  `json:"task_id"`
	OwnerTaskID       string  `json:"owner_task_id"`
	SessionKey        string  `json:"session_key"`
	ParentSessionID   string  `json:"parent_session_id"`
	StartedAt         float64 `json:"started_at"`
	ExitCode          *int    `json:"exit_code"`
	CompletionReason  string  `json:"completion_reason"`
	TerminationSource string  `json:"termination_source"`
	NotifyOnComplete  bool    `json:"notify_on_complete"`
	Output            string  `json:"output"`
}

func (r *Registry) resultsDir(ctx context.Context) (string, error) {
	h, err := r.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.ProcessResults.In(h), nil
}

func (r *Registry) saveCompletedResult(ctx context.Context, s *Session) {
	s.mu.Lock()
	rec := resultRecord{
		ID:                s.id,
		Command:           s.command,
		Cwd:               cwdValue(s.cwd),
		TaskID:            s.taskID,
		OwnerTaskID:       s.ownerTaskID,
		SessionKey:        s.sessionKey,
		ParentSessionID:   s.parentSessionID,
		StartedAt:         s.startedAt,
		ExitCode:          s.exitCode,
		CompletionReason:  s.completionReason,
		TerminationSource: s.terminationSource,
		NotifyOnComplete:  s.notify,
		Output:            s.out.String(),
	}
	s.mu.Unlock()
	rec.Output = r.redactor.Terminal(ctx, rec.Output, rec.Command, true)
	rec.Command = r.redactor.Text(ctx, rec.Command, redact.Mode{CodeFile: true, Force: true})
	if err := r.writeResult(context.WithoutCancel(ctx), rec); err != nil {
		slog.Warn("Could not retain completed process result", "process", s.id, "err", err)
	}
}

var resultKeys = []string{
	"id", "command", "cwd", "task_id", "owner_task_id", "session_key",
	"parent_session_id", "started_at", "exit_code", "completion_reason",
	"termination_source", "notify_on_complete", "output",
}

func receiptID(id string) bool {
	rest, ok := strings.CutPrefix(id, "proc_")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if !unicode.IsLetter(c) && !unicode.IsNumber(c) && c != '_' {
			return false
		}
	}
	return true
}

func (r *Registry) loadCompletedResults(ctx context.Context, owner, prefix string) []*Session {
	if owner == "" {
		return nil
	}
	dir, err := r.resultsDir(ctx)
	if err == nil {
		var paths []string
		if paths, err = retainedResults(dir); err == nil {
			var out []*Session
			for _, path := range paths {
				stem := strings.TrimSuffix(filepath.Base(path), ".json")
				if !strings.HasPrefix(stem, prefix) {
					continue
				}
				s, err := r.readReceipt(ctx, owner, path, stem)
				if err != nil {
					slog.Debug("Skipping unreadable process result", "file", filepath.Base(path), "err", err)
					continue
				}
				if s != nil {
					out = append(out, s)
				}
			}
			return out
		}
	}
	slog.Warn("Could not read retained process results", "err", err)
	return nil
}

func (r *Registry) readReceipt(ctx context.Context, owner, path, stem string) (*Session, error) {
	text, err := fileutil.ReadText(path)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return nil, err
	}
	for _, k := range resultKeys {
		if _, ok := fields[k]; !ok {
			return nil, fmt.Errorf("receipt has no %q field", k)
		}
	}
	var rec resultRecord
	if err := json.Unmarshal([]byte(text), &rec); err != nil {
		return nil, err
	}
	if rec.ID != stem || !receiptID(rec.ID) {
		return nil, nil
	}
	if rec.ParentSessionID == "" {
		return nil, nil
	}
	if rec.ParentSessionID != owner {
		tip, err := r.lineage.CompressionTip(ctx, rec.ParentSessionID)
		if err != nil {
			return nil, err
		}
		if tip != owner {
			return nil, nil
		}
	}
	cwd := ""
	if rec.Cwd != nil {
		cwd = *rec.Cwd
	}
	s := &Session{
		id:                rec.ID,
		command:           rec.Command,
		taskID:            rec.TaskID,
		ownerTaskID:       rec.OwnerTaskID,
		sessionKey:        rec.SessionKey,
		parentSessionID:   rec.ParentSessionID,
		cwd:               cwd,
		startedAt:         rec.StartedAt,
		pidScope:          scopeHost,
		ctx:               context.WithoutCancel(ctx),
		exited:            true,
		exitCode:          rec.ExitCode,
		completionReason:  rec.CompletionReason,
		terminationSource: rec.TerminationSource,
		notify:            rec.NotifyOnComplete,
		maxOutput:         maxOutputChars,
	}
	s.out.replace(rec.Output)
	s.initSignals()
	s.markDone()
	return s, nil
}

func cwdValue(cwd string) *string {
	if cwd == "" {
		return nil
	}
	return &cwd
}

func (r *Registry) writeResult(ctx context.Context, rec resultRecord) error {
	dir, err := r.resultsDir(ctx)
	if err != nil {
		return err
	}
	if err := home.AssertProfileLive(dir); err != nil {
		return err
	}
	if err := r.homes.MakeUserDir(ctx, dir, 0o700, true); err != nil {
		return err
	}
	if err := fileutil.WriteJSON(context.WithoutCancel(ctx), filepath.Join(dir, rec.ID+".json"), rec,
		fileutil.JSONStyle{Indent: 2}, fileutil.Private()); err != nil {
		return err
	}
	_, err = retainedResults(dir)
	return err
}

func retainedResults(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type retained struct {
		modified time.Time
		name     string
	}
	cutoff := time.Now().Add(-resultRetention)
	var kept []retained
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "proc_") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			continue
		}
		kept = append(kept, retained{info.ModTime(), name})
	}
	slices.SortFunc(kept, func(a, b retained) int {
		if c := b.modified.Compare(a.modified); c != 0 {
			return c
		}
		return cmp.Compare(b.name, a.name)
	})
	var paths []string
	for i, k := range kept {
		path := filepath.Join(dir, k.name)
		if i >= maxRetainedResult {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}
