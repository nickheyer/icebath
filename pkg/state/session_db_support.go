package state

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	sessionDBConsequence = "Sessions will not be saved until this is fixed."
	networkDriveHint     = " If the database lives on a network drive, move it to a local disk."
	networkDriveGloss    = "the session database could not be opened; it may be on a network or unsupported drive"
	networkDriveAction   = "Move it to a local disk (`icebath {profile_arg}doctor` shows where it is), then start icebath again."
)

// Default lead of the session database unavailable notice
const SessionDBUnavailablePrefix = "icebath can't open its session history right now"

// Builds the one line unavailable notice from the last init error, with a Details line when asked
func FormatSessionDBUnavailable(ctx context.Context, prefix string, details bool) string {
	profileArg := home.ProfileCLISelector(home.Dir(ctx))
	cause := GetLastInitError()
	if cause == "" {
		return prefix + ". " + sessionDBConsequence + " Run `icebath " + profileArg + "doctor` to check the " +
			"storage location."
	}
	failure := DescribeStorageFailure(ctx, cause)
	gloss, action, hint := failure.Gloss, failure.Action, ""
	if containsAny(pystr.Lower(cause), walIncompatMarkers) {
		if failure.Cause == "unknown" {
			gloss, action = networkDriveGloss, strings.ReplaceAll(networkDriveAction, "{profile_arg}", profileArg)
		} else {
			hint = networkDriveHint
		}
	}
	text := prefix + ": " + gloss + ". " + sessionDBConsequence + " " + action + hint
	if details {
		text += "\nDetails: " + StorageFailureDetails(cause, 200)
	}
	return text
}

// Database paths this process already tried to auto repair, serialized by repairAttemptMu
var (
	repairAttemptedPaths = map[string]struct{}{}
	repairAttemptMu      sync.Mutex
)

// Reports whether close time checkpoints can be switched off, always true on the linked SQLite
var closeTimeCheckpointConfigurable = func() bool {
	return sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE != 0
}

// Foreign holders of a database path through the state holder authority, swappable by tests
var foreignStateDBHoldersOf = ForeignStateDBHolders

const deleteUnreferencedSystemPromptsSQL = "DELETE FROM system_prompts WHERE NOT EXISTS (" +
	"SELECT 1 FROM sessions WHERE sessions.system_prompt_hash = system_prompts.hash) AND NOT EXISTS (" +
	"SELECT 1 FROM sessions WHERE sessions.tool_names = system_prompts.hash)"

// Deletes system prompt rows no session references by prompt hash or tool names hash
func deleteUnreferencedSystemPrompts(ctx context.Context, conn SQLiteConn) error {
	_, err := conn.ExecContext(ctx, deleteUnreferencedSystemPromptsSQL)
	return err
}

// Copies a session row, applying resolved prompt columns and lifting reset and branch markers from model_config
func sessionRowDict(row *pyjson.Object) *pyjson.Object {
	data := pyjson.NewObject()
	for _, k := range row.Keys {
		data.Set(k, row.Vals[k])
	}
	for _, column := range []string{"system_prompt", "tool_names"} {
		key := "_" + column + "_resolved"
		if resolved, ok := data.Get(key); ok {
			data.Delete(key)
			if _, present := data.Get(column); present {
				data.Set(column, resolved)
			}
		}
	}
	resetFrom, _ := data.Get("_reset_from")
	branchedFrom, _ := data.Get("_branched_from")
	if pyTruthy(resetFrom) && pyTruthy(branchedFrom) {
		return data
	}
	raw, _ := data.Get("model_config")
	var cfg *pyjson.Object
	switch v := raw.(type) {
	case string:
		if v != "" {
			if parsed, err := pyjson.Loads(v); err == nil {
				cfg, _ = parsed.(*pyjson.Object)
			}
		}
	case *pyjson.Object:
		cfg = v
	case map[string]any:
		cfg = pyjson.NewObject()
		for k, val := range v {
			cfg.Set(k, val)
		}
	}
	if cfg == nil {
		return data
	}
	for _, key := range []string{"_reset_from", "_branched_from"} {
		if current, _ := data.Get(key); pyTruthy(current) {
			continue
		}
		if value, ok := cfg.Vals[key].(string); ok && pystr.Strip(value) != "" {
			data.Set(key, pystr.Strip(value))
		}
	}
	return data
}

// Closes a partially initialized connection, logging a failed close at debug
func closeConnectionQuietly(conn SQLiteConn) {
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		slog.Debug("Could not close a SessionDB connection", "error", err)
	}
}
