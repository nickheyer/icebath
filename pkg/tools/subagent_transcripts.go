package tools

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Model config keys of a child's session row naming its subagent id, live log and spawning session
const (
	SubagentIDConfigKey     = "_subagent_id"
	LiveTranscriptConfigKey = "_live_transcript"
	delegateFromConfigKey   = "_delegate_from"
)

// Parent links a finished child's log is traced through before the walk gives up
const transcriptOwnerHops = 8

// Session store reads the finished child logs need
type transcriptStore interface {
	ReadAll(ctx context.Context, query string, args ...any) ([]*jsonutil.Object, error)
	GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error)
}

// One model config key of a stored row read through the JSON guard every session query uses
func modelConfigField(key string) string {
	return "json_extract(CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END, '$." + key + "')"
}

// Model config of a stored session row
func rowModelConfig(row *jsonutil.Object) map[string]any {
	raw, _ := row.Get("model_config")
	return state.ParseModelConfig(raw)
}

// Log a delegated child of the conversation wrote at any depth, empty when no child of it stored one
func FinishedSubagentTranscript(ctx context.Context, db any, ownerSessionID, subagentID string) (string, error) {
	store, ok := db.(transcriptStore)
	if !ok || db == (*state.SessionDB)(nil) || ownerSessionID == "" || subagentID == "" {
		return "", nil
	}
	ids, err := store.GetCompressionLineage(ctx, ownerSessionID)
	if err != nil {
		return "", err
	}
	lineage := map[string]bool{ownerSessionID: true}
	for _, id := range ids {
		lineage[id] = true
	}
	rows, err := store.ReadAll(ctx, "SELECT id, model_config FROM sessions WHERE "+modelConfigField(SubagentIDConfigKey)+" = ?", subagentID)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		cfg := rowModelConfig(row)
		path, _ := cfg[LiveTranscriptConfigKey].(string)
		if path == "" {
			continue
		}
		owned, err := spawnedWithin(ctx, store, cfg, lineage)
		if err != nil {
			return "", err
		}
		if owned {
			return path, nil
		}
	}
	return "", nil
}

// Whether the chain of spawning sessions above a child row reaches the conversation
func spawnedWithin(ctx context.Context, store transcriptStore, cfg map[string]any, lineage map[string]bool) (bool, error) {
	for range transcriptOwnerHops {
		from, _ := cfg[delegateFromConfigKey].(string)
		if from == "" {
			return false, nil
		}
		if lineage[from] {
			return true, nil
		}
		rows, err := store.ReadAll(ctx, "SELECT model_config FROM sessions WHERE id = ?", from)
		if err != nil || len(rows) == 0 {
			return false, err
		}
		cfg = rowModelConfig(rows[0])
	}
	return false, nil
}

// Whether a stored child session still names a log inside the delegation directory
func LiveTranscriptDirReferenced(ctx context.Context, db any, dir string) (bool, error) {
	store, ok := db.(transcriptStore)
	if !ok || db == (*state.SessionDB)(nil) {
		return false, nil
	}
	rows, err := store.ReadAll(ctx, "SELECT 1 AS one FROM sessions WHERE instr("+modelConfigField(LiveTranscriptConfigKey)+", ?) = 1 LIMIT 1",
		dir+string(os.PathSeparator))
	return len(rows) > 0, err
}
