package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store in a fresh temp home with its log captured in the returned buffer
func newTestStore(t *testing.T) (*Store, *bytes.Buffer) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(filepath.Join(home, "cron"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	s := openCronStore(home, nil)
	buf := &bytes.Buffer{}
	s.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s, buf
}

func writeJobsFile(t *testing.T, s *Store, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.jobsFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func jobIDs(jobs []*Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func sortedIDs(jobs []*Job) []string {
	ids := jobIDs(jobs)
	sort.Strings(ids)
	return ids
}

func diskJobIDs(t *testing.T, s *Store) []string {
	t.Helper()
	raw, err := os.ReadFile(s.jobsFile)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	list, ok := v.(*pyjson.Object).Vals["jobs"].([]any)
	if !ok {
		t.Fatalf("jobs on disk is not a list: %s", raw)
	}
	var ids []string
	for _, e := range list {
		id, _ := e.(*pyjson.Object).Get("id")
		ids = append(ids, id.(string))
	}
	return ids
}

func intervalJob(id, name string, minutes int) map[string]any {
	j := map[string]any{"name": name, "enabled": true, "prompt": "do a thing",
		"schedule": map[string]any{"kind": "interval", "minutes": minutes, "display": fmt.Sprintf("every %dm", minutes)}}
	if id != "" {
		j["id"] = id
	}
	return j
}

func TestLoadJobsAcceptsUTF8BOM(t *testing.T) {
	s, _ := newTestStore(t)
	b, _ := json.Marshal(map[string]any{"jobs": []any{intervalJob("bomjob01", "bom-test", 60)}})
	os.WriteFile(s.jobsFile, append([]byte("\xef\xbb\xbf"), b...), 0o600)
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobIDs(loaded), []string{"bomjob01"}) || loaded[0].Name != "bom-test" {
		t.Fatalf("loaded %v", jobIDs(loaded))
	}
}

var idKeyedPayload = map[string]any{
	"jobs": map[string]any{
		"cron1234abcd": intervalJob("cron1234abcd", "Example job", 60),
		"cron5678efgh": intervalJob("cron5678efgh", "Second job", 30),
	},
	"updated_at": "2026-08-23T10:10:12+08:00",
}

func TestLoadJobsFlattensIDKeyedMapAndRepairsDisk(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, idKeyedPayload)
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cron1234abcd", "cron5678efgh"}
	if !reflect.DeepEqual(sortedIDs(loaded), want) {
		t.Fatalf("loaded %v", jobIDs(loaded))
	}
	disk := diskJobIDs(t, s)
	sort.Strings(disk)
	if !reflect.DeepEqual(disk, want) {
		t.Fatalf("disk %v", disk)
	}
	reloaded, _ := s.LoadJobs()
	if !reflect.DeepEqual(sortedIDs(reloaded), want) {
		t.Fatalf("reloaded %v", jobIDs(reloaded))
	}
}

func TestEmptyIDKeyedMapReturnsEmptyList(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": map[string]any{}})
	loaded, err := s.LoadJobs()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("loaded %v %v", jobIDs(loaded), err)
	}
}

func TestMapValueWithoutInlineIDAdoptsKey(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": map[string]any{
		"cronkeyonly1":     intervalJob("", "keyed only", 15),
		"cron-ignored-key": intervalJob("croninline99", "inline id wins", 5),
	}})
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Job{}
	for _, j := range loaded {
		byID[j.ID] = j
	}
	if byID["cronkeyonly1"] == nil || byID["cronkeyonly1"].Name != "keyed only" || byID["croninline99"] == nil || byID["cron-ignored-key"] != nil {
		t.Fatalf("loaded %v", jobIDs(loaded))
	}
	disk := diskJobIDs(t, s)
	sort.Strings(disk)
	if !reflect.DeepEqual(disk, []string{"croninline99", "cronkeyonly1"}) {
		t.Fatalf("disk %v", disk)
	}
}

func TestNonDictMapValuesSkippedWithWarning(t *testing.T) {
	s, logs := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": map[string]any{
		"goodjob1":    intervalJob("", "survivor", 60),
		"junk-string": "i am not a job",
		"junk-number": 42,
		"junk-null":   nil,
	}})
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jobIDs(loaded), []string{"goodjob1"}) {
		t.Fatalf("loaded %v", jobIDs(loaded))
	}
	if !strings.Contains(logs.String(), "non-dict") {
		t.Fatalf("no non-dict warning in %s", logs)
	}
	if disk := diskJobIDs(t, s); !reflect.DeepEqual(disk, []string{"goodjob1"}) {
		t.Fatalf("disk %v", disk)
	}
}

func TestNonObjectListEntriesDroppedAndScalarJobsFieldReplaced(t *testing.T) {
	s, logs := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": []any{intervalJob("keep1", "keep", 60), nil, "i am not a job", 42}})
	loaded, err := s.LoadJobs()
	if err != nil || !reflect.DeepEqual(jobIDs(loaded), []string{"keep1"}) {
		t.Fatalf("loaded %v %v", jobIDs(loaded), err)
	}
	if !strings.Contains(logs.String(), "Skipping 3 non-object entries in jobs.json (types: NoneType, int, str)") {
		t.Fatalf("log %s", logs)
	}
	if strings.Contains(logs.String(), "i am not a job") {
		t.Fatal("raw junk value logged")
	}
	writeJobsFile(t, s, map[string]any{"jobs": 7})
	loaded, err = s.LoadJobs()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("scalar jobs field: %v %v", jobIDs(loaded), err)
	}
	if !strings.Contains(logs.String(), "Replacing invalid jobs.json 'jobs' field (int) with an empty list") {
		t.Fatalf("log %s", logs)
	}
	if disk := diskJobIDs(t, s); len(disk) != 0 {
		t.Fatalf("disk %v", disk)
	}
}

func TestLoadJobsRepairsControlCharactersAndBareList(t *testing.T) {
	s, logs := newTestStore(t)
	os.WriteFile(s.jobsFile, []byte("{\"jobs\": [{\"id\": \"ctl1\", \"prompt\": \"line\tone\"}]}"), 0o600)
	loaded, err := s.LoadJobs()
	if err != nil || len(loaded) != 1 || loaded[0].Prompt != "line\tone" {
		t.Fatalf("loaded %v %v", jobIDs(loaded), err)
	}
	if !strings.Contains(logs.String(), "Auto-repaired jobs.json (had invalid control characters)") {
		t.Fatalf("log %s", logs)
	}
	raw, _ := os.ReadFile(s.jobsFile)
	if !strings.Contains(string(raw), `"line\tone"`) {
		t.Fatalf("control character not escaped on disk: %s", raw)
	}
	os.WriteFile(s.jobsFile, []byte(`[{"id": "bare1"}]`), 0o600)
	loaded, err = s.LoadJobs()
	if err != nil || !reflect.DeepEqual(jobIDs(loaded), []string{"bare1"}) || !strings.Contains(logs.String(), "bare list wrapped as dict") {
		t.Fatalf("bare list %v %v", jobIDs(loaded), err)
	}
	os.WriteFile(s.jobsFile, []byte(`"nope"`), 0o600)
	if _, err := s.LoadJobs(); err == nil || err.Error() != "Cron database corrupted: expected {'jobs': [...]}, got str" {
		t.Fatalf("scalar store: %v", err)
	}
}

func TestLoadJobsNormalizesRepeatCompleted(t *testing.T) {
	s, logs := newTestStore(t)
	os.WriteFile(s.jobsFile, []byte(`{"jobs": [{"id": "a", "repeat": {"times": 3, "completed": -4}}, {"id": "b", "repeat": {"times": null, "completed": "x"}}, {"id": "c", "repeat": {"times": 2, "completed": 1.9}}]}`), 0o600)
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	got := []int{loaded[0].Repeat.Completed, loaded[1].Repeat.Completed, loaded[2].Repeat.Completed}
	if !reflect.DeepEqual(got, []int{0, 0, 1}) || !strings.Contains(logs.String(), "invalid repeat.completed normalized") {
		t.Fatalf("completed %v log %s", got, logs)
	}
}

func TestSaveKeepsLoadedKeyOrderAndPythonFloats(t *testing.T) {
	s, _ := newTestStore(t)
	os.WriteFile(s.jobsFile, []byte(`{"jobs": [{"zeta_custom": 1, "prompt": "p", "id": "k1", "last_dispatch": {"scheduled_at": "a", "dispatched_at": "b", "lateness_seconds": 12.0, "kind": "on_time"}}]}`), 0o600)
	loaded, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveJobs(loaded, nil, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.jobsFile)
	text := string(raw)
	zi, pi, ii := strings.Index(text, `"zeta_custom"`), strings.Index(text, `"prompt"`), strings.Index(text, `"id"`)
	if !(zi < pi && pi < ii) {
		t.Fatalf("key order lost: %s", text)
	}
	if !strings.Contains(text, `"lateness_seconds": 12.0`) || !strings.HasPrefix(text, "{\n  \"jobs\": [\n    {\n") || !strings.Contains(text, `"updated_at": "`) {
		t.Fatalf("payload format: %s", text)
	}
}

func TestSaveJobOutputCreatesOutputFile(t *testing.T) {
	s, _ := newTestStore(t)
	out, err := s.SaveJobOutput("test123", "# Results\nEverything ok.")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != "# Results\nEverything ok." || !strings.Contains(out, "test123") {
		t.Fatalf("output %s %q %v", out, b, err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "/abs", "  "} {
		if _, err := s.SaveJobOutput(bad, "x"); err == nil || !strings.Contains(err.Error(), "Invalid cron job id for output path: ") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestPruneJobOutputKeepsNewestN(t *testing.T) {
	s, _ := newTestStore(t)
	d := filepath.Join(t.TempDir(), "job")
	os.MkdirAll(d, 0o700)
	var names []string
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("2026-06-25_10-00-%02d.md", i)
		names = append(names, n)
		os.WriteFile(filepath.Join(d, n), []byte("x"), 0o600)
	}
	if got := s.pruneJobOutput(d, 3); got != 7 {
		t.Fatalf("deleted %d", got)
	}
	left, _ := filepath.Glob(filepath.Join(d, "*.md"))
	for i := range left {
		left[i] = filepath.Base(left[i])
	}
	sort.Strings(left)
	if !reflect.DeepEqual(left, names[7:]) {
		t.Fatalf("left %v", left)
	}
	if s.pruneJobOutput(d, 0) != 0 || s.pruneJobOutput(d, -1) != 0 {
		t.Fatal("non positive keep pruned")
	}
}

func TestOutputRetentionReadsConfig(t *testing.T) {
	s, _ := newTestStore(t)
	os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  output_retention: 2\n"), 0o600)
	for i := 0; i < 4; i++ {
		ts := time.Date(2026, 6, 25, 10, 0, i, 0, time.UTC)
		s.clk.nowFn = func() time.Time { return ts }
		if _, err := s.SaveJobOutput("job1", "x"); err != nil {
			t.Fatal(err)
		}
	}
	left, _ := filepath.Glob(filepath.Join(s.outputDir, "job1", "*.md"))
	if len(left) != 2 {
		t.Fatalf("kept %v", left)
	}
}

func TestSiblingWriteInsideSectionIsMerged(t *testing.T) {
	s, _ := newTestStore(t)
	seed := &Job{ID: "aaaaaaaaaaaa", Name: "watchdog", Enabled: true}
	if err := s.SaveJobs([]*Job{seed}, nil, true); err != nil {
		t.Fatal(err)
	}
	err := s.withJobsLock(func(tx *jobsTx) error {
		current, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{"id": "aaaaaaaaaaaa", "name": "watchdog"}, map[string]any{"id": "bbbbbbbbbbbb", "name": "b"}}})
		return s.saveJobsLocked(tx, current, nil, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ := s.LoadJobs()
	if !reflect.DeepEqual(sortedIDs(loaded), []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"}) {
		t.Fatalf("ids %v", jobIDs(loaded))
	}
}

func TestSaveRemovedIDsDropDiskJobs(t *testing.T) {
	s, _ := newTestStore(t)
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}})
	if err := s.SaveJobs([]*Job{}, []string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	loaded, _ := s.LoadJobs()
	if !reflect.DeepEqual(jobIDs(loaded), []string{"b"}) {
		t.Fatalf("ids %v", jobIDs(loaded))
	}
	payload := []*Job{}
	if err := s.SaveJobs(payload, nil, false); err != nil || len(payload) != 0 {
		t.Fatalf("caller payload mutated %v %v", payload, err)
	}
	if err := s.SaveJobs(nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := s.LoadJobs(); len(loaded) != 0 {
		t.Fatalf("replace kept %v", jobIDs(loaded))
	}
}

func TestSaveOverCorruptStoreFailsClosed(t *testing.T) {
	s, _ := newTestStore(t)
	corrupt := []byte(`{"jobs": [{"id": "ccc`)
	os.WriteFile(s.jobsFile, corrupt, 0o600)
	err := s.SaveJobs([]*Job{{ID: "aaaaaaaaaaaa", Name: "a"}}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(s.jobsFile); !bytes.Equal(b, corrupt) {
		t.Fatalf("corrupt bytes touched: %s", b)
	}
	if err := s.SaveJobs([]*Job{{ID: "aaaaaaaaaaaa", Name: "a"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := s.LoadJobs(); !reflect.DeepEqual(jobIDs(loaded), []string{"aaaaaaaaaaaa"}) {
		t.Fatalf("ids %v", jobIDs(loaded))
	}
	if _, err := s.LoadJobs(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.jobsFile, corrupt, 0o600)
	if _, err := s.LoadJobs(); err == nil || !strings.HasPrefix(err.Error(), "Cron database corrupted and unrepairable: ") {
		t.Fatalf("load of corrupt store: %v", err)
	}
}

func TestSymlinkedStoreSavesByRenameNotInPlaceCopy(t *testing.T) {
	s, _ := newTestStore(t)
	realDir := filepath.Join(t.TempDir(), "elsewhere")
	os.Mkdir(realDir, 0o700)
	os.WriteFile(filepath.Join(realDir, "jobs.json"), []byte(`{"jobs": []}`), 0o600)
	if err := os.Symlink(filepath.Join(realDir, "jobs.json"), s.jobsFile); err != nil {
		t.Skip("symlinks unavailable")
	}
	before, _ := os.Stat(filepath.Join(realDir, "jobs.json"))
	if err := s.SaveJobs([]*Job{{ID: "a", Prompt: "x", Enabled: true}}, nil, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(s.jobsFile); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	after, _ := os.Stat(filepath.Join(realDir, "jobs.json"))
	if os.SameFile(before, after) {
		t.Fatal("store rewritten in place instead of renamed")
	}
	if loaded, _ := s.LoadJobs(); !reflect.DeepEqual(jobIDs(loaded), []string{"a"}) {
		t.Fatalf("ids %v", jobIDs(loaded))
	}
}

func TestCronDirsAndStoreAreOwnerOnly(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveJobs([]*Job{{ID: "a"}}, nil, true); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{s.cronDir: 0o700, s.outputDir: 0o700, s.jobsFile: 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s mode %v want %v", path, st.Mode().Perm(), want)
		}
	}
}

func TestEnsureCronDirNeverRecreatesDeletedProfile(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "profiles", "work", "cron")
	if err := ensureCronDir(gone); err == nil {
		t.Fatal("deleted named profile home recreated")
	}
	plain := filepath.Join(root, "custom", "home", "cron")
	if err := ensureCronDir(plain); err != nil {
		t.Fatal(err)
	}
	if err := ensureCronDir(plain); err != nil {
		t.Fatalf("existing dir rejected: %v", err)
	}
}

func TestTickerMarkersRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	if _, ok := s.TickerHeartbeatAge(); ok {
		t.Fatal("age reported without a heartbeat")
	}
	now := 1_800_000_000.25
	epochNow = func() float64 { return now }
	t.Cleanup(func() { epochNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 } })
	s.RecordTickerHeartbeat(true)
	raw, _ := os.ReadFile(filepath.Join(s.cronDir, "ticker_heartbeat"))
	if string(raw) != fmt.Sprintf("1800000000.25 %d", os.Getpid()) {
		t.Fatalf("heartbeat %q", raw)
	}
	now += 30
	if age, ok := s.TickerHeartbeatAge(); !ok || age != 30 {
		t.Fatalf("age %v %v", age, ok)
	}
	if age, ok := s.TickerSuccessAge(); !ok || age != 30 {
		t.Fatalf("success age %v %v", age, ok)
	}
	if !s.TickerHeartbeatWriterAlive() {
		t.Fatal("own pid reported dead")
	}
	os.WriteFile(filepath.Join(s.cronDir, "ticker_heartbeat"), []byte("1800000000.25"), 0o600)
	if s.TickerHeartbeatWriterAlive() {
		t.Fatal("legacy bare stamp treated as a live writer")
	}
	os.WriteFile(filepath.Join(s.cronDir, "ticker_last_success"), []byte(fmt.Sprintf("%v", now+100)), 0o600)
	if age, ok := s.TickerSuccessAge(); !ok || age != 0 {
		t.Fatalf("future stamp age %v %v", age, ok)
	}
	s.RecordTickerError("  boom\nsecond line  ")
	if msg, ok := s.TickerLastError(); !ok || msg != "boom\nsecond line" {
		t.Fatalf("error %q %v", msg, ok)
	}
	s.ClearTickerError()
	if _, ok := s.TickerLastError(); ok {
		t.Fatal("error survived clear")
	}
}

func TestFireClaimFenceChecksOwnerAndSelfRemoval(t *testing.T) {
	s, _ := newTestStore(t)
	job := &Job{ID: "fire1", Enabled: true, FireClaim: &Claim{At: "2026-01-01T00:00:00+00:00", By: "owner-a"}}
	if err := s.SaveJobs([]*Job{job}, nil, true); err != nil {
		t.Fatal(err)
	}
	check := func(ctx context.Context, id, owner string) bool {
		var got bool
		if err := s.fireClaimFence(ctx, id, owner, func(ctx context.Context, owns bool) error {
			nested, err := s.underFireFence(ctx, id, func(context.Context) (bool, error) { return true, nil })
			if err != nil || !nested {
				t.Fatalf("nested fence not reentrant: %v %v", nested, err)
			}
			got = owns
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	ctx := context.Background()
	if !check(ctx, "fire1", "owner-a") || check(ctx, "fire1", "owner-b") {
		t.Fatal("claim ownership misread")
	}
	if check(ctx, "gone1", "owner-a") {
		t.Fatal("missing record accepted without a self removal scope")
	}
	scoped, marker := selfRemovalDeliveryScope(ctx, "gone1")
	if check(scoped, "gone1", "owner-a") {
		t.Fatal("self removal accepted before the record was removed")
	}
	marker.removed = true
	if !check(scoped, "gone1", "owner-a") {
		t.Fatal("self removed run refused its final delivery")
	}
}

func TestCronLockHelperProcess(t *testing.T) {
	role := os.Getenv("ICEBATH_CRON_LOCK_ROLE")
	if role == "" {
		return
	}
	home, dir := os.Getenv("ICEBATH_CRON_LOCK_HOME"), os.Getenv("ICEBATH_CRON_LOCK_DIR")
	s := openCronStore(home, nil)
	if role == "blocker" {
		os.WriteFile(filepath.Join(dir, "blocker_started"), []byte("1"), 0o600)
	}
	s.withJobsLock(func(*jobsTx) error {
		if role == "blocker" {
			os.WriteFile(filepath.Join(dir, "blocker_acquired"), []byte("1"), 0o600)
			return nil
		}
		os.WriteFile(filepath.Join(dir, "child_holds_lock"), []byte("1"), 0o600)
		for i := 0; i < 1000; i++ {
			if _, err := os.Stat(filepath.Join(dir, "child_may_release")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	})
	os.Exit(0)
}

func waitFile(path string) bool {
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestJobsLockExcludesAnotherProcess(t *testing.T) {
	s, _ := newTestStore(t)
	dir := t.TempDir()
	spawn := func(role string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCronLockHelperProcess$")
		cmd.Env = append(os.Environ(), "ICEBATH_CRON_LOCK_ROLE="+role, "ICEBATH_CRON_LOCK_HOME="+s.home, "ICEBATH_CRON_LOCK_DIR="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	holder := spawn("holder")
	defer holder.Process.Kill()
	if !waitFile(filepath.Join(dir, "child_holds_lock")) {
		t.Fatal("child never acquired the jobs lock")
	}
	f, err := os.OpenFile(filepath.Join(s.cronDir, ".jobs.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := acquireFlock(f, 0); ok {
		t.Fatal("flock taken while another process holds the jobs lock")
	}
	f.Close()
	blocker := spawn("blocker")
	defer blocker.Process.Kill()
	if !waitFile(filepath.Join(dir, "blocker_started")) {
		t.Fatal("blocker never started")
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "blocker_acquired")); err == nil {
		t.Fatal("second process entered the jobs lock while the first held it")
	}
	os.WriteFile(filepath.Join(dir, "child_may_release"), []byte("1"), 0o600)
	if err := holder.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if !waitFile(filepath.Join(dir, "blocker_acquired")) {
		t.Fatal("blocker never acquired the jobs lock after release")
	}
	blocker.Wait()
}
