package process

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

type checkpointEntry struct {
	SessionID             string   `json:"session_id"`
	Command               string   `json:"command"`
	PID                   *int     `json:"pid"`
	PIDScope              string   `json:"pid_scope"`
	HostStartTime         *int64   `json:"host_start_time"`
	SystemdUnit           string   `json:"systemd_unit"`
	WSLChain              bool     `json:"wsl_chain"`
	Cwd                   *string  `json:"cwd"`
	StartedAt             float64  `json:"started_at"`
	TaskID                string   `json:"task_id"`
	OwnerTaskID           string   `json:"owner_task_id"`
	SessionKey            string   `json:"session_key"`
	WatcherPlatform       string   `json:"watcher_platform"`
	WatcherChatID         string   `json:"watcher_chat_id"`
	WatcherUserID         string   `json:"watcher_user_id"`
	WatcherUserName       string   `json:"watcher_user_name"`
	WatcherThreadID       string   `json:"watcher_thread_id"`
	WatcherMsgID          string   `json:"watcher_message_id"`
	WatcherInterval       int      `json:"watcher_interval"`
	ParentSessionID       string   `json:"parent_session_id"`
	NotifyOnComplete      bool     `json:"notify_on_complete"`
	CompletionOutputChars int      `json:"completion_output_chars"`
	WatchPatterns         []string `json:"watch_patterns"`
	HeartbeatSeconds      int      `json:"heartbeat_seconds"`
	PersistOnRelease      bool     `json:"persist_on_release"`
	OutputLog             string   `json:"output_log"`
}

func (e checkpointEntry) pid() int {
	if e.PID == nil {
		return 0
	}
	return *e.PID
}

func (s *Session) checkpointLocked() checkpointEntry {
	var pid *int
	if s.pid != 0 {
		pid = intPtr(s.pid)
	}
	owner := s.ownerTaskID
	if owner == "" {
		owner = s.taskID
	}
	patterns := s.watchPatterns
	if patterns == nil {
		patterns = []string{}
	}
	return checkpointEntry{
		SessionID:             s.id,
		Command:               s.command,
		PID:                   pid,
		PIDScope:              s.pidScope,
		HostStartTime:         s.hostStart,
		SystemdUnit:           s.systemdUnit,
		WSLChain:              s.wslChain,
		Cwd:                   cwdValue(s.cwd),
		StartedAt:             s.startedAt,
		TaskID:                s.taskID,
		OwnerTaskID:           owner,
		SessionKey:            s.sessionKey,
		WatcherPlatform:       s.route.Platform,
		WatcherChatID:         s.route.ChatID,
		WatcherUserID:         s.route.UserID,
		WatcherUserName:       s.route.UserName,
		WatcherThreadID:       s.route.ThreadID,
		WatcherMsgID:          s.route.MessageID,
		WatcherInterval:       s.watcherInterval,
		ParentSessionID:       s.parentSessionID,
		NotifyOnComplete:      s.notify,
		CompletionOutputChars: s.outputChars,
		WatchPatterns:         patterns,
		HeartbeatSeconds:      s.heartbeatSeconds,
		PersistOnRelease:      s.persistOnRelease,
		OutputLog:             s.outputLog,
	}
}

func (r *Registry) checkpointPaths(ctx context.Context) (string, string, error) {
	h, err := r.homes.Home(ctx)
	if err != nil {
		return "", "", err
	}
	return home.ProcessesFile.In(h), home.ProcessesLock.In(h), nil
}

func readCheckpoint(path string) ([]json.RawMessage, error) {
	text, err := fileutil.ReadText(path)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func decodeEntry(raw json.RawMessage) (checkpointEntry, bool) {
	e := checkpointEntry{PIDScope: scopeHost, Command: "unknown"}
	if len(raw) == 0 || raw[0] != '{' {
		return e, false
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, false
	}
	return e, true
}

func (r *Registry) writeCheckpoint(ctx context.Context, extra []checkpointEntry) {
	if err := r.persistCheckpoint(context.WithoutCancel(ctx), extra); err != nil {
		slog.Debug("Failed to write checkpoint file", "err", err)
	}
}

func (r *Registry) persistCheckpoint(ctx context.Context, extra []checkpointEntry) error {
	r.mu.Lock()
	entries := []checkpointEntry{}
	for _, s := range r.running.list() {
		s.mu.Lock()
		if s.exited {
			s.mu.Unlock()
			continue
		}
		if s.hostStart == nil && s.pidScope == scopeHost && s.pid != 0 {
			s.hostStart = startFingerprint(s.pid)
		}
		e := s.checkpointLocked()
		s.mu.Unlock()
		e.Command = r.redactor.Text(ctx, s.command, redact.Mode{CodeFile: true})
		entries = append(entries, e)
	}
	if len(extra) > 0 {
		ids := map[string]bool{}
		for _, e := range entries {
			ids[e.SessionID] = true
		}
		for _, e := range extra {
			if !ids[e.SessionID] {
				entries = append(entries, e)
			}
		}
	}
	known := map[string]bool{}
	for _, s := range r.running.list() {
		known[s.id] = true
	}
	for _, s := range r.finished.list() {
		known[s.id] = true
	}
	r.mu.Unlock()
	path, lockPath, err := r.checkpointPaths(ctx)
	if err != nil {
		return err
	}
	lock, err := fileutil.OpenLock(ctx, lockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	entries = append(entries, r.foreignHandoffs(ctx, path, known)...)
	return fileutil.WriteJSON(lock.Context(ctx), path, entries, fileutil.JSONStyle{Indent: 2})
}

func (r *Registry) foreignHandoffs(ctx context.Context, path string, known map[string]bool) []checkpointEntry {
	raw, err := readCheckpoint(path)
	if err != nil {
		return nil
	}
	var out []checkpointEntry
	for _, item := range raw {
		e, ok := decodeEntry(item)
		if !ok || known[e.SessionID] {
			continue
		}
		log := r.adoptableLog(ctx, e.OutputLog)
		if log == "" {
			continue
		}
		if _, err := os.Stat(log); err != nil {
			continue
		}
		if _, err := os.Stat(exitPath(log)); err == nil || detachedHostFate(e.pid(), e.HostStartTime) == fateRunning {
			out = append(out, e)
		}
	}
	return out
}

func (r *Registry) Recover(ctx context.Context) int {
	r.jobsOutliveHost.Store(true)
	path, _, err := r.checkpointPaths(ctx)
	if err != nil {
		return 0
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	raw, err := readCheckpoint(path)
	if err != nil {
		return 0
	}
	recovered := 0
	var unresolved []checkpointEntry
	for _, item := range raw {
		e, ok := decodeEntry(item)
		if !ok {
			continue
		}
		pid := e.pid()
		if pid == 0 {
			continue
		}
		r.mu.Lock()
		_, tracked := r.running.get(e.SessionID)
		r.mu.Unlock()
		if tracked {
			continue
		}
		if e.PIDScope != scopeHost {
			slog.Info("Skipping recovery for non-host process", "command", runeHead(e.Command, 60), "pid", pid, "scope", e.PIDScope)
			continue
		}
		fate := detachedHostFate(pid, e.HostStartTime)
		log := r.adoptableLog(ctx, e.OutputLog)
		if log != "" {
			fate = fateRunning
		}
		if fate == fateReused {
			slog.Info("Not recovering session: its pid is alive but its start time no longer matches, the PID was "+
				"recycled onto an unrelated process; refusing to adopt or signal it", "process", e.SessionID, "pid", pid)
			continue
		}
		if fate != fateRunning {
			if e.SystemdUnit != "" && !stopUnit(ctx, e.SystemdUnit) {
				slog.Warn("Could not reap persisted scope for dead wrapper pid; retaining checkpoint entry for the next startup",
					"unit", e.SystemdUnit, "pid", pid)
				unresolved = append(unresolved, e)
			}
			continue
		}
		s := r.recoveredSession(ctx, e, log)
		if log != "" {
			if err := r.trackStarted(ctx, s, func(c context.Context) { r.readLog(c, s) }); err != nil {
				break
			}
		} else {
			r.mu.Lock()
			r.running.put(s)
			r.mu.Unlock()
		}
		recovered++
		slog.Info("Recovered detached process", "command", runeHead(s.command, 60), "pid", pid)
		if s.watcherInterval > 0 {
			r.AddWatcher(Watcher{
				SessionID:       s.id,
				CheckInterval:   s.watcherInterval,
				SessionKey:      s.sessionKey,
				Route:           s.route,
				Notify:          s.notify,
				ParentSessionID: s.parentSessionID,
			})
		}
	}
	r.writeCheckpoint(ctx, unresolved)
	return recovered
}

func (r *Registry) recoveredSession(ctx context.Context, e checkpointEntry, log string) *Session {
	owner := e.OwnerTaskID
	if owner == "" {
		owner = e.TaskID
	}
	cwd := ""
	if e.Cwd != nil {
		cwd = *e.Cwd
	}
	startedAt := e.StartedAt
	if startedAt == 0 {
		startedAt = now()
	}
	s := &Session{
		id:               e.SessionID,
		command:          e.Command,
		taskID:           e.TaskID,
		ownerTaskID:      owner,
		sessionKey:       e.SessionKey,
		parentSessionID:  e.ParentSessionID,
		cwd:              cwd,
		startedAt:        startedAt,
		pidScope:         e.PIDScope,
		wslChain:         e.WSLChain,
		detached:         true,
		ctx:              context.WithoutCancel(ctx),
		pid:              e.pid(),
		hostStart:        e.HostStartTime,
		completionReason: reasonExited,
		maxOutput:        maxOutputChars,
		systemdUnit:      e.SystemdUnit,
		persistOnRelease: e.PersistOnRelease,
		outputLog:        log,
		route: Route{
			Platform:  e.WatcherPlatform,
			ChatID:    e.WatcherChatID,
			UserID:    e.WatcherUserID,
			UserName:  e.WatcherUserName,
			ThreadID:  e.WatcherThreadID,
			MessageID: e.WatcherMsgID,
		},
		watcherInterval:  e.WatcherInterval,
		notify:           e.NotifyOnComplete,
		outputChars:      e.CompletionOutputChars,
		watchPatterns:    e.WatchPatterns,
		heartbeatSeconds: e.HeartbeatSeconds,
	}
	s.initSignals()
	return s
}

func (r *Registry) refreshDetached(ctx context.Context, s *Session) *Session {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	skip := s.exited || !s.detached || s.pidScope != scopeHost || s.outputLog != ""
	pid, expected := s.pid, s.hostStart
	s.mu.Unlock()
	if skip {
		return s
	}
	switch detachedHostFate(pid, expected) {
	case fateRunning:
		return s
	case fateGone:
		return r.pruneUncollectedDetached(ctx, s)
	}
	r.closeReusedDetached(ctx, s)
	return s
}

func (r *Registry) closeReusedDetached(ctx context.Context, s *Session) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	s.mu.Unlock()
	r.mu.Lock()
	if _, ok := r.running.get(s.id); ok {
		s.mu.Lock()
		s.exitedAt = now()
		s.mu.Unlock()
		r.running.remove(s.id)
	}
	r.finished.put(s)
	r.mu.Unlock()
	r.writeCheckpoint(ctx, nil)
	s.markDone()
}

func (r *Registry) pruneUncollectedDetached(ctx context.Context, s *Session) *Session {
	s.mu.Lock()
	s.exited = true
	unit := s.systemdUnit
	s.mu.Unlock()
	r.mu.Lock()
	r.running.remove(s.id)
	if unit != "" {
		r.finished.put(s)
	} else {
		r.finished.remove(s.id)
	}
	r.mu.Unlock()
	r.writeCheckpoint(ctx, nil)
	s.markDone()
	if unit != "" {
		return s
	}
	return nil
}

func uncollectedGone(s *Session) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	gone := s.exited && s.exitCode == nil && s.detached && s.pidScope == scopeHost
	pid := s.pid
	s.mu.Unlock()
	return gone && !hostPIDAlive(pid)
}
