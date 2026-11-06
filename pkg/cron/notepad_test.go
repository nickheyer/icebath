package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Notepad over a temp database the way the fixture points NOTEPAD_FILE at tmp_path
func testNotepad(t *testing.T) (*Store, *Notepad) {
	t.Helper()
	s, _ := newTestStore(t)
	s.notepadFile = filepath.Join(t.TempDir(), "cron", "notepad.db")
	return s, openNotepad(s)
}

func mustSetNote(t *testing.T, n *Notepad, job, key, value string) {
	t.Helper()
	if _, err := n.Set(job, key, value); err != nil {
		t.Fatal(err)
	}
}

func noteValue(t *testing.T, n *Notepad, job, key string) *string {
	t.Helper()
	v, err := n.Get(job, key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func noteKeys(t *testing.T, n *Notepad, job string) []string {
	t.Helper()
	notes, err := n.List(job)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, note := range notes {
		k, _ := note.Get("key")
		keys = append(keys, k.(string))
	}
	return keys
}

func TestNotepadSetThenGetRoundtrip(t *testing.T) {
	_, n := testNotepad(t)
	entry, err := n.Set("job-1", "cursor", "page=7")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(entry.Keys, ","); got != "job_id,key,value,updated_at" {
		t.Fatalf("entry keys %s", got)
	}
	if v := noteValue(t, n, "job-1", "cursor"); v == nil || *v != "page=7" {
		t.Fatalf("roundtrip %v", v)
	}
	if v := noteValue(t, n, "job-1", "missing"); v != nil {
		t.Fatalf("missing key %q", *v)
	}
}

func TestNotepadOverwriteAndDelete(t *testing.T) {
	_, n := testNotepad(t)
	mustSetNote(t, n, "job-1", "cursor", "page=1")
	mustSetNote(t, n, "job-1", "cursor", "page=2")
	if v := noteValue(t, n, "job-1", "cursor"); *v != "page=2" {
		t.Fatalf("overwrite kept %q", *v)
	}
	if keys := noteKeys(t, n, "job-1"); len(keys) != 1 {
		t.Fatalf("overwrite duplicated rows %v", keys)
	}
	if ok, err := n.Delete("job-1", "cursor"); err != nil || !ok {
		t.Fatalf("delete %v %v", ok, err)
	}
	if noteValue(t, n, "job-1", "cursor") != nil {
		t.Fatal("deleted key still readable")
	}
	if ok, err := n.Delete("job-1", "nope"); err != nil || ok {
		t.Fatalf("missing delete %v %v", ok, err)
	}
}

func TestNotepadListSortedAndJobsIsolated(t *testing.T) {
	_, n := testNotepad(t)
	mustSetNote(t, n, "job-1", "zeta", "1")
	mustSetNote(t, n, "job-1", "alpha", "2")
	mustSetNote(t, n, "job-1", "mid", "3")
	mustSetNote(t, n, "job-2", "alpha", "other")
	if got := strings.Join(noteKeys(t, n, "job-1"), ","); got != "alpha,mid,zeta" {
		t.Fatalf("sorted keys %s", got)
	}
	notes, _ := n.List("job-1")
	if text, _ := pyjson.Dumps(notes[0]); !strings.HasPrefix(text, `{"job_id": "job-1", "key": "alpha", "value": "2", "updated_at": `) {
		t.Fatalf("row shape %s", text)
	}
	if v := noteValue(t, n, "job-2", "alpha"); *v != "other" {
		t.Fatalf("job isolation %q", *v)
	}
	if v := noteValue(t, n, "job-1", "alpha"); *v != "2" {
		t.Fatalf("job isolation %q", *v)
	}
}

func TestClearNotepadRemovesOnlyThatJob(t *testing.T) {
	_, n := testNotepad(t)
	mustSetNote(t, n, "job-1", "a", "1")
	mustSetNote(t, n, "job-1", "b", "2")
	mustSetNote(t, n, "job-2", "a", "keep")
	if count, err := n.Clear("job-1"); err != nil || count != 2 {
		t.Fatalf("clear %d %v", count, err)
	}
	if keys := noteKeys(t, n, "job-1"); len(keys) != 0 {
		t.Fatalf("cleared job kept %v", keys)
	}
	if v := noteValue(t, n, "job-2", "a"); *v != "keep" {
		t.Fatal("sibling job cleared")
	}
}

func TestClearNotepadNoopWithoutDBFile(t *testing.T) {
	_, n := testNotepad(t)
	if count, err := n.Clear("job-1"); err != nil || count != 0 {
		t.Fatalf("clear %d %v", count, err)
	}
	if _, err := os.Stat(n.path); !os.IsNotExist(err) {
		t.Fatalf("clear created the database: %v", err)
	}
}

func TestNotepadProfileOverrideRoutesWritesToCurrentHome(t *testing.T) {
	root := t.TempDir()
	a := openCronStore(filepath.Join(root, "profile-a"), nil)
	b := openCronStore(filepath.Join(root, "profile-b"), nil)
	_ = openNotepad(a)
	mustSetNote(t, openNotepad(b), "job-1", "cursor", "page=7")
	if _, err := os.Stat(filepath.Join(root, "profile-b", "cron", "notepad.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "profile-a", "cron", "notepad.db")); !os.IsNotExist(err) {
		t.Fatalf("profile a notepad written: %v", err)
	}
}

func TestRemoveJobClearsNotepad(t *testing.T) {
	s, _ := newTestStore(t)
	n := openNotepad(s)
	job, err := s.createJob(CreateJobOptions{Prompt: prompt("Check the feed"), Schedule: "every 1h"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.createJob(CreateJobOptions{Prompt: prompt("Other job"), Schedule: "every 2h"})
	if err != nil {
		t.Fatal(err)
	}
	mustSetNote(t, n, job.ID, "cursor", "page=7")
	mustSetNote(t, n, other.ID, "cursor", "keep")
	if ok, err := s.removeJob(context.Background(), job.ID); err != nil || !ok {
		t.Fatalf("remove %v %v", ok, err)
	}
	if keys := noteKeys(t, n, job.ID); len(keys) != 0 {
		t.Fatalf("removed job orphaned %v", keys)
	}
	if v := noteValue(t, n, other.ID, "cursor"); v == nil || *v != "keep" {
		t.Fatal("sibling notepad touched")
	}
}

func TestRemoveJobSurvivesNotepadFailure(t *testing.T) {
	s, _ := newTestStore(t)
	job, err := s.createJob(CreateJobOptions{Prompt: prompt("Check the feed"), Schedule: "every 1h"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.cronDir, "notepad.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := clearNotepad(s, job.ID); err == nil {
		t.Fatal("a directory in place of the notepad opened")
	}
	if ok, err := s.removeJob(context.Background(), job.ID); err != nil || !ok {
		t.Fatalf("remove %v %v", ok, err)
	}
	if got, err := s.getJob(job.ID); err != nil || got != nil {
		t.Fatalf("job survived removal %v %v", got, err)
	}
}

func TestNotepadCaps(t *testing.T) {
	_, n := testNotepad(t)
	_, err := n.Set("job-1", "big", strings.Repeat("x", NotepadMaxValueBytes+1))
	var rejected *NoteRejectedError
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "value too large") {
		t.Fatalf("oversized value %v", err)
	}
	if noteValue(t, n, "job-1", "big") != nil {
		t.Fatal("refused value stored")
	}
	exact := strings.Repeat("x", NotepadMaxValueBytes)
	mustSetNote(t, n, "job-1", "exact", exact)
	if v := noteValue(t, n, "job-1", "exact"); *v != exact {
		t.Fatal("value at cap not stored")
	}
	if _, err := n.Set("job-1", strings.Repeat("k", NotepadMaxKeyChars+1), "v"); err == nil || !strings.Contains(err.Error(), "key too long") {
		t.Fatalf("long key %v", err)
	}
	mustSetNote(t, n, "job-1", strings.Repeat("é", NotepadMaxKeyChars), "v")
	if _, err := n.Set("job-1", "", "v"); err == nil || !strings.Contains(err.Error(), "key must be non-empty") {
		t.Fatalf("empty key %v", err)
	}
	if _, err := n.Set("", "k", "v"); err == nil || err.Error() != "job_id must be non-empty" {
		t.Fatalf("empty job %v", err)
	}
}

func TestNotepadRefusesLoneSurrogates(t *testing.T) {
	_, n := testNotepad(t)
	lone := pystr.SurrogateEscapeDecode([]byte{'a', 0xff})
	var encodeErr *pystr.UnicodeEncodeError
	if _, err := n.Set("job-1", "k", lone); !errors.As(err, &encodeErr) {
		t.Fatalf("surrogate value %v", err)
	}
	if _, err := n.Set("job-1", lone, "v"); !errors.As(err, &encodeErr) {
		t.Fatalf("surrogate key %v", err)
	}
	if _, err := n.Get(lone, "k"); !errors.As(err, &encodeErr) {
		t.Fatalf("surrogate job id %v", err)
	}
	if keys := noteKeys(t, n, "job-1"); len(keys) != 0 {
		t.Fatalf("refused writes stored %v", keys)
	}
}

func TestNotepadPerJobTotalCap(t *testing.T) {
	prev := NotepadMaxJobTotalBytes
	NotepadMaxJobTotalBytes = 100
	t.Cleanup(func() { NotepadMaxJobTotalBytes = prev })
	_, n := testNotepad(t)
	mustSetNote(t, n, "job-1", "a", strings.Repeat("x", 60))
	_, err := n.Set("job-1", "b", strings.Repeat("x", 60))
	if err == nil || err.Error() != "notepad full: job 'job-1' would exceed 100 bytes total; delete unused keys first" {
		t.Fatalf("total cap %v", err)
	}
	mustSetNote(t, n, "job-1", "a", strings.Repeat("x", 90))
	if v := noteValue(t, n, "job-1", "a"); *v != strings.Repeat("x", 90) {
		t.Fatal("overwrite within budget refused")
	}
	mustSetNote(t, n, "other-job", "a", strings.Repeat("x", 90))
}

func TestNotepadRenderSection(t *testing.T) {
	_, n := testNotepad(t)
	if got := n.RenderSection("job-1"); got != "" {
		t.Fatalf("empty notepad rendered %q", got)
	}
	mustSetNote(t, n, "job-1", "watermark", "2026-01-01")
	mustSetNote(t, n, "job-1", "cursor", "page=7")
	want := "## Job notepad (persistent across runs)\nThis durable scratchpad survives between scheduled runs of this job. Update it via the CLI, e.g.:\n" +
		"`icebath cron notepad job-1 set <key> <value>` (also: get/delete/list; `icebath cron notepad job-1 delete <key>` removes an entry).\n\n" +
		"- cursor: page=7\n- watermark: 2026-01-01\n\n"
	if got := n.RenderSection("job-1"); got != want {
		t.Fatalf("section\n%q\nwant\n%q", got, want)
	}
	prev := notepadLister
	notepadLister = func(*Notepad, string) ([]*pyjson.Object, error) { panic("db unavailable") }
	t.Cleanup(func() { notepadLister = prev })
	if got := n.RenderSection("job-1"); got != "" {
		t.Fatalf("failed read rendered %q", got)
	}
}

func TestStoresOpenThroughTheCanonicalOpenDB(t *testing.T) {
	s, _ := newTestStore(t)
	dir := t.TempDir()
	s.notepadFile = filepath.Join(dir, "notepad_file.db")
	s.executionsFile = filepath.Join(dir, "executions_file.db")
	real := state.OpenDB
	var labels []string
	state.OpenDB = func(ctx context.Context, path, label string, opts ...state.OpenDBOption) (state.SQLiteConn, error) {
		labels = append(labels, label)
		return real(ctx, path, label, opts...)
	}
	t.Cleanup(func() { state.OpenDB = real })
	ctx := context.Background()
	openers := []func(context.Context) (state.SQLiteConn, error){openNotepad(s).connect, openExecutionsLedger(s).connect}
	for i, open := range openers {
		conn, err := open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var timeout int
		err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout)
		conn.Close()
		if err != nil || timeout <= 0 {
			t.Fatalf("opener %d busy_timeout %d %v", i, timeout, err)
		}
		if len(labels) != i+1 || labels[i] == "" {
			t.Fatalf("opener %d labels %v", i, labels)
		}
	}
	if labels[0] != "cron/notepad.db" || labels[1] != "cron/executions.db" {
		t.Fatalf("labels %v", labels)
	}
}

func TestNotepadOpensInAFreshProcessStore(t *testing.T) {
	s := openCronStore(filepath.Join(t.TempDir(), "home"), nil)
	n := openNotepad(s)
	mustSetNote(t, n, "job", "k", "v")
	if keys := noteKeys(t, n, "job"); len(keys) != 1 {
		t.Fatalf("fresh store notepad %v", keys)
	}
	info, err := os.Stat(s.cronDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cron dir mode %v %v", info, err)
	}
}
