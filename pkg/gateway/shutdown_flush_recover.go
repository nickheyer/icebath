package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	transcriptSpoolSeq  atomic.Int64
	replayedUnremovable = struct {
		sync.Mutex
		paths map[string]bool
	}{paths: map[string]bool{}}
	spoolRemove    = os.Remove
	spoolAcquireDB = func(ctx context.Context) (TranscriptAppender, func(), error) {
		owned, err := state.Acquire(ctx, "")
		if err != nil {
			return nil, nil, err
		}
		return owned, func() { state.ReleaseOrClose(owned) }, nil
	}
)

// Store surface a recovered row is appended through
type TranscriptAppender interface {
	AppendMessage(ctx context.Context, sessionID string, fields map[string]any, guards state.AppendGuards) (int64, error)
}

// Session key to id resolution with the store owning the key, an empty id or nil store meaning unresolved
type SpoolSessionResolver func(ctx context.Context, sessionKey string, notAfter *float64) (string, SessionStoreDB)

func replayedUnremovableHas(path string) bool {
	replayedUnremovable.Lock()
	defer replayedUnremovable.Unlock()
	return replayedUnremovable.paths[path]
}

// Spools one cap evicted transcript message as a private payload, empty when the write failed
func SpoolDroppedTranscriptMessage(ctx context.Context, sessionID string, message map[string]any) string {
	flushDir, err := shutdownFlushDir(ctx)
	if err == nil {
		data := pyjson.NewObject()
		data.Set("session_id", sessionID)
		data.Set("message", message)
		payload := pyjson.NewObject()
		payload.Set("session_key", sessionID)
		payload.Set("reason", TranscriptCapDropReason)
		payload.Set("ts", int64(math.Floor(shutdownFlushNow())))
		payload.Set("seq", transcriptSpoolSeq.Add(1)-1)
		payload.Set("data", data)
		var path string
		if path, err = writeFlushPayload(flushDir, payload); err == nil {
			return path
		}
	}
	slog.Debug(fmt.Sprintf("Failed to spool cap-dropped transcript message for %s: %v", sessionID, err))
	return ""
}

// Decoded JSON of a spool file read as utf-8-sig
func readSpoolPayload(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
}

type spoolEntry struct {
	ts, seq float64
	name    string
	path    string
	message map[string]any
}

// Replays the spooled cap drops of a session in drop order, returning replayed and remaining counts
func DrainTranscriptSpool(ctx context.Context, sessionID string, replay func(message map[string]any) error, dbKnownFailing bool) (int, int, error) {
	flushDir, err := shutdownFlushDir(ctx)
	var candidates []string
	if err == nil {
		candidates, err = spoolDirMatches(flushDir, "pending-*.json")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Cannot scan transcript spool: %v", err))
		return 0, 0, nil
	}
	var entries []spoolEntry
	for _, path := range candidates {
		if replayedUnremovableHas(path) {
			continue
		}
		parsed, err := readSpoolPayload(path)
		if err != nil {
			continue
		}
		payload, ok := parsed.(*pyjson.Object)
		if !ok {
			continue
		}
		if reason, _ := payload.Get("reason"); reason != TranscriptCapDropReason {
			continue
		}
		if key, _ := payload.Get("session_key"); key != sessionID {
			continue
		}
		data, _ := payload.Get("data")
		var message any
		if pyTruthy(data) {
			obj, ok := data.(*pyjson.Object)
			if !ok {
				return 0, 0, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(data) + "' object has no attribute 'get'"}
			}
			message, _ = obj.Get("message")
		}
		msg, ok := message.(*pyjson.Object)
		if !ok {
			slog.Warn(fmt.Sprintf("Removing structurally invalid transcript spool file %s", path))
			_ = os.Remove(path)
			continue
		}
		ts, seq := spoolSortKey(payload)
		entries = append(entries, spoolEntry{ts: ts, seq: seq, name: filepath.Base(path), path: path,
			message: pyjson.Plain(msg).(map[string]any)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return spoolEntryLess(entries[i], entries[j]) })
	replayed, remaining := 0, 0
	for idx, e := range entries {
		if err := replay(e.message); err != nil {
			if IsRowRejection(err) {
				quarantineSpoolFile(e.path, sessionID, err)
				continue
			}
			msg := fmt.Sprintf("Replay of spooled transcript message %s for %s failed; keeping spool file for retry: %v", e.path, sessionID, err)
			if dbKnownFailing {
				slog.Debug(msg)
			} else {
				slog.Warn(msg)
			}
			remaining = len(entries) - idx
			break
		}
		removeReplayed(e.path, sessionID)
		replayed++
	}
	if replayed > 0 {
		slog.Info(fmt.Sprintf("Replayed %d spooled transcript message(s) for %s after DB recovery", replayed, sessionID))
	}
	return replayed, remaining, nil
}

func spoolEntryLess(a, b spoolEntry) bool {
	if a.ts != b.ts {
		return a.ts < b.ts
	}
	if a.seq != b.seq {
		return a.seq < b.seq
	}
	return a.name < b.name
}

// Entries of a spool directory whose names match the pattern, dot files included like pathlib glob
func spoolDirMatches(dir, pattern string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); ok {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths, nil
}

// Spool ordering field as a finite float, anything else sorting first as zero
func spoolSortNumber(value any) float64 {
	var number float64
	switch v := value.(type) {
	case int64:
		number = float64(v)
	case *big.Int:
		f, _ := new(big.Float).SetInt(v).Float64()
		number = f
	case float64:
		number = v
	default:
		return 0
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0
	}
	return number
}

// Drop order timestamp and sequence of a payload, a missing sequence sorting as minus one
func spoolSortKey(payload *pyjson.Object) (float64, float64) {
	ts, _ := payload.Get("ts")
	seq, ok := payload.Get("seq")
	if !ok {
		seq = int64(-1)
	}
	return spoolSortNumber(ts), spoolSortNumber(seq)
}

// Whether the database refused the row values themselves so no retry can write it
func IsRowRejection(err error) bool {
	var pyErr *PyError
	if errors.As(err, &pyErr) {
		switch pyErr.Kind {
		case "TypeError", "ValueError", "AttributeError", "OverflowError":
			return true
		}
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "binding parameter") || strings.HasPrefix(text, "sql: converting argument")
}

// Deletes a replayed spool file, remembering it for this process when the delete fails
func removeReplayed(path, sessionID string) {
	if err := spoolRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		replayedUnremovable.Lock()
		replayedUnremovable.paths[path] = true
		replayedUnremovable.Unlock()
		slog.Error(fmt.Sprintf("Spooled transcript row for %s was written, but %s could not be deleted (%v); delete it by hand, or the next start writes that row again", sessionID, path, err))
	}
}

// Moves a rejected spool file out of every replay glob for manual recovery
func quarantineSpoolFile(path, sessionID string, cause error) {
	target := path + QuarantineSuffix
	if err := os.Rename(path, target); err != nil {
		slog.Error(fmt.Sprintf("Spooled transcript row for %s in %s was rejected by the database (%v) and could not be quarantined: %v", sessionID, path, cause, err))
		return
	}
	slog.Warn(fmt.Sprintf("Spooled transcript row for %s was rejected by the database (%v); moved it to %s for manual recovery and continuing with the session's later rows", sessionID, cause, target))
}

// Orders recovery files by drop key, unparseable or non object files last by name
func orderFlushFiles(paths []string) []string {
	type keyed struct {
		bad     int
		ts, seq float64
		name    string
		path    string
	}
	entries := make([]keyed, 0, len(paths))
	for _, path := range paths {
		parsed, err := readSpoolPayload(path)
		if payload, ok := parsed.(*pyjson.Object); err == nil && ok {
			ts, seq := spoolSortKey(payload)
			entries = append(entries, keyed{0, ts, seq, filepath.Base(path), path})
		} else {
			entries = append(entries, keyed{1, 0, 0, filepath.Base(path), path})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.bad != b.bad {
			return a.bad < b.bad
		}
		return spoolEntryLess(spoolEntry{ts: a.ts, seq: a.seq, name: a.name}, spoolEntry{ts: b.ts, seq: b.seq, name: b.name})
	})
	ordered := make([]string, len(entries))
	for i, e := range entries {
		ordered[i] = e.path
	}
	return ordered
}

// Replays flush files into state.db and returns how many messages were recovered
func RecoverPendingToDB(ctx context.Context, sessionDB TranscriptAppender, resolver SpoolSessionResolver) (int, error) {
	recovered, _, err := RecoverPendingSpool(ctx, sessionDB, resolver)
	return recovered, err
}

// Replays flush files deleting each on success, returning the count and the sessions held back
func RecoverPendingSpool(ctx context.Context, sessionDB TranscriptAppender, resolver SpoolSessionResolver) (int, map[string]bool, error) {
	flushDir, err := shutdownFlushDir(ctx)
	if err != nil {
		return 0, nil, err
	}
	found, err := spoolDirMatches(flushDir, "*.json")
	if err != nil {
		return 0, nil, err
	}
	flushFiles := orderFlushFiles(found)
	if len(flushFiles) == 0 {
		return 0, map[string]bool{}, nil
	}
	if sessionDB == nil {
		owned, release, err := spoolAcquireDB(ctx)
		if err != nil {
			return 0, nil, err
		}
		defer release()
		sessionDB = owned
	}
	recovered := 0
	blocked := map[string]int{}
	var blockedOrder []string
	for _, path := range flushFiles {
		if replayedUnremovableHas(path) {
			continue
		}
		before := len(blocked)
		ok, sessionKey, err := recoverFlushFile(ctx, sessionDB, path, resolver, blocked)
		if len(blocked) > before {
			for sid := range blocked {
				if !containsString(blockedOrder, sid) {
					blockedOrder = append(blockedOrder, sid)
				}
			}
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to recover pending message from %s: %v", path, err))
			continue
		}
		if ok {
			recovered++
			removeReplayed(path, sessionKey)
		}
	}
	if recovered > 0 {
		slog.Info(fmt.Sprintf("Recovered %d pending message(s) from shutdown flush", recovered))
	}
	held := map[string]bool{}
	if len(blocked) > 0 {
		total := 0
		parts := make([]string, 0, len(blockedOrder))
		for _, sid := range blockedOrder {
			total += blocked[sid]
			parts = append(parts, fmt.Sprintf("%s (%d)", sid, blocked[sid]))
			held[sid] = true
		}
		slog.Info(fmt.Sprintf("Held back %d spooled transcript file(s) for the next start after a failed replay: %s", total, strings.Join(parts, ", ")))
	}
	return recovered, held, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Reads one flush file and replays it unless it is an agent history snapshot
func recoverFlushFile(ctx context.Context, sessionDB TranscriptAppender, path string, resolver SpoolSessionResolver, blocked map[string]int) (bool, string, error) {
	parsed, err := readSpoolPayload(path)
	if err != nil {
		return false, "", err
	}
	payload, isObject := parsed.(*pyjson.Object)
	if !isObject {
		return false, "", &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(parsed) + "' object has no attribute 'get'"}
	}
	if reason, _ := payload.Get("reason"); reason == AgentHistoryReason {
		return false, "", nil
	}
	ok, err := recoverOnePayload(ctx, sessionDB, path, payload, resolver, blocked)
	if err != nil || !ok {
		return false, "", err
	}
	key, found := payload.Get("session_key")
	if !found {
		key = ""
	}
	return true, pyStrOf(key), nil
}

// Dict view of a payload field, an AttributeError for a present non object value
func payloadDict(value any) (*pyjson.Object, error) {
	if obj, ok := value.(*pyjson.Object); ok {
		return obj, nil
	}
	return nil, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(value) + "' object has no attribute 'get'"}
}

// Appends one flush payload, false when it was kept or quarantined instead of replayed
func recoverOnePayload(ctx context.Context, sessionDB TranscriptAppender, path string, payload *pyjson.Object, resolver SpoolSessionResolver, blocked map[string]int) (bool, error) {
	if reason, _ := payload.Get("reason"); reason == TranscriptCapDropReason {
		data := pyjson.NewObject()
		if raw, ok := payload.Get("data"); ok && pyTruthy(raw) {
			obj, err := payloadDict(raw)
			if err != nil {
				return false, err
			}
			data = obj
		}
		rawSID, ok := data.Get("session_id")
		if !ok {
			rawSID = ""
		}
		rawMessage, _ := data.Get("message")
		message, isDict := rawMessage.(*pyjson.Object)
		if !pyTruthy(rawSID) || !isDict {
			slog.Warn(fmt.Sprintf("Cannot recover structurally invalid transcript spool file %s; preserved for manual inspection", path))
			return false, nil
		}
		spooledSID := pyStrOf(rawSID)
		if _, held := blocked[spooledSID]; held {
			blocked[spooledSID]++
			return false, nil
		}
		ts, _ := payload.Get("ts")
		kwargs := transcriptAppendKwargs(spooledSID, pyjson.Plain(message).(map[string]any), ts)
		if err := appendKwargs(ctx, sessionDB, kwargs); err != nil {
			if IsRowRejection(err) {
				quarantineSpoolFile(path, spooledSID, err)
				return false, nil
			}
			blocked[spooledSID] = 1
			return false, err
		}
		return true, nil
	}
	rawKey, ok := payload.Get("session_key")
	if !ok {
		rawKey = ""
	}
	rawData, ok := payload.Get("data")
	if !ok {
		rawData = pyjson.NewObject()
	}
	data, err := payloadDict(rawData)
	if err != nil {
		return false, err
	}
	text, ok := data.Get("text")
	if !ok {
		text = ""
	}
	if !pyTruthy(text) || !pyTruthy(rawKey) {
		slog.Warn(fmt.Sprintf("Cannot recover structurally invalid pending message from %s; the flush file has been preserved", path))
		return false, nil
	}
	sessionKey := pyStrOf(rawKey)
	rawSID, _ := data.Get("session_id")
	sessionID, target := "", sessionDB
	if pyTruthy(rawSID) {
		sessionID = pyStrOf(rawSID)
	}
	ts, hasTS := payload.Get("ts")
	if sessionID == "" && resolver != nil {
		if notAfter, err := spoolNotAfter(ts); err != nil {
			slog.Debug(fmt.Sprintf("Session key->id resolution failed for %s: %v", sessionKey, err))
		} else if sid, db := resolver(ctx, sessionKey, notAfter); db != nil {
			appender, ok := db.(TranscriptAppender)
			if !ok {
				return false, &PyError{Kind: "AttributeError", Msg: "session store has no attribute 'append_message'"}
			}
			sessionID, target = sid, appender
		}
	}
	if sessionID == "" {
		slog.Warn(fmt.Sprintf("Cannot recover pending message for %s: no session_id in flush file and session_key-to-id resolution failed. The message text is preserved in %s", sessionKey, path))
		return false, nil
	}
	if !hasTS {
		ts = int64(math.Floor(shutdownFlushNow()))
	}
	_, err = target.AppendMessage(ctx, sessionID, map[string]any{"role": "user", "content": text, "timestamp": ts}, state.AppendGuards{})
	return err == nil, err
}

// Not after bound a payload ts gives the resolver, the int error for a value the resolver cannot bound by
func spoolNotAfter(ts any) (*float64, error) {
	if ts == nil {
		return nil, nil
	}
	n, err := pyint.Of(ts)
	if err != nil {
		return nil, err
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	return &f, nil
}

// Appends one transcript row built by the shared kwargs builder through the store
func appendKwargs(ctx context.Context, appender TranscriptAppender, kwargs map[string]any) error {
	fields := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		if k != "session_id" {
			fields[k] = v
		}
	}
	sid, _ := kwargs["session_id"].(string)
	_, err := appender.AppendMessage(ctx, sid, fields, state.AppendGuards{})
	return err
}

// Replays every pending spool the gateway owns, the launch home first then each served profile home
func RecoverGatewayPending(ctx context.Context, store *SessionStore, cfg *GatewayConfig) (int, error) {
	recovered, held, err := RecoverPendingSpool(ctx, nil, store.ResolveSessionIDForKey)
	if err != nil {
		return 0, err
	}
	store.markSpooledDropSessions(held)
	if cfg == nil || cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		return recovered, nil
	}
	launchHome := resolvedPath(home.Dir(ctx))
	homes, err := multiplexProfileHomes(ctx, cfg)
	if err != nil {
		return recovered, err
	}
	for _, served := range homes {
		if resolvedPath(served.Home) == launchHome {
			continue
		}
		if info, err := os.Stat(filepath.Join(served.Home, "pending_messages")); err != nil || !info.IsDir() {
			continue
		}
		count, held, err := RecoverPendingSpool(home.WithOverride(ctx, served.Home), nil, store.ResolveSessionIDForKey)
		if err != nil {
			slog.Warn(fmt.Sprintf("Pending-message recovery failed for profile %s", served.Name), "error", err)
			continue
		}
		recovered += count
		store.markSpooledDropSessions(held)
	}
	return recovered, nil
}

// Absolute path with symlinks resolved where they exist, like Path.resolve
func resolvedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Writes an agent in memory transcript outside the database before teardown, swallowing every failure
func FlushAgentHistoryToFile(ctx context.Context, sessionID *string, history []any) {
	if len(history) == 0 {
		return
	}
	var sid any
	if sessionID != nil {
		sid = *sessionID
	}
	flushDir, err := shutdownFlushDir(ctx)
	if err == nil {
		snapshot := make([]any, 0, len(history))
		for _, m := range history {
			switch m.(type) {
			case nil, bool, string, int, int64, float64, *big.Int, []any, map[string]any, *pyjson.Object:
				snapshot = append(snapshot, m)
			default:
				snapshot = append(snapshot, pyrepr.Str(m))
			}
		}
		payload := pyjson.NewObject()
		payload.Set("reason", AgentHistoryReason)
		payload.Set("issue", "#72680")
		payload.Set("session_id", sid)
		payload.Set("count", len(snapshot))
		payload.Set("messages", snapshot)
		if _, err = writeFlushPayload(flushDir, payload); err == nil {
			slog.Warn(fmt.Sprintf("Preserved %d in-memory message(s) for session %s (possible FTS corruption — recover after repairing state.db)", len(snapshot), pyrepr.Str(sid)))
			return
		}
	}
	slog.Warn(fmt.Sprintf("Agent-history shutdown preservation failed for session %s: %v", pyrepr.Str(sid), err))
}
