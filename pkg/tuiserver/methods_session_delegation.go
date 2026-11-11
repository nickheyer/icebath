package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	registerSessionDelegationHandlers()
}

var sessionDelegationRegistry = &handlerRegistry{}

func registerSessionDelegationHandlers() {
	sessionDelegationRegistry.method("delegation.status", delegationStatus)
	sessionDelegationRegistry.method("delegation.pause", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		var paused any = true
		if value, present := params["paused"]; present {
			paused = value
		}
		return ok(rid, map[string]any{"paused": agent.SetSpawnPaused(truthy(paused))})
	})
	sessionDelegationRegistry.method("subagent.steer", subagentSteer)
	sessionDelegationRegistry.method("spawn_tree.save", spawnTreeSave)
	sessionDelegationRegistry.method("spawn_tree.list", spawnTreeList)
	sessionDelegationRegistry.method("spawn_tree.load", spawnTreeLoad)
	sessionDelegationRegistry.install()
}

// Live delegated children, the spawn pause and the depth and concurrency limits
func delegationStatus(ctx context.Context, rid any, params map[string]any) map[string]any {
	return ok(rid, map[string]any{"active": agent.ListActiveSubagents(), "paused": agent.IsSpawnPaused(),
		"max_spawn_depth": tools.MaxSpawnDepth(ctx), "max_concurrent_children": tools.MaxConcurrentChildren(ctx)})
}

// Queues steering text into a live delegated child owned by the caller's session
func subagentSteer(ctx context.Context, rid any, params map[string]any) map[string]any {
	subagentID := strParam(params, "subagent_id", "")
	if subagentID == "" {
		return rpcErr(rid, 4000, "subagent_id required", nil)
	}
	var raw any = ""
	if truthy(params["text"]) {
		raw = params["text"]
	}
	textValue, isStr := raw.(string)
	if !isStr {
		panic(fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw)))
	}
	text := strings.TrimSpace(textValue)
	if text == "" {
		return rpcErr(rid, 4002, "text is required", nil)
	}
	if _, errResp := sessNowait(ctx, params, rid); errResp != nil {
		return errResp
	}
	ownerID := strParam(params, "session_id", "")
	transport, owner := currentSessionSteerAuthority(ctx, ownerID)
	queued := transport != nil && owner != nil && agent.SteerSubagent(subagentID, text, &ownerID, transport, owner)
	status := "rejected"
	if queued {
		status = "queued"
	}
	return ok(rid, map[string]any{"status": status, "subagent_id": subagentID, "text": text})
}

// Writes a finished spawn tree snapshot under the session directory and indexes it
func spawnTreeSave(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionID := strParam(params, "session_id", "")
	var subagents []any
	if truthy(params["subagents"]) {
		list, isList := params["subagents"].([]any)
		if !isList {
			return rpcErr(rid, 4000, "subagents list required", nil)
		}
		subagents = list
	}
	if len(subagents) == 0 {
		return rpcErr(rid, 4000, "subagents list required", nil)
	}
	startedAt, label := params["started_at"], strOr(params["label"])
	finishedAt := float64(time.Now().UnixNano()) / 1e9
	if truthy(params["finished_at"]) {
		parsed, err := pyint.Float(params["finished_at"])
		panicIf(err)
		finishedAt = parsed
	}
	dirName := sessionID
	if dirName == "" {
		dirName = "default"
	}
	d := spawnTreeSessionDir(dirName)
	stamp := time.Unix(0, int64(finishedAt*1e9)).UTC().Format("20060102T150405")
	path := filepath.Join(d, stamp+".json")
	var started any
	if truthy(startedAt) {
		parsed, err := pyint.Float(startedAt)
		panicIf(err)
		started = parsed
	}
	meta := [][2]any{{"session_id", sessionID}, {"started_at", started}, {"finished_at", finishedAt}, {"label", label}}
	snapshot := pyjson.NewObject()
	for _, kv := range meta {
		snapshot.Set(kv[0].(string), kv[1])
	}
	snapshot.Set("subagents", subagents)
	encoded, err := pyjson.Dumps(snapshot, pyjson.EnsureASCII(false))
	panicIf(err)
	if err := os.WriteFile(path, []byte(encoded), 0o644); err != nil {
		return rpcErr(rid, 5000, "spawn_tree.save failed: "+pypath.OSErrorText(err), nil)
	}
	entry := pyjson.NewObject()
	entry.Set("path", path)
	for _, kv := range meta {
		entry.Set(kv[0].(string), kv[1])
	}
	entry.Set("count", len(subagents))
	appendSpawnTreeIndexObject(d, entry)
	return ok(rid, map[string]any{"path": path, "session_id": sessionID})
}

// Appends an ordered entry line to the session index
func appendSpawnTreeIndexObject(sessionDir string, entry *pyjson.Object) {
	plain, _ := pyjson.Plain(entry).(map[string]any)
	line, err := pyjson.Dumps(entry, pyjson.EnsureASCII(false))
	if err != nil {
		appendSpawnTreeIndex(sessionDir, plain)
		return
	}
	f, err := os.OpenFile(filepath.Join(sessionDir, spawnTreeIndex), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.WriteString(line + "\n")
		f.Close()
	}
	if err != nil {
		logger().Debug("spawn_tree index append failed: " + err.Error())
	}
}

// Index shaped entry of a snapshot file written before the index existed, nil when unreadable
func legacySpawnTreeEntry(p, sessionDirName string) map[string]any {
	info, err := os.Stat(p)
	if err != nil {
		return nil
	}
	raw := map[string]any{}
	suppress(func() {
		data, readErr := os.ReadFile(p)
		panicIf(readErr)
		decoded, loadErr := pyjson.Loads(string(data))
		panicIf(loadErr)
		if dict, isDict := pyjson.Plain(decoded).(map[string]any); isDict {
			raw = dict
		}
	})
	count := 0
	if subagents, isList := raw["subagents"].([]any); isList {
		count = len(subagents)
	}
	sessionID := raw["session_id"]
	if !truthy(sessionID) {
		sessionID = sessionDirName
	}
	finished := raw["finished_at"]
	if !truthy(finished) {
		finished = float64(info.ModTime().UnixNano()) / 1e9
	}
	label := raw["label"]
	if !truthy(label) {
		label = ""
	}
	return map[string]any{"path": p, "session_id": sessionID, "finished_at": finished, "started_at": raw["started_at"],
		"label": label, "count": count}
}

// Saved spawn trees of the session or every session, newest first
func spawnTreeList(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionID := strParam(params, "session_id", "")
	var roots []string
	if truthy(params["cross_session"]) {
		root := spawnTreesRoot()
		children, err := os.ReadDir(root)
		panicIf(err)
		for _, child := range children {
			if isDir(filepath.Join(root, child.Name())) {
				roots = append(roots, filepath.Join(root, child.Name()))
			}
		}
	} else {
		if sessionID == "" {
			sessionID = "default"
		}
		roots = []string{spawnTreeSessionDir(sessionID)}
	}
	entries := []map[string]any{}
	for _, d := range roots {
		if indexed := readSpawnTreeIndex(d); len(indexed) > 0 {
			for _, item := range indexed {
				entry, isDict := item.(map[string]any)
				if !isDict {
					panic(fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(item)))
				}
				if p := entry["path"]; truthy(p) {
					if _, err := os.Stat(pyrepr.Str(p)); err == nil {
						entries = append(entries, entry)
					}
				}
			}
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(d, "*.json"))
		for _, p := range matches {
			if filepath.Base(p) == spawnTreeIndex {
				continue
			}
			if entry := legacySpawnTreeEntry(p, filepath.Base(d)); entry != nil {
				entries = append(entries, entry)
			}
		}
	}
	finishedOf := func(entry map[string]any) float64 {
		if value := entry["finished_at"]; truthy(value) {
			if f, err := pyint.Float(value); err == nil {
				return f
			}
		}
		return 0
	}
	sort.SliceStable(entries, func(i, j int) bool { return finishedOf(entries[i]) > finishedOf(entries[j]) })
	limit := int64(50)
	if truthy(params["limit"]) {
		n, err := pyint.Int64(params["limit"])
		panicIf(err)
		limit = n
	}
	if limit < 0 {
		limit = int64(len(entries)) + limit
		if limit < 0 {
			limit = 0
		}
	}
	if limit < int64(len(entries)) {
		entries = entries[:limit]
	}
	return ok(rid, map[string]any{"entries": entries})
}

// Loads a saved spawn tree snapshot that lies under the spawn trees root
func spawnTreeLoad(ctx context.Context, rid any, params map[string]any) map[string]any {
	rawPath := strParam(params, "path", "")
	if rawPath == "" {
		return rpcErr(rid, 4000, "path required", nil)
	}
	resolved, err := pypath.Realpath(rawPath, pypath.Lax)
	if err != nil {
		return rpcErr(rid, 4030, "path outside spawn-trees root: "+err.Error(), nil)
	}
	root, err := pypath.Realpath(spawnTreesRoot(), pypath.Lax)
	if err != nil {
		return rpcErr(rid, 4030, "path outside spawn-trees root: "+err.Error(), nil)
	}
	if rel, relErr := filepath.Rel(root, resolved); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rpcErr(rid, 4030, fmt.Sprintf("path outside spawn-trees root: %s is not in the subpath of %s",
			pyrepr.StrRepr(resolved), pyrepr.StrRepr(root)), nil)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return rpcErr(rid, 5000, "spawn_tree.load failed: "+pypath.OSErrorText(err), nil)
	}
	decoded, err := pyjson.Loads(string(data))
	if err != nil {
		return rpcErr(rid, 5000, "spawn_tree.load failed: "+err.Error(), nil)
	}
	payload, isDict := pyjson.Plain(decoded).(map[string]any)
	if !isDict {
		return rpcErr(rid, 5000, "spawn_tree.load failed: snapshot is not a JSON object", nil)
	}
	return ok(rid, payload)
}
