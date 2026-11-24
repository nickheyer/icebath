package state

import (
	"unicode"

	"context"
	"fmt"
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
		"Nothing is lost. Quit every icebath process on this profile (TUI, " +
			"`icebath {profile_arg}gateway stop`, cron), run `icebath {profile_arg}doctor` — it names " +
			"any process still holding the log — then start icebath again. Do not run `doctor --fix` or delete " +
			"any state.db files while they run. Guide: `" + StorageRecoveryHelp + "`",
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
	"newer_schema": {
		"storage_newer_schema",
		"the session database was written by a newer icebath than this one",
		"Run the newer icebath that wrote it, or install that release over this one, then start it again; " +
			"this build leaves the file untouched.",
	},
	"store_dir": {
		"storage_dir_unwritable",
		"the folder that holds the session database cannot be created",
		"Make the folder named above creatable (fix its permissions or remove the file that sits in its place), " +
			"or point ICEBATH_HOME at a writable folder, then start icebath again.",
	},
	"unknown": {
		"storage_unavailable",
		"the session database could not be opened, read or written",
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
	return describeStorageKey(ctx, cause, key)
}

// Description of a cause bucket a caller already classified, as a turn result carries it
func DescribeStorageCause(ctx context.Context, cause string) StorageFailure {
	return describeStorageKey(ctx, cause, cause)
}

// Gloss, code and profile pinned action of the copy key, the unknown copy for a key without one
func describeStorageKey(ctx context.Context, cause, key string) StorageFailure {
	c, ok := storageFailures[key]
	if !ok {
		c = storageFailures["unknown"]
	}
	action := strings.ReplaceAll(c.action, "{profile_arg}", home.ProfileCLISelector(home.Dir(ctx)))
	return StorageFailure{Cause: cause, Code: c.code, Gloss: c.gloss, Action: action}
}

// Whether the error came from the session database itself, a SQLite failure or one of the store refusals
func IsStoreFailure(err error) bool {
	if _, ok := sqliteClassOf(err); ok {
		return true
	}
	_, ok := persistenceCauseByType(err)
	return ok
}

// Session store that would not open, its text naming the store, the cause and what the user does
type StoreUnavailableError struct {
	Path  string
	Cause error
	text  string
}

func (e *StoreUnavailableError) Error() string { return e.text }

func (e *StoreUnavailableError) Unwrap() error { return e.Cause }

// Open failure of the store at path described with the storage failure gloss, details and remedy
func NewStoreUnavailableError(ctx context.Context, path string, cause error) *StoreUnavailableError {
	failure := DescribeStorageFailure(ctx, cause)
	return &StoreUnavailableError{Path: path, Cause: cause, text: fmt.Sprintf("Session storage %s is unavailable: %s (%s). %s",
		path, failure.Gloss, StorageFailureDetails(cause, 200), failure.Action)}
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
	return strings.TrimRightFunc(pystr.FromRunes(runes[:min(cut, len(runes))]), unicode.IsSpace) + "..."
}
