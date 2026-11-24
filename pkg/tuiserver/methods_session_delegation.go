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
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

func init() {
	registerSessionDelegationHandlers()
}

var sessionDelegationRegistry = &handlerRegistry{}

func registerSessionDelegationHandlers() {
	sessionDelegationRegistry.method("delegation.status", delegationStatus)
	sessionDelegationRegistry.method("spawn_tree.save", spawnTreeSave)
	sessionDelegationRegistry.method("spawn_tree.list", spawnTreeList)
	sessionDelegationRegistry.method("spawn_tree.load", spawnTreeLoad)
	sessionDelegationRegistry.install()
}

// Value a typed optional field holds, the zero value when absent
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// Live delegated children, the spawn pause and the depth and concurrency limits
func delegationStatus(ctx context.Context, rid any, params map[string]any) map[string]any {
	result := tuigateway.DelegationStatusResult{Active: []tuigateway.ActiveSubagent{}, Paused: agent.IsSpawnPaused(),
		MaxSpawnDepth: int64(tools.MaxSpawnDepth(ctx)), MaxConcurrentChildren: int64(tools.MaxConcurrentChildren(ctx))}
	for _, record := range agent.ListActiveSubagents() {
		result.Active = append(result.Active, typedRow[tuigateway.ActiveSubagent]("delegation.status", record))
	}
	return ok(rid, tuigateway.Plain(result))
}

// Writes a finished spawn tree snapshot under the session directory, a file saved again keeping its one index entry
func spawnTreeSave(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.SpawnTreeSave.Params(params)
	if err != nil || len(request.Subagents) == 0 {
		return rpcErr(rid, 4000, "subagents list required", nil)
	}
	sessionID := deref(request.SessionID)
	finishedAt := float64(time.Now().UnixNano()) / 1e9
	if request.FinishedAt != nil && *request.FinishedAt != 0 {
		finishedAt = *request.FinishedAt
	}
	dirName := sessionID
	if dirName == "" {
		dirName = "default"
	}
	d := spawnTreeSessionDir(dirName)
	stamp := time.Unix(0, int64(finishedAt*1e9)).UTC().Format("20060102T150405")
	path := filepath.Join(d, stamp+".json")
	var started any
	if request.StartedAt != nil && *request.StartedAt != 0 {
		started = *request.StartedAt
	}
	meta := [][2]any{{"session_id", sessionID}, {"started_at", started}, {"finished_at", finishedAt}, {"label", deref(request.Label)}}
	snapshot := jsonutil.NewObject()
	for _, kv := range meta {
		snapshot.Set(kv[0].(string), kv[1])
	}
	snapshot.Set("subagents", tuigateway.Plain(request.Subagents))
	encoded, err := jsonutil.Encode(snapshot, jsonutil.Spaced())
	panicIf(err)
	_, statErr := os.Stat(path)
	listed := statErr == nil
	if err := os.WriteFile(path, []byte(encoded), 0o644); err != nil {
		return rpcErr(rid, 5000, "spawn_tree.save failed: "+err.Error(), nil)
	}
	entry := jsonutil.NewObject()
	entry.Set("path", path)
	for _, kv := range meta {
		entry.Set(kv[0].(string), kv[1])
	}
	entry.Set("count", len(request.Subagents))
	if !listed {
		appendSpawnTreeIndexObject(d, entry)
	}
	return ok(rid, tuigateway.Plain(tuigateway.SpawnTreeSaveResult{Path: path, SessionID: sessionID}))
}

// Appends an ordered entry line to the session index
func appendSpawnTreeIndexObject(sessionDir string, entry *jsonutil.Object) {
	plain, _ := jsonutil.Plain(entry).(map[string]any)
	line, err := jsonutil.Encode(entry, jsonutil.Spaced())
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
		pkgLog().Debug("spawn_tree index append failed: " + err.Error())
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
		decoded, loadErr := jsonutil.Decode(string(data))
		panicIf(loadErr)
		if dict, isDict := jsonutil.Plain(decoded).(map[string]any); isDict {
			raw = dict
		}
	})
	count := 0
	if subagents, isList := raw["subagents"].([]any); isList {
		count = len(subagents)
	}
	sessionID := raw["session_id"]
	if !anyutil.IsTrue(sessionID) {
		sessionID = sessionDirName
	}
	finished := raw["finished_at"]
	if !anyutil.IsTrue(finished) {
		finished = float64(info.ModTime().UnixNano()) / 1e9
	}
	label := raw["label"]
	if !anyutil.IsTrue(label) {
		label = ""
	}
	return map[string]any{"path": p, "session_id": sessionID, "finished_at": finished, "started_at": raw["started_at"],
		"label": label, "count": count}
}

// Saved spawn trees of the session or every session, newest first
func spawnTreeList(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.SpawnTreeList.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	sessionID := deref(request.SessionID)
	var roots []string
	if request.CrossSession {
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
	entries := []tuigateway.SpawnTreeEntry{}
	for _, d := range roots {
		if indexed := readSpawnTreeIndex(d); len(indexed) > 0 {
			for _, item := range indexed {
				entry, isDict := item.(map[string]any)
				if !isDict {
					panic(fmt.Errorf("a spawn tree index entry must be a mapping, got %s", errchain.Kind(item)))
				}
				if p := entry["path"]; anyutil.IsTrue(p) {
					if _, err := os.Stat(anyutil.String(p)); err == nil {
						entries = append(entries, typedRow[tuigateway.SpawnTreeEntry]("spawn_tree.list", entry))
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
				entries = append(entries, typedRow[tuigateway.SpawnTreeEntry]("spawn_tree.list", entry))
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return deref(entries[i].FinishedAt) > deref(entries[j].FinishedAt) })
	limit := int64(50)
	if request.Limit != nil && *request.Limit != 0 {
		limit = *request.Limit
	}
	if limit < 0 {
		limit = max(int64(len(entries))+limit, 0)
	}
	if limit < int64(len(entries)) {
		entries = entries[:limit]
	}
	return ok(rid, tuigateway.Plain(tuigateway.SpawnTreeListResult{Entries: entries}))
}

// Loads a saved spawn tree snapshot that lies under the spawn trees root
func spawnTreeLoad(ctx context.Context, rid any, params map[string]any) map[string]any {
	request, err := tuigateway.SpawnTreeLoad.Params(params)
	if err != nil || request.Path == "" {
		return rpcErr(rid, 4000, "path required", nil)
	}
	resolved, err := pathutil.Resolve(request.Path)
	if err != nil {
		return rpcErr(rid, 4030, "path outside spawn-trees root: "+err.Error(), nil)
	}
	root, err := pathutil.Resolve(spawnTreesRoot())
	if err != nil {
		return rpcErr(rid, 4030, "path outside spawn-trees root: "+err.Error(), nil)
	}
	if rel, relErr := filepath.Rel(root, resolved); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rpcErr(rid, 4030, fmt.Sprintf("path outside spawn-trees root: %s is not in the subpath of %s",
			resolved, root), nil)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return rpcErr(rid, 5000, "spawn_tree.load failed: "+err.Error(), nil)
	}
	var snapshot tuigateway.SpawnTreeLoadResult
	if err := tuigateway.Strict(data, &snapshot); err != nil {
		return rpcErr(rid, 5000, "spawn_tree.load failed: "+err.Error(), nil)
	}
	return ok(rid, tuigateway.Plain(snapshot))
}
