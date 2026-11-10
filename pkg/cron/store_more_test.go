package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func TestLateEnvRepointScopesStoreLateEnvRepointScopesStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	s := cronStoreFrom(context.Background())
	resolved, _ := pypath.Realpath(root, pypath.Lax)
	want := filepath.Join(resolved, "cron")
	if s.CronDir() != want || s.JobsFile() != filepath.Join(want, "jobs.json") || s.OutputDir() != filepath.Join(want, "output") {
		t.Fatalf("paths %s %s %s", s.CronDir(), s.JobsFile(), s.OutputDir())
	}
}

func TestLateEnvRepointScopesStoreUseCronStoreOverrideStillWins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "env-home"))
	ctx := withCronStore(context.Background(), openCronStore(filepath.Join(root, "override-home"), nil))
	resolved, _ := pypath.Realpath(filepath.Join(root, "override-home"), pypath.Lax)
	if got := cronStoreFrom(ctx).JobsFile(); got != filepath.Join(resolved, "cron", "jobs.json") {
		t.Fatalf("override %s", got)
	}
}

func TestLateEnvRepointScopesStoreHeartbeatDoesNotRecreateDeletedNamedProfile(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "profiles"), 0o700)
	deleted := filepath.Join(root, "profiles", "deleted")
	openCronStore(deleted, nil).RecordTickerHeartbeat(context.Background(), false)
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted profile recreated: %v", err)
	}
}

func TestLateEnvRepointScopesStoreHeartbeatInitializesExistingNamedProfile(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "profiles", "active")
	os.MkdirAll(active, 0o700)
	openCronStore(active, nil).RecordTickerHeartbeat(context.Background(), false)
	if st, err := os.Stat(filepath.Join(active, "cron", "ticker_heartbeat")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("heartbeat missing in live profile: %v", err)
	}
}

func TestLateEnvRepointScopesStorePublicIoAfterLateEnvRepointLeavesOldFileUntouched(t *testing.T) {
	root := t.TempDir()
	oldStore := openCronStore(filepath.Join(root, "import-time-home"), nil)
	os.MkdirAll(oldStore.CronDir(), 0o700)
	sentinel := `[{"id": "sentinel-do-not-touch"}]`
	os.WriteFile(oldStore.JobsFile(), []byte(sentinel), 0o600)
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "late-home"))
	late := cronStoreFrom(context.Background())
	job, _ := jobFromMap(map[string]any{"id": "lateenvjob01", "name": "late-env", "prompt": nil, "schedule_display": nil,
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true})
	if err := late.SaveJobs([]*Job{job}, nil, false); err != nil {
		t.Fatal(err)
	}
	loaded, _ := cronStoreFrom(context.Background()).LoadJobs()
	if ids := jobIDs(loaded); len(ids) != 1 || ids[0] != "lateenvjob01" {
		t.Fatalf("loaded %v", ids)
	}
	resolved, _ := pypath.Realpath(filepath.Join(root, "late-home"), pypath.Lax)
	if st, err := os.Stat(filepath.Join(resolved, "cron", "jobs.json")); err != nil || !st.Mode().IsRegular() {
		t.Fatal("late home store not written")
	}
	if raw, _ := os.ReadFile(oldStore.JobsFile()); string(raw) != sentinel {
		t.Fatalf("old store touched: %s", raw)
	}
}

func jobFromMap(m map[string]any) (*Job, error) {
	var j Job
	text, err := pyjson.Dumps(m)
	if err != nil {
		return nil, err
	}
	return &j, j.UnmarshalJSON([]byte(text))
}

// Profiles dir holding no deleted profile home, the home a deleted named profile had
func deletedProfileHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "profiles"), 0o700)
	return filepath.Join(root, "profiles", "deleted")
}

func mustFailClosed(t *testing.T, deleted, dir string) {
	t.Helper()
	if err := ensureCronDir(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: %v", dir, err)
	}
	if _, err := os.Stat(deleted); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted profile home recreated")
	}
}

func mustCreateDir(t *testing.T, dir string) {
	t.Helper()
	if err := ensureCronDir(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("%s not created", dir)
	}
}

func TestEnsureCronDirWidenedEnsureCronDirNamedProfileSubdirFailsClosed(t *testing.T) {
	deleted := deletedProfileHome(t)
	mustFailClosed(t, deleted, filepath.Join(deleted, "cron", "output", "job_123"))
}

func TestEnsureCronDirWidenedEnsureCronDirDefaultHomeCreatesSubdir(t *testing.T) {
	defaultHome := filepath.Join(t.TempDir(), "default_home")
	os.MkdirAll(defaultHome, 0o700)
	mustCreateDir(t, filepath.Join(defaultHome, "cron", "output", "job_123"))
}

func TestEnsureCronDirWidenedEnsureCronDirNamedProfileCronDirFailsClosed(t *testing.T) {
	deleted := deletedProfileHome(t)
	mustFailClosed(t, deleted, filepath.Join(deleted, "cron"))
}

func TestEnsureCronDirWidenedEnsureCronDirExistingNamedProfileCronDirWorks(t *testing.T) {
	active := filepath.Join(t.TempDir(), "profiles", "active")
	os.MkdirAll(active, 0o700)
	mustCreateDir(t, filepath.Join(active, "cron"))
}

func TestEnsureCronDirWidenedEnsureCronDirScriptsDirUnderNamedProfileFailsClosed(t *testing.T) {
	deleted := deletedProfileHome(t)
	mustFailClosed(t, deleted, filepath.Join(deleted, "scripts"))
}

func TestCronFilePermissions(t *testing.T) {
	s, _ := newTestStore(t)
	os.RemoveAll(s.cronDir)
	if err := s.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{s.cronDir, s.outputDir} {
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", dir, st.Mode().Perm())
		}
	}
	if err := s.SaveJobs([]*Job{{ID: "test-job", Name: "test"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(s.jobsFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("jobs.json mode %v", st.Mode().Perm())
	}
	path, err := s.SaveJobOutput(context.Background(), "test-job", "test output content")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("output mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("job output dir mode %v", st.Mode().Perm())
	}
	s.secureFile("/nonexistent/path/file.json")
}

func TestSecureDirKeepsOperatorModeInContainer(t *testing.T) {
	s, _ := newTestStore(t)
	os.MkdirAll(s.outputDir, 0o755)
	os.Chmod(s.cronDir, 0o755)
	t.Setenv("ICEBATH_CONTAINER", "1")
	t.Setenv("ICEBATH_HOME_MODE", "")
	os.Unsetenv("ICEBATH_HOME_MODE")
	s.ensureDirs()
	if st, _ := os.Stat(s.cronDir); st.Mode().Perm() != 0o755 {
		t.Fatalf("container mode overridden %v", st.Mode().Perm())
	}
	t.Setenv("ICEBATH_HOME_MODE", "0701")
	s.ensureDirs()
	if st, _ := os.Stat(s.cronDir); st.Mode().Perm() != 0o701 {
		t.Fatalf("explicit home mode ignored %v", st.Mode().Perm())
	}
}

type chownCall struct {
	path     string
	uid, gid int
}

// Poses as root over a jobs.json owned by uid and gid 1000, recording or failing every chown
func poseAsRoot(t *testing.T, chownErr error) *[]chownCall {
	t.Helper()
	saved := ownership
	t.Cleanup(func() { ownership = saved })
	calls := &[]chownCall{}
	ownership.owner = func(st os.FileInfo) (int, int, bool) {
		if st.Name() == "jobs.json" {
			return 1000, 1000, true
		}
		return saved.owner(st)
	}
	ownership.euid = func() int { return 0 }
	ownership.egid = func() int { return 0 }
	ownership.chown = func(path string, uid, gid int) error {
		*calls = append(*calls, chownCall{path, uid, gid})
		return chownErr
	}
	return calls
}

func seedPrompt(prompt string) []*Job { return []*Job{{ID: "seed", Prompt: prompt}} }

func TestRootWriterRestoresPreviousOwner(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveJobs(seedPrompt("hello"), nil, false); err != nil {
		t.Fatal(err)
	}
	calls := poseAsRoot(t, nil)
	if err := s.SaveJobs(seedPrompt("updated"), nil, false); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0] != (chownCall{s.jobsFile, 1000, 1000}) {
		t.Fatalf("chown calls %+v", *calls)
	}
}

func TestUnprivilegedWriterNeverChowns(t *testing.T) {
	s, _ := newTestStore(t)
	s.SaveJobs(seedPrompt("hello"), nil, false)
	saved := ownership
	t.Cleanup(func() { ownership = saved })
	ownership.chown = func(string, int, int) error {
		t.Fatal("unprivileged save called chown")
		return nil
	}
	if os.Geteuid() == 0 {
		ownership.euid = func() int { return 1000 }
	}
	if err := s.SaveJobs(seedPrompt("updated"), nil, false); err != nil {
		t.Fatal(err)
	}
}

func TestChownFailureNeverBreaksSave(t *testing.T) {
	s, buf := newTestStore(t)
	s.SaveJobs(seedPrompt("hello"), nil, false)
	poseAsRoot(t, &os.PathError{Op: "chown", Path: s.jobsFile, Err: os.ErrPermission})
	if err := s.SaveJobs(seedPrompt("updated"), nil, false); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.LoadJobs()
	if jobs[0].Prompt != "updated" {
		t.Fatalf("prompt %q", jobs[0].Prompt)
	}
	if !strings.Contains(buf.String(), "Could not restore ownership") {
		t.Fatalf("missing warning: %s", buf.String())
	}
	if st, _ := os.Stat(s.jobsFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

// Store whose home carries an executable scripts/watch.sh the no agent jobs point at
func newScriptStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	os.MkdirAll(filepath.Join(s.home, "scripts"), 0o700)
	os.WriteFile(filepath.Join(s.home, "scripts", "watch.sh"), []byte("#!/usr/bin/env bash\necho alert\n"), 0o700)
	return s
}

func watchdog(t *testing.T, s *Store) *Job {
	t.Helper()
	return mustCreate(t, s, CreateJobOptions{Schedule: "every 2m", Script: "watch.sh", NoAgent: true,
		Deliver: prompt("local"), Name: prompt("watchdog"), Repeat: 0})
}

func TestStaleEmptySavePreservesConcurrentNoAgentCreate(t *testing.T) {
	s := newScriptStore(t)
	job := watchdog(t, s)
	if err := s.SaveJobs(nil, nil, false); err != nil {
		t.Fatal(err)
	}
	left, _ := s.LoadJobs()
	if len(left) != 1 || left[0].ID != job.ID || !left[0].NoAgent || deref(left[0].Script) != "watch.sh" {
		t.Fatalf("no agent create lost %+v", left)
	}
}

func TestRemoveOtherJobPreservesConcurrentCreate(t *testing.T) {
	s := newScriptStore(t)
	agent := mustCreate(t, s, CreateJobOptions{Prompt: prompt("hello"), Schedule: "every 5m", Name: prompt("agent"), Deliver: prompt("local")})
	s.SaveJobs(nil, []string{agent.ID}, false)
	dog := watchdog(t, s)
	s.SaveJobs(nil, []string{agent.ID}, false)
	left, _ := s.LoadJobs()
	if ids := sortedIDs(left); len(ids) != 1 || ids[0] != dog.ID {
		t.Fatalf("left %v", ids)
	}
}

func TestIntentionalRemoveAndReplaceStillDelete(t *testing.T) {
	s := newScriptStore(t)
	job := watchdog(t, s)
	if ok, err := s.removeJob(t.Context(), job.ID); !ok || err != nil {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	if j, _ := s.getJob(job.ID); j != nil {
		t.Fatal("removed job still stored")
	}
	watchdog(t, s)
	if err := s.SaveJobs(nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.LoadJobs(); len(left) != 0 {
		t.Fatalf("replace kept %v", jobIDs(left))
	}
}

func TestMergeDoesNotMutateCallerList(t *testing.T) {
	s := newScriptStore(t)
	watchdog(t, s)
	mine := make([]*Job, 0, 4)
	if err := s.SaveJobs(mine, nil, false); err != nil {
		t.Fatal(err)
	}
	for i, j := range mine[:cap(mine)] {
		if j != nil {
			t.Fatalf("caller backing array slot %d written with %s", i, j.ID)
		}
	}
}

func TestNestedCreateSurvivesOuterStaleSave(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveJobs([]*Job{{ID: "aaaaaaaaaaaa", Name: "a"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	var created *Job
	err := s.withJobsLock(func(tx *jobsTx) error {
		stale, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		if created, err = s.createJobTx(tx, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("nested"), Deliver: prompt("local")}); err != nil {
			return err
		}
		return s.saveJobsLocked(tx, stale, nil, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	left, _ := s.LoadJobs()
	ids := sortedIDs(left)
	if !containsString(ids, created.ID) || !containsString(ids, "aaaaaaaaaaaa") {
		t.Fatalf("nested create clobbered %v", ids)
	}
}

var idKeyedJobs = map[string]any{"jobs": map[string]any{
	"cron1234abcd": map[string]any{"id": "cron1234abcd", "name": "first", "enabled": true, "prompt": "a",
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}},
	"cron5678efgh": map[string]any{"id": "cron5678efgh", "name": "second", "enabled": true, "prompt": "b",
		"schedule": map[string]any{"kind": "interval", "minutes": 30, "display": "every 30m"}},
}}

func TestJobsJsonIdKeyedMapListJobsSurvivesIdKeyedMap(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, idKeyedJobs)
	jobs, err := s.listJobs(true)
	if ids := sortedIDs(jobs); err != nil || len(ids) != 2 || ids[0] != "cron1234abcd" || ids[1] != "cron5678efgh" {
		t.Fatalf("list %v, %v", ids, err)
	}
}

func TestJobsJsonIdKeyedMapNonDictListEntriesDoNotStopHealthyJobsFiring(t *testing.T) {
	s, logs := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("keep me"), Schedule: "every 1h", Name: prompt("survivor")})
	addJunk := func() {
		jobs, _ := s.LoadJobs()
		jobs[0].NextRunAt = strPtr(FormatISO(s.clk.now().Add(-5e9)))
		seedJobs(t, s, jobToMap(t, jobs[0]))
		raw, _ := os.ReadFile(s.jobsFile)
		text := strings.TrimSuffix(strings.TrimSpace(string(raw)), "]}") + `, null, "i am not a job", 42]}`
		os.WriteFile(s.jobsFile, []byte(text), 0o600)
	}
	addJunk()
	raced := false
	real := jobsFileParser
	jobsFileParser = func(path string) (any, bool, error) {
		parsed, lenient, err := real(path)
		if !raced {
			raced = true
			if _, err := s.updateJob(job.ID, map[string]any{"name": "raced"}); err != nil {
				t.Errorf("racing update: %v", err)
			}
		}
		return parsed, lenient, err
	}
	listed, _ := s.listJobs(true)
	jobsFileParser = real
	if ids := jobIDs(listed); len(ids) != 1 || ids[0] != job.ID {
		t.Fatalf("listed %v", ids)
	}
	disk, _ := s.LoadJobs()
	if len(disk) != 1 || disk[0].ID != job.ID || disk[0].Name != "raced" {
		t.Fatalf("racing write lost %+v", disk)
	}
	addJunk()
	if due := mustDue(t, s); len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("due %v", jobIDs(due))
	}
	if ids := diskJobIDs(t, s); len(ids) != 1 || ids[0] != job.ID {
		t.Fatalf("disk %v", ids)
	}
	for _, tc := range []struct {
		bad    any
		detail string
	}{{[]any{nil, "***", 42}, "Skipping 3 non-object"}, {nil, "Replacing invalid"}, {"not-a-list", "Replacing invalid"}} {
		writeJobsFile(t, s, map[string]any{"jobs": tc.bad})
		logs.Reset()
		if loaded, err := s.LoadJobs(); err != nil || len(loaded) != 0 {
			t.Fatalf("%v loaded %v %v", tc.bad, jobIDs(loaded), err)
		}
		if ids := diskJobIDs(t, s); len(ids) != 0 {
			t.Fatalf("%v disk %v", tc.bad, ids)
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if n := countContaining(lines, tc.detail); n != 1 {
			t.Fatalf("%v logged %q %d times: %s", tc.bad, tc.detail, n, logs)
		}
		if n := countContaining(lines, "Auto-repaired"); n != 1 {
			t.Fatalf("%v logged the repair %d times: %s", tc.bad, n, logs)
		}
		if strings.Contains(logs.String(), "***") {
			t.Fatalf("raw junk value logged: %s", logs)
		}
	}
}

// Lines holding the substring
func countContaining(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
