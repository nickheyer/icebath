package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func resolvedDir(t *testing.T, dir string) string {
	t.Helper()
	r, err := pypath.Realpath(dir, pypath.Lax)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustWorkdirError(t *testing.T, value any, want string) {
	t.Helper()
	if got, err := normalizeWorkdir(value); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("normalizeWorkdir(%v) = %v %v, want %q", value, got, err, want)
	}
}

func TestNormalizeWorkdirEmptyStringReturnsNone(t *testing.T) {
	for _, v := range []string{"", "   "} {
		if got, err := normalizeWorkdir(v); got != nil || err != nil {
			t.Fatalf("normalizeWorkdir(%q) = %v %v", v, got, err)
		}
	}
}

func TestNormalizeWorkdirAbsoluteExistingDirReturnsResolvedStr(t *testing.T) {
	dir := t.TempDir()
	if got, err := normalizeWorkdir(dir); err != nil || deref(got) != resolvedDir(t, dir) {
		t.Fatalf("normalizeWorkdir = %v %v", got, err)
	}
}

func TestNormalizeWorkdirTildeExpands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if got, err := normalizeWorkdir("~"); err != nil || deref(got) != resolvedDir(t, dir) {
		t.Fatalf("normalizeWorkdir(~) = %v %v", got, err)
	}
}

func TestNormalizeWorkdirRelativePathRejected(t *testing.T) {
	mustWorkdirError(t, "some/relative/path", "absolute path")
}

func TestNormalizeWorkdirMissingDirRejected(t *testing.T) {
	mustWorkdirError(t, filepath.Join(t.TempDir(), "does-not-exist"), "does not exist")
}

func TestNormalizeWorkdirFileNotDirRejected(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(f, []byte("hi"), 0o600)
	mustWorkdirError(t, f, "not a directory")
}

func TestCreateJobWorkdirWorkdirStoredWhenSet(t *testing.T) {
	s, _ := newTestStore(t)
	dir := t.TempDir()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("hello"), Schedule: "every 1h", Workdir: dir})
	if got := deref(mustGet(t, s, job.ID).Workdir); got != resolvedDir(t, dir) {
		t.Fatalf("workdir %q", got)
	}
}

func TestCreateJobWorkdirCreateRejectsInvalidWorkdir(t *testing.T) {
	s, _ := newTestStore(t)
	if job, err := s.createJob(CreateJobOptions{Prompt: prompt("hello"), Schedule: "every 1h", Workdir: "not/absolute"}); err == nil {
		t.Fatalf("relative workdir accepted %+v", job)
	}
}

func TestUpdateJobWorkdirSetWorkdirViaUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	dir := t.TempDir()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	mustUpdate(t, s, job.ID, map[string]any{"workdir": dir})
	if got := deref(mustGet(t, s, job.ID).Workdir); got != resolvedDir(t, dir) {
		t.Fatalf("workdir %q", got)
	}
}

func TestUpdateJobWorkdirClearWorkdirWithNone(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h", Workdir: t.TempDir()})
	mustUpdate(t, s, job.ID, map[string]any{"workdir": nil})
	if got := mustGet(t, s, job.ID).Workdir; got != nil {
		t.Fatalf("workdir %q", *got)
	}
}

func TestUpdateJobWorkdirUpdateRejectsInvalidWorkdir(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	if updated, err := s.updateJob(job.ID, map[string]any{"workdir": "nope/relative"}); err == nil {
		t.Fatalf("relative workdir accepted %+v", updated)
	}
}
