//go:build !windows

package cron

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestCronFilePermissionsEnsureDirsSets0700(t *testing.T) {
	s := openCronStore(filepath.Join(t.TempDir(), "home"), nil)
	if err := os.MkdirAll(s.Home(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	if cron, output := modeOf(t, s.cronDir), modeOf(t, s.outputDir); cron != 0o700 || output != 0o700 {
		t.Fatalf("cron %o output %o", cron, output)
	}
}

func TestCronFilePermissionsSaveJobsSets0600(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveJobs([]*Job{{ID: "test", Prompt: "hello"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	if mode := modeOf(t, s.JobsFile()); mode != 0o600 {
		t.Fatalf("jobs.json mode %o", mode)
	}
}

func TestCronFilePermissionsSaveJobOutputSets0600(t *testing.T) {
	s, _ := newTestStore(t)
	output, err := s.SaveJobOutput(context.Background(), "test-job", "test output content")
	if err != nil {
		t.Fatal(err)
	}
	if mode := modeOf(t, output); mode != 0o600 {
		t.Fatalf("output file mode %o", mode)
	}
	if mode := modeOf(t, filepath.Join(s.outputDir, "test-job")); mode != 0o700 {
		t.Fatalf("job output dir mode %o", mode)
	}
}

func TestSecureHelpersSecureFileNonexistentNoError(t *testing.T) {
	s, _ := newTestStore(t)
	s.secureFile("/nonexistent/path/file.json")
}

func TestSecureHelpersSecureDirPreservesOperatorModeInContainer(t *testing.T) {
	s, _ := newTestStore(t)
	d := filepath.Join(t.TempDir(), "cron")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_CONTAINER", "1")
	t.Setenv("ICEBATH_HOME_MODE", "")
	os.Unsetenv("ICEBATH_HOME_MODE")
	config.SecureDir(s.Home(), d)
	if mode := modeOf(t, d); mode != 0o755 {
		t.Fatalf("operator mode replaced by %o", mode)
	}
	t.Setenv("ICEBATH_HOME_MODE", "0701")
	config.SecureDir(s.Home(), d)
	if mode := modeOf(t, d); mode != 0o701 {
		t.Fatalf("explicit mode not applied, got %o", mode)
	}
}

func TestSaveJobsOwnershipPreservationSaveStillEnforces0600(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.SaveJobs([]*Job{{ID: "seed", Prompt: "hello"}}, nil, false); err != nil {
		t.Fatal(err)
	}
	if mode := modeOf(t, s.JobsFile()); mode != 0o600 {
		t.Fatalf("jobs.json mode %o", mode)
	}
}

func TestTickerErrorMarkerRecordAndGetRoundtrip(t *testing.T) {
	s, _ := newTestStore(t)
	if msg, ok := s.TickerLastError(); ok {
		t.Fatalf("marker before any error %q", msg)
	}
	s.RecordTickerError(context.Background(), "RuntimeError: Failed to read cron database: [Errno 13] Permission denied: '/opt/data/cron/jobs.json'")
	msg, ok := s.TickerLastError()
	if !ok || !strings.Contains(msg, "Permission denied") {
		t.Fatalf("marker %q %v", msg, ok)
	}
}

func TestTickerErrorMarkerClearRemovesMarker(t *testing.T) {
	s, _ := newTestStore(t)
	s.RecordTickerError(context.Background(), "RuntimeError: boom")
	if _, ok := s.TickerLastError(); !ok {
		t.Fatal("no marker after the record")
	}
	s.ClearTickerError()
	if msg, ok := s.TickerLastError(); ok {
		t.Fatalf("marker survived the clear %q", msg)
	}
}

// One iteration of the built in ticker loop over the given tick
func runOneTickerIteration(t *testing.T, s *Store, tick func(context.Context, *Store, tickOptions) (int, error)) {
	t.Helper()
	patchChronos(t, &recoverInterruptedExecutions, func(context.Context, *Store) (int, error) { return 0, nil })
	patchChronos(t, &tickHook, tick)
	patchChronos(t, &tickerWait, func(<-chan struct{}, float64) bool { return true })
	(&InProcessCronScheduler{}).Start(withCronStore(context.Background(), s), make(chan struct{}), StartOptions{})
}

func TestTickerLoopRecordsErrorsFailingTickPersistsError(t *testing.T) {
	s, _ := newTestStore(t)
	runOneTickerIteration(t, s, func(context.Context, *Store, tickOptions) (int, error) {
		return 0, errors.New("Failed to read cron database: [Errno 13] Permission denied")
	})
	msg, ok := s.TickerLastError()
	if !ok {
		t.Fatal("a failing tick must persist its reason for `icebath cron status` to surface")
	}
	if !strings.Contains(msg, "Permission denied") {
		t.Fatalf("marker %q", msg)
	}
}

func TestTickerLoopRecordsErrorsSuccessfulTickClearsError(t *testing.T) {
	s, _ := newTestStore(t)
	s.RecordTickerError(context.Background(), "RuntimeError: stale failure")
	runOneTickerIteration(t, s, func(context.Context, *Store, tickOptions) (int, error) { return 0, nil })
	if msg, ok := s.TickerLastError(); ok {
		t.Fatalf("a clean tick must clear the stale error marker, got %q", msg)
	}
}
