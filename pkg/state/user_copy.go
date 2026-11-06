package state

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Plain language description of a persistence failure shared by every surface
type StorageFailure struct {
	Cause  string
	Code   string
	Gloss  string
	Action string
}

const storageDoctorAction = "Run `icebath {profile_arg}doctor --fix` to diagnose and repair."

type storageFailureCopy struct{ code, gloss, action string }

var storageFailures = map[string]storageFailureCopy{
	"locked": {
		"storage_locked",
		"the session database is locked by another icebath process",
		"Wait a moment and try again; if it persists, stop the other icebath process " +
			"(`icebath {profile_arg}gateway stop`).",
	},
	"disk_full": {"disk_full", "the disk is full", "Free some disk space, then try again."},
	"disk": {
		"storage_readonly",
		"the session database file is read-only or not writable",
		storageDoctorAction,
	},
	"corrupt": {
		"storage_corrupt",
		"the session database file is damaged",
		storageDoctorAction + " Recovery: `icebath {profile_arg}sessions recover --source <state.db> --inspect-only`.",
	},
	"session_row_missing": {
		"storage_session_missing",
		"this session's database row was deleted while the chat was still open and could not be recreated",
		"Try again; if it persists, run `icebath {profile_arg}doctor`.",
	},
	"fts_index": {
		"storage_index_corrupt",
		"the session search index is damaged (the messages themselves are intact)",
		"Run `icebath {profile_arg}doctor --fix` (or `icebath {profile_arg}sessions repair`) to rebuild it.",
	},
	"replaced": {
		"storage_replaced",
		"the session database file was replaced while icebath was running",
		"Stop icebath (`icebath {profile_arg}gateway stop`), run `icebath {profile_arg}doctor`, then start it again.",
	},
	"deleted_wal": {
		"storage_replaced",
		"another icebath process still holds an old copy of the session database's write-ahead log, " +
			"so icebath stopped writing to keep the file safe",
		"Nothing is lost. Quit every icebath process on this profile (Desktop app, " +
			"`icebath {profile_arg}gateway stop`, dashboard, cron), run `icebath {profile_arg}doctor` — it names " +
			"any process still holding the log — then start icebath again. Do not run `doctor --fix` or delete " +
			"any state.db files while they run. Guide: " + StorageRecoveryDocsURL,
	},
	"compression": {
		"storage_busy",
		"another process is compressing this session",
		"Send your message again once compression finishes.",
	},
	"compression_closed": {
		"storage_session_rotated",
		"this session was rotated by context compression",
		"Refresh the client (or start a new turn) and send your message again.",
	},
	"turn_lease": {
		"storage_busy",
		"another icebath process took over this session",
		"Wait for it to finish, then send your message again.",
	},
	"unknown": {
		"storage_unavailable",
		"the session database could not be opened",
		storageDoctorAction,
	},
}

// Describes a persistence failure with its cause bucket, stable code, gloss and profile pinned action
func DescribeStorageFailure(ctx context.Context, excOrStr any) StorageFailure {
	cause := ClassifyPersistenceError(excOrStr)
	key := cause
	if cause == "disk" && IsDiskFullError(excOrStr) {
		key = "disk_full"
	}
	c, ok := storageFailures[key]
	if !ok {
		c = storageFailures["unknown"]
	}
	action := strings.ReplaceAll(c.action, "{profile_arg}", home.ProfileCLISelector(home.Dir(ctx)))
	return StorageFailure{Cause: cause, Code: c.code, Gloss: c.gloss, Action: action}
}

// Whitespace flattened raw cause cut to limit code points with an ellipsis
func StorageFailureDetails(excOrStr any, limit int) string {
	raw := ""
	if pyTruthy(excOrStr) {
		raw = errText(excOrStr)
	}
	text := strings.Join(pystr.SplitWhitespace(raw, -1), " ")
	if pystr.Len(text) <= limit {
		return text
	}
	runes := pystr.Runes(text)
	cut := limit - 3
	if cut < 0 {
		cut = max(len(runes)+cut, 0)
	}
	return pystr.Rstrip(pystr.FromRunes(runes[:min(cut, len(runes))])) + "..."
}
