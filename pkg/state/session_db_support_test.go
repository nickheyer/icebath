package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDBUnavailableIsOneLineForChatSurfacesByDefault(t *testing.T) {
	setLastInitError("OperationalError: database is locked")
	defer setLastInitError("")
	text := FormatSessionDBUnavailable(context.Background(), "Cannot resume", false)
	if strings.Contains(text, "\n") || strings.Contains(text, "Details:") || !strings.HasPrefix(text, "Cannot resume:") {
		t.Fatalf("text %q", text)
	}
}

func TestDBUnavailableCommandsArePinnedToTheFailingProfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath", "profiles", "research"))
	selector := "-p research "
	setLastInitError("")
	noCause := FormatSessionDBUnavailable(context.Background(), SessionDBUnavailablePrefix, false)
	setLastInitError("OperationalError: locking protocol")
	network := FormatSessionDBUnavailable(context.Background(), SessionDBUnavailablePrefix, false)
	setLastInitError("")
	for _, text := range []string{noCause, network} {
		if !strings.Contains(text, "`icebath "+selector+"doctor`") || strings.Contains(text, "`icebath doctor`") ||
			strings.Contains(text, "{profile_arg}") {
			t.Errorf("text %q not pinned to the research profile", text)
		}
	}
}

func TestDBUnavailableDetailsLineCarriesRawCauseOnlyBelowTheLead(t *testing.T) {
	setLastInitError("OperationalError: database is locked")
	text := FormatSessionDBUnavailable(context.Background(), SessionDBUnavailablePrefix, true)
	setLastInitError("")
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "OperationalError") || !strings.HasPrefix(lines[1], "Details: ") ||
		!strings.Contains(lines[1], "database is locked") {
		t.Fatalf("text %q", text)
	}
	noCause := FormatSessionDBUnavailable(context.Background(), SessionDBUnavailablePrefix, true)
	if strings.Contains(noCause, "\n") || strings.Contains(noCause, "Details:") {
		t.Fatalf("no cause text %q", noCause)
	}
	want := SessionDBUnavailablePrefix + ". Sessions will not be saved until this is fixed. Run `icebath doctor` to check the storage location."
	if noCause != want {
		t.Fatalf("no cause text %q, want %q", noCause, want)
	}
}

func TestDBUnavailableNetworkDriveCauseDoesNotOfferDoctorFix(t *testing.T) {
	setLastInitError("OperationalError: locking protocol")
	text := FormatSessionDBUnavailable(context.Background(), SessionDBUnavailablePrefix, true)
	setLastInitError("")
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "doctor --fix") || strings.Contains(lines[0], "locking protocol") ||
		!strings.Contains(lines[1], "locking protocol") {
		t.Fatalf("text %q", text)
	}
	if !strings.Contains(lines[0], networkDriveGloss) {
		t.Fatalf("lead %q lacks the network drive gloss", lines[0])
	}
}

func TestDBUnavailableKnownCauseOnNetworkMarkerGetsTheHint(t *testing.T) {
	setLastInitError("OperationalError: disk I/O error, database is locked")
	text := FormatSessionDBUnavailable(context.Background(), "Cannot resume", false)
	setLastInitError("")
	if !strings.HasSuffix(text, networkDriveHint) || !strings.Contains(text, "locked by another icebath process") {
		t.Fatalf("text %q", text)
	}
}

func TestCloseTimeCheckpointIsConfigurable(t *testing.T) {
	if !closeTimeCheckpointConfigurable() {
		t.Fatal("the linked SQLite exposes SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE")
	}
}

func TestSessionRowDictResolvesPromptsAndLiftsMarkers(t *testing.T) {
	row := msgObj("id", "s1", "system_prompt", "hash", "_system_prompt_resolved", "full prompt",
		"_tool_names_resolved", "tools", "model_config", `{"_reset_from": "  parent  ", "_branched_from": " "}`,
		"_reset_from", nil, "_branched_from", nil)
	got := sessionRowDict(row)
	if got == row {
		t.Fatal("the row was not copied")
	}
	if v, _ := got.Get("system_prompt"); v != "full prompt" {
		t.Fatalf("system_prompt %v", v)
	}
	if _, ok := got.Get("tool_names"); ok {
		t.Fatal("tool_names appeared without its column")
	}
	if _, ok := got.Get("_system_prompt_resolved"); ok {
		t.Fatal("resolved helper column kept")
	}
	if v, _ := got.Get("_reset_from"); v != "parent" {
		t.Fatalf("_reset_from %v", v)
	}
	if v, _ := got.Get("_branched_from"); v != nil {
		t.Fatalf("blank _branched_from lifted to %v", v)
	}
	if got.Keys[1] != "system_prompt" {
		t.Fatalf("resolved prompt moved its column, keys %v", got.Keys)
	}
	kept := sessionRowDict(msgObj("_reset_from", "a", "_branched_from", "b", "model_config", `{"_reset_from": "x"}`))
	if v, _ := kept.Get("_reset_from"); v != "a" {
		t.Fatalf("existing markers overwritten: %v", v)
	}
	bad := sessionRowDict(msgObj("model_config", "{not json"))
	if bad.Len() != 1 {
		t.Fatalf("invalid model_config added keys %v", bad.Keys)
	}
	dict := sessionRowDict(msgObj("model_config", msgObj("_branched_from", "fork")))
	if v, _ := dict.Get("_branched_from"); v != "fork" {
		t.Fatalf("dict model_config not lifted: %v", v)
	}
}

func TestDeleteUnreferencedSystemPromptsKeepsToolNameReferences(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE TABLE system_prompts (hash TEXT PRIMARY KEY, prompt TEXT)",
		"CREATE TABLE sessions (id TEXT, system_prompt_hash TEXT, tool_names TEXT)",
		"INSERT INTO system_prompts VALUES ('a', 'pa'), ('b', 'pb'), ('c', 'pc')",
		"INSERT INTO sessions VALUES ('s1', 'a', NULL), ('s2', NULL, 'b')",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := deleteUnreferencedSystemPrompts(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var hashes string
	if err := db.QueryRowContext(ctx, "SELECT group_concat(hash, ',') FROM (SELECT hash FROM system_prompts ORDER BY hash)").Scan(&hashes); err != nil {
		t.Fatal(err)
	}
	if hashes != "a,b" {
		t.Fatalf("remaining prompts %q", hashes)
	}
}

type failingCloseConn struct{ SQLiteConn }

func (failingCloseConn) Close() error { return errors.New("close failed") }

func TestCloseConnectionQuietlySwallowsCloseErrors(t *testing.T) {
	closeConnectionQuietly(nil)
	records := captureSlog(t)
	closeConnectionQuietly(failingCloseConn{})
	if !strings.Contains(records.String(), "Could not close a SessionDB connection") {
		t.Fatalf("debug record missing: %q", records.String())
	}
}

func TestFalsyCausesHaveEmptyDetailsAndEmptyObjectsAreFalsy(t *testing.T) {
	for _, v := range []any{nil, "", 0, false, []any{}} {
		if got := StorageFailureDetails(v, 200); got != "" {
			t.Errorf("StorageFailureDetails(%#v) = %q", v, got)
		}
	}
	empty := sessionRowDict(msgObj("_reset_from", msgObj(), "model_config", `{"_reset_from": "p"}`))
	if v, _ := empty.Get("_reset_from"); v != "p" {
		t.Fatalf("an empty dict marker was treated as set: %v", v)
	}
}
