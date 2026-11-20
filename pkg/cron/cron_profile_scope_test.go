package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func TestForeignProcessTickPersistsOwningProfileOffset(t *testing.T) {
	t.Setenv("ICEBATH_TIMEZONE", "")
	os.Unsetenv("ICEBATH_TIMEZONE")
	config.ResetTimezoneCache()
	t.Cleanup(config.ResetTimezoneCache)
	root := t.TempDir()
	backend, profileA := filepath.Join(root, "backend"), filepath.Join(root, "north-caribbean")
	os.MkdirAll(backend, 0o700)
	os.MkdirAll(profileA, 0o700)
	os.WriteFile(filepath.Join(backend, "config.yaml"), []byte("timezone: UTC\n"), 0o600)
	os.WriteFile(filepath.Join(profileA, "config.yaml"), []byte("timezone: America/New_York\n"), 0o600)
	t.Setenv("ICEBATH_HOME", backend)
	if _, off := config.TimeNow(context.Background()).Zone(); off != 0 {
		t.Fatalf("backend process zone offset %d", off)
	}
	ctx := home.WithOverride(context.Background(), profileA)
	s := cronStoreFrom(withCronStore(ctx, openCronStore(profileA, nil)))
	mustCreate(t, s, CreateJobOptions{Name: prompt("daily-2pm"), Prompt: prompt("x"), Schedule: "0 14 * * *"})
	jobs, err := s.LoadJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v %v", jobIDs(jobs), err)
	}
	next, err := ParseISO(deref(jobs[0].NextRunAt))
	if err != nil {
		t.Fatal(err)
	}
	newYork := mustLoadZone(t, "America/New_York")
	naive := next.Time
	wallNY := time.Date(naive.Year(), naive.Month(), naive.Day(), naive.Hour(), naive.Minute(), naive.Second(), 0, newYork)
	_, want := wallNY.Zone()
	if _, off := next.Time.Zone(); off != want {
		t.Fatalf("next_run_at %s persisted with foreign offset %d instead of the owning profile's %d", *jobs[0].NextRunAt, off, want)
	}
	if next.Time.Hour() != 14 || next.Time.Minute() != 0 {
		t.Fatalf("wall clock %s", *jobs[0].NextRunAt)
	}
}

func TestCronStorageAnchorsAtProfileHome(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	root := filepath.Join(tmp, ".icebath")
	profileHome := filepath.Join(root, "profiles", "coder")
	os.MkdirAll(profileHome, 0o700)
	t.Setenv("ICEBATH_HOME", profileHome)
	resolve := func(p string) string {
		r, _ := pypath.Realpath(p, pypath.Lax)
		return r
	}
	if got := resolve(home.Dir(context.Background())); got != resolve(profileHome) {
		t.Fatalf("home %s", got)
	}
	if got := resolve(home.Root()); got != resolve(root) {
		t.Fatalf("root %s", got)
	}
	s := cronStoreFrom(context.Background())
	if resolve(s.Home()) != resolve(profileHome) || s.JobsFile() != filepath.Join(resolve(profileHome), "cron", "jobs.json") {
		t.Fatalf("store home %s jobs %s", s.Home(), s.JobsFile())
	}
	if s.JobsFile() == filepath.Join(resolve(root), "cron", "jobs.json") {
		t.Fatal("the shared root store anchored a profile's jobs")
	}
}
