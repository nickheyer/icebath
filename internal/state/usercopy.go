package state

import (
	"context"
	"strings"
)

// Plain language description of a persistence failure every surface shows alike
type Failure struct {
	Cause Cause
	// Stable machine readable code a client can attach a doctor action to
	Code string
	// What happened, one clause starting lower case
	Gloss string
	// What to do, one sentence naming the exact command
	Action string
}

// Doctor remedy most failures share
const doctorAction = "Run `icebath {profile_arg}doctor --fix` to diagnose and repair."

// Code, gloss and action per bucket, disk split into full and read-only at lookup
var failureCopy = map[string][3]string{
	"locked": {"storage_locked", "the session database is locked by another icebath process",
		"Wait a moment and try again; if it persists, stop the other icebath process (`icebath {profile_arg}gateway stop`)."},
	"disk_full": {"disk_full", "the disk is full", "Free some disk space, then try again."},
	"disk":      {"storage_readonly", "the session database file is read-only or not writable", doctorAction},
	"corrupt": {"storage_corrupt", "the session database file is damaged",
		doctorAction + " Recovery: `icebath {profile_arg}sessions recover --source <state.db> --inspect-only`."},
	"session_row_missing": {"storage_session_missing",
		"this session's database row was deleted while the chat was still open and could not be recreated",
		"Try again; if it persists, run `icebath {profile_arg}doctor`."},
	"fts_index": {"storage_index_corrupt", "the session search index is damaged (the messages themselves are intact)",
		"Run `icebath {profile_arg}doctor --fix` (or `icebath {profile_arg}sessions repair`) to rebuild it."},
	"replaced": {"storage_replaced", "the session database file was replaced while icebath was running",
		"Stop icebath (`icebath {profile_arg}gateway stop`), run `icebath {profile_arg}doctor`, then start it again."},
	"deleted_wal": {"storage_replaced",
		"another icebath process still holds an old copy of the session database's write-ahead log, " +
			"so icebath stopped writing to keep the file safe",
		"Nothing is lost. Quit every icebath process on this profile (Desktop app, " +
			"`icebath {profile_arg}gateway stop`, dashboard, cron), run `icebath {profile_arg}doctor` — it names " +
			"any process still holding the log — then start icebath again. Do not run `doctor --fix` or delete " +
			"any state.db files while they run. Guide: " + RecoveryDocsURL},
	"compression": {"storage_busy", "another process is compressing this session",
		"Send your message again once compression finishes."},
	"compression_closed": {"storage_session_rotated", "this session was rotated by context compression",
		"Refresh the client (or start a new turn) and send your message again."},
	"turn_lease": {"storage_busy", "another icebath process took over this session",
		"Wait for it to finish, then send your message again."},
	"unknown": {"storage_unavailable", "the session database could not be opened", doctorAction},
}

// Consequence every unavailable-store notice states
const unavailableConsequence = "Sessions will not be saved until this is fixed."

// Network drive wording for a cause that names a WAL-incompatible filesystem
const (
	networkDriveHint   = " If the database lives on a network drive, move it to a local disk."
	networkDriveGloss  = "the session database could not be opened; it may be on a network or unsupported drive"
	networkDriveAction = "Move it to a local disk (`icebath {profile_arg}doctor` shows where it is), then start icebath again."
)

// Prefix of the unavailable-store notice when the caller supplies none
const unavailablePrefix = "icebath can't open its session history right now"

// Plain language failure for err, its commands pinned to the profile of the home in effect for ctx
func (e *Engine) DescribeFailure(ctx context.Context, err error) Failure {
	cause := Classify(err)
	key := string(cause)
	if cause == CauseDisk && IsDiskFull(err) {
		key = "disk_full"
	}
	entry, ok := failureCopy[key]
	if !ok {
		entry = failureCopy["unknown"]
	}
	profile := e.profileArg(ctx)
	return Failure{Cause: cause, Code: entry[0], Gloss: entry[1], Action: strings.ReplaceAll(entry[2], "{profile_arg}", profile)}
}

// Raw cause for a trailing details line, whitespace collapsed and cut to two hundred characters
func FailureDetails(err error) string {
	if err == nil {
		return ""
	}
	text := strings.Join(strings.Fields(errText(err)), " ")
	if r := []rune(text); len(r) > 200 {
		return strings.TrimRight(string(r[:197]), " \t\r\n") + "..."
	}
	return text
}

// One line notice that the store cannot open, explaining the last failure, details adding its raw cause
func (e *Engine) UnavailableMessage(ctx context.Context, prefix string, details bool) string {
	if prefix == "" {
		prefix = unavailablePrefix
	}
	profile := e.profileArg(ctx)
	cause := e.LastInitError()
	if cause == "" {
		return prefix + ". " + unavailableConsequence + " Run `icebath " + profile + "doctor` to check the storage location."
	}
	causeErr := textError(cause)
	failure := e.DescribeFailure(ctx, causeErr)
	gloss, action, hint := failure.Gloss, failure.Action, ""
	if containsAny(strings.ToLower(cause), walIncompatMarkers) {
		if failure.Cause == CauseUnknown {
			gloss, action = networkDriveGloss, strings.ReplaceAll(networkDriveAction, "{profile_arg}", profile)
		} else {
			hint = networkDriveHint
		}
	}
	text := prefix + ": " + gloss + ". " + unavailableConsequence + " " + action + hint
	if details {
		text += "\nDetails: " + FailureDetails(causeErr)
	}
	return text
}

// Error carrying only text, the form a cause takes after it was recorded or passed over RPC
type textError string

// Recorded text
func (t textError) Error() string { return string(t) }
