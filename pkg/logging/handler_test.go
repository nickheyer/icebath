package logging

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func plainFormatter() *Formatter { return NewFormatter("%(message)s", "") }

func newTestRotating(t *testing.T, path string, maxBytes float64, backups int) *managedRotatingFileHandler {
	t.Helper()
	h, err := newManagedRotatingFileHandler(context.Background(), path, maxBytes, backups)
	if err != nil {
		t.Fatal(err)
	}
	h.SetFormatter(plainFormatter())
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func newTestRouter(t *testing.T, existing *managedRotatingFileHandler, profileHomes ...string) *profileRoutingFileHandler {
	t.Helper()
	r, err := newProfileRoutingFileHandler(existing, profileHomes)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAddRotatingHandlerNoDuplicateForSamePath(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "test.log")
	for range 2 {
		if err := addRotatingHandler(context.Background(), path, LevelInfo, 1024, 1, plainFormatter(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(queuedFileHandlers()); n != 1 {
		t.Fatalf("%d rotating handlers", n)
	}
}

// Forces the platform the rotation code sees and reselects the rotating handler for it
func withGOOS(t *testing.T, name string) {
	t.Helper()
	prev := goos
	goos = name
	selectRotation()
	t.Cleanup(func() {
		goos = prev
		selectRotation()
	})
}

func TestHelperOnlyMatchesWindowsConcurrentLockTimeout(t *testing.T) {
	withGOOS(t, "windows")
	if !isWindowsConcurrentLogLockTimeout(&lockTimeoutError{}) {
		t.Fatal("lock timeout not matched")
	}
	if !isWindowsConcurrentLogLockTimeout(errors.New("RuntimeError: Cannot acquire lock after 20 attempts")) {
		t.Fatal("runtime error text not matched")
	}
	if isWindowsConcurrentLogLockTimeout(errors.New("RuntimeError: some other logging failure")) {
		t.Fatal("other failure matched")
	}
}

func TestHelperNeverMatchesOffWindows(t *testing.T) {
	withGOOS(t, "linux")
	if isWindowsConcurrentLogLockTimeout(&lockTimeoutError{}) {
		t.Fatal("matched off Windows")
	}
}

// Handler collecting every record it is given
type captureHandler struct {
	handlerCore
	recs []*Record
}

func (c *captureHandler) Handle(_ context.Context, rec *Record) bool {
	c.recs = append(c.recs, rec)
	return true
}

func (c *captureHandler) Close() error { return nil }

func resetLockTimeoutWarned(t *testing.T) {
	lockTimeoutWarnLock.Lock()
	lockTimeoutWarned = false
	lockTimeoutWarnLock.Unlock()
	t.Cleanup(func() {
		lockTimeoutWarnLock.Lock()
		lockTimeoutWarned = false
		lockTimeoutWarnLock.Unlock()
	})
}

func TestLockTimeoutRoutedToHandleErrorIsSuppressed(t *testing.T) {
	withGOOS(t, "windows")
	resetLockTimeoutWarned(t)
	h := newTestRotating(t, filepath.Join(t.TempDir(), "agent.log"), 1, 1)
	listener := &captureHandler{}
	GetLogger("hermes_logging").AddHandler(listener)
	t.Cleanup(func() { GetLogger("hermes_logging").RemoveHandler(listener) })
	rec := testRecord("_test_concurrent_lock_timeout_agent", LevelInfo, "force rollover")
	errText := captureStderr(t, func() {
		h.handleError(rec, &lockTimeoutError{})
		h.handleError(rec, &lockTimeoutError{})
	})
	if strings.Contains(errText, "Cannot acquire lock after 20 attempts") || strings.Contains(errText, "--- Logging error ---") {
		t.Fatalf("stderr %q", errText)
	}
	if len(listener.recs) != 1 || !strings.Contains(listener.recs[0].Msg, "concurrent-log-handler") {
		t.Fatalf("warnings %v", listener.recs)
	}
}

func TestLockTimeoutWarningIsOneShot(t *testing.T) {
	resetLockTimeoutWarned(t)
	listener := &captureHandler{}
	GetLogger("hermes_logging").AddHandler(listener)
	t.Cleanup(func() { GetLogger("hermes_logging").RemoveHandler(listener) })
	warnWindowsLockTimeoutOnce()
	warnWindowsLockTimeoutOnce()
	if len(listener.recs) != 1 || listener.recs[0].Levelno != LevelWarning || !strings.Contains(listener.recs[0].Msg, "concurrent-log-handler") {
		t.Fatalf("warnings %v", listener.recs)
	}
}

func TestFreshImportSelectsWorkingConcurrentHandler(t *testing.T) {
	withGOOS(t, "windows")
	if !useCLH || windowsCLHFallback || windowsCLHReason != "" {
		t.Fatalf("clh %v fallback %v reason %q", useCLH, windowsCLHFallback, windowsCLHReason)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	h, err := newFileHandler(context.Background(), path, LevelInfo, 1, 1, plainFormatter())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	for _, msg := range []string{"before rollover", "after rollover"} {
		h.Handle(context.Background(), testRecord("test", LevelInfo, msg))
	}
	if got := readText(t, path); got != "after rollover\n" {
		t.Fatalf("live file %q", got)
	}
	if got := readText(t, path+".1"); got != "before rollover\n" {
		t.Fatalf("backup %q", got)
	}
}

func TestFreshImportRetainsLockFailureAndWritesWithoutRollover(t *testing.T) {
	failure := errors.New("ImportError: pywintypes is required for Win32Locker but not found")
	prevLock := lockFile
	lockFile = func(*os.File) error { return failure }
	t.Cleanup(func() { lockFile = prevLock })
	withGOOS(t, "windows")
	if useCLH || !windowsCLHFallback || windowsCLHReason != `ImportError('pywintypes is required for Win32Locker but not found')` {
		t.Fatalf("clh %v fallback %v reason %q", useCLH, windowsCLHFallback, windowsCLHReason)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	h, err := newFileHandler(context.Background(), path, LevelInfo, 1, 1, plainFormatter())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if h.maxBytes != 0 || h.backupCount != 0 {
		t.Fatalf("rotation kept %v %d", h.maxBytes, h.backupCount)
	}
	for _, msg := range []string{"first message", "second message"} {
		h.Handle(context.Background(), testRecord("test", LevelInfo, msg))
	}
	if got := readText(t, path); got != "first message\nsecond message\n" {
		t.Fatalf("file %q", got)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "agent.log.*")); len(m) != 0 {
		t.Fatalf("backups %v", m)
	}
}

func TestRecoversAfterExternalRename(t *testing.T) {
	dir := t.TempDir()
	path, rotated := filepath.Join(dir, "gateway.log"), filepath.Join(dir, "gateway.log.1")
	h := newTestRotating(t, path, 10*1024*1024, 3)
	h.Handle(context.Background(), testRecord("gateway.run", LevelInfo, "before rotation"))
	if readText(t, path) != "before rotation\n" {
		t.Fatal("first write missing")
	}
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	h.Handle(context.Background(), testRecord("gateway.run", LevelInfo, "after rotation"))
	if readText(t, path) != "after rotation\n" {
		t.Fatalf("handler did not recreate gateway.log: %q", readText(t, path))
	}
	if readText(t, rotated) != "before rotation\n" {
		t.Fatalf("rotated %q", readText(t, rotated))
	}
}

func TestExternalTruncateDoesNotForceReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	h := newTestRotating(t, path, 10*1024*1024, 3)
	h.Handle(context.Background(), testRecord("gateway.run", LevelInfo, strings.Repeat("AAAA", 32)))
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	h.Handle(context.Background(), testRecord("gateway.run", LevelInfo, "after truncate"))
	if got := readText(t, path); got != "after truncate\n" {
		t.Fatalf("file %q", got)
	}
}

// Stream whose every operation fails with EIO
type sickStream struct{}

func (sickStream) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "sick", Err: syscall.EIO}
}
func (sickStream) Tell() (int64, error) {
	return 0, &os.PathError{Op: "seek", Path: "sick", Err: syscall.EIO}
}
func (sickStream) Close() error { return nil }

func TestEIOFromFileHandlerNamesThePathOnceThenRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	h := newTestRotating(t, path, 1024, 1)
	errText := captureStderr(t, func() {
		_ = h.stream.Close()
		h.stream = sickStream{}
		for i := range 5 {
			h.Handle(context.Background(), testRecord("t", LevelInfo, "sick "+string(rune('0'+i))))
		}
	})
	if strings.Contains(errText, "--- Logging error ---") || strings.Count(errText, path) != 1 || !strings.Contains(errText, "Input/output error") {
		t.Fatalf("stderr %q", errText)
	}
	h.Handle(context.Background(), testRecord("t", LevelInfo, "recovered"))
	if !strings.Contains(readText(t, path), "recovered") {
		t.Fatal("no recovery")
	}
}

func TestEIOAfterSuccessfulReopenStillNamesThePathOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	h := newTestRotating(t, path, 1024, 1)
	errText := captureStderr(t, func() {
		h.open = func(string) (logStream, error) { return sickStream{}, nil }
		_ = h.stream.Close()
		h.stream = sickStream{}
		for range 25 {
			h.Handle(context.Background(), testRecord("t", LevelInfo, "sick"))
		}
	})
	if strings.Contains(errText, "--- Logging error ---") || strings.Count(errText, path) != 1 {
		t.Fatalf("stderr %q", errText)
	}
}

func TestSafeStderrWritesUTF8(t *testing.T) {
	sh := newStreamHandler(safeStderr())
	defer func() { _ = sh.Close() }()
	sh.SetFormatter(plainFormatter())
	got := captureStderr(t, func() { sh.Handle(context.Background(), testRecord("unicode", LevelInfo, "Session — 日本")) })
	if got != "Session — 日本\n" {
		t.Fatalf("stderr %q", got)
	}
}

// Stdout double counting reconfigure calls
type fakeStdout struct {
	tty   bool
	calls []bool
}

func (f *fakeStdout) IsTerminal() bool { return f.tty }
func (f *fakeStdout) Reconfigure(lineBuffering bool) error {
	f.calls = append(f.calls, lineBuffering)
	return nil
}

func withStdout(t *testing.T, s any) {
	prev, prevBuffered := currentStdout, lineBufferedStdout
	currentStdout = func() any { return s }
	t.Cleanup(func() { currentStdout, lineBufferedStdout = prev, prevBuffered })
}

func TestTTYNoneOrReconfigureLessStdoutLeftAlone(t *testing.T) {
	tty := &fakeStdout{tty: true}
	withStdout(t, tty)
	lineBufferPipedStdout()
	if len(tty.calls) != 0 {
		t.Fatal("terminal reconfigured")
	}
	withStdout(t, nil)
	lineBufferPipedStdout()
	withStdout(t, (*os.File)(nil))
	lineBufferPipedStdout()
	withStdout(t, io.Discard)
	lineBufferPipedStdout()
}

func TestSetupLoggingAppliesItToPipedStdout(t *testing.T) {
	testHome(t)
	stream := &fakeStdout{}
	withStdout(t, stream)
	dir := t.TempDir()
	for range 2 {
		mustSetup(t, Options{Home: dir, Force: true})
	}
	if len(stream.calls) != 1 || !stream.calls[0] {
		t.Fatalf("reconfigure calls %v", stream.calls)
	}
}

func TestProfileRouterNeverHoldsItsLockWhileTakingTheLoggingModuleLock(t *testing.T) {
	h := testHome(t)
	profile := filepath.Join(h, "profiles", "t23")
	_ = os.MkdirAll(filepath.Join(profile, "logs"), 0o755)
	_ = os.MkdirAll(filepath.Join(h, "logs"), 0o755)
	existing := newTestRotating(t, filepath.Join(h, "logs", "agent.log"), 0, 0)
	router := newTestRouter(t, existing, profile)
	_ = existing.Close()
	defer func() { _ = router.Close() }()
	building := &pythread.Event{}
	prev := newFileHandlerFn
	newFileHandlerFn = func(ctx context.Context, p string, l int, m float64, b int, f *Formatter) (*managedRotatingFileHandler, error) {
		building.Set()
		return prev(ctx, p, l, m, b, f)
	}
	t.Cleanup(func() { newFileHandlerFn = prev })
	rec := testRecord("t23", LevelError, "routed")
	rec.HermesHome = profile
	done := make(chan struct{})
	moduleLock.Lock()
	go func() {
		defer close(done)
		router.Handle(context.Background(), rec)
	}()
	if !building.Wait(5) {
		moduleLock.Unlock()
		t.Fatal("the router never built the profile handler")
	}
	acquired := router.lock.TryLockFor(2 * time.Second)
	if acquired {
		router.lock.Unlock()
	}
	moduleLock.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener still blocked")
	}
	if !acquired {
		t.Fatal("router held its lock while waiting on the module lock")
	}
	if !strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "routed") {
		t.Fatal("record missing")
	}
}

func TestProfileRouterCloseIsNeverUndoneByARecordInFlight(t *testing.T) {
	for _, closedAt := range []string{"after_pick", "before_publish"} {
		t.Run(closedAt, func(t *testing.T) {
			h := testHome(t)
			profile := filepath.Join(h, "profiles", "t23b")
			_ = os.MkdirAll(filepath.Join(profile, "logs"), 0o755)
			_ = os.MkdirAll(filepath.Join(h, "logs"), 0o755)
			existing := newTestRotating(t, filepath.Join(h, "logs", "agent.log"), 0, 0)
			router := newTestRouter(t, existing, profile)
			_ = existing.Close()
			profileLog := filepath.Join(profile, "logs", "agent.log")
			var built []*managedRotatingFileHandler
			if closedAt == "after_pick" {
				if _, err := router.handlerForHome(profile); err != nil {
					t.Fatal(err)
				}
				router.pick = func(hm string) (*managedRotatingFileHandler, error) {
					handler, err := router.handlerForHome(hm)
					_ = router.Close()
					_ = os.Remove(profileLog)
					return handler, err
				}
			} else {
				prev := newFileHandlerFn
				newFileHandlerFn = func(ctx context.Context, p string, l int, m float64, b int, f *Formatter) (*managedRotatingFileHandler, error) {
					handler, err := prev(ctx, p, l, m, b, f)
					built = append(built, handler)
					if len(built) == 1 {
						_ = router.Close()
						_ = os.Remove(profileLog)
					}
					return handler, err
				}
				t.Cleanup(func() { newFileHandlerFn = prev })
			}
			rec := testRecord("t23", LevelError, "in flight")
			rec.HermesHome = profile
			router.Handle(context.Background(), rec)
			if _, err := os.Stat(profileLog); err == nil {
				t.Fatal("a closed per-home log file was reopened")
			}
			router.mu.Lock()
			n := len(router.profileHandlers)
			router.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d handlers after close", n)
			}
			for _, b := range built {
				if b.currentStream() != nil {
					t.Fatal("a handler built after close leaked its fd")
				}
			}
		})
	}
}

func TestRouterNeverBuildsAChildHandlerUnderALockLoggingShutdownTakes(t *testing.T) {
	h := testHome(t)
	profile := filepath.Join(h, "profiles", "lockorder")
	_ = os.MkdirAll(filepath.Join(profile, "logs"), 0o755)
	_ = os.MkdirAll(filepath.Join(h, "logs"), 0o755)
	existing := newTestRotating(t, filepath.Join(h, "logs", "agent.log"), 0, 0)
	router := newTestRouter(t, existing, profile)
	_ = existing.Close()
	building, proceed := &pythread.Event{}, &pythread.Event{}
	prev := newFileHandlerFn
	newFileHandlerFn = func(ctx context.Context, p string, l int, m float64, b int, f *Formatter) (*managedRotatingFileHandler, error) {
		building.Set()
		proceed.Wait(5)
		return prev(ctx, p, l, m, b, f)
	}
	t.Cleanup(func() { newFileHandlerFn = prev })
	rec := testRecord("lockorder", LevelWarning, "routed")
	rec.HermesHome = profile
	holding, released := &pythread.Event{}, &pythread.Event{}
	var got bool
	emitterDone, clearerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(emitterDone)
		router.Handle(context.Background(), rec)
	}()
	if !building.Wait(5) {
		t.Fatal("no build")
	}
	go func() {
		defer close(clearerDone)
		moduleLock.Lock()
		defer moduleLock.Unlock()
		holding.Set()
		released.Wait(5)
		got = router.lock.TryLockFor(5 * time.Second)
		if got {
			_ = router.Close()
			router.lock.Unlock()
		}
	}()
	if !holding.Wait(5) {
		t.Fatal("clearer never took the module lock")
	}
	proceed.Set()
	released.Set()
	for _, ch := range []chan struct{}{clearerDone, emitterDone} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("lock order inversion hung a goroutine")
		}
	}
	router.mu.Lock()
	for _, child := range router.profileHandlers {
		_ = child.Close()
	}
	router.mu.Unlock()
	if !got {
		t.Fatal("shutdown under the module lock could not take the router lock")
	}
}

// Real icebath home of the operator running the tests
func realIcebathHome() string {
	userHome, _ := os.UserHomeDir()
	return filepath.Join(userHome, ".icebath")
}

func TestIcebathHomeIsSandboxedBeforeTests(t *testing.T) {
	if homeAtTestMain == "" {
		t.Fatal("TestMain must set ICEBATH_HOME before tests run")
	}
	if resolvePath(homeAtTestMain) == resolvePath(realIcebathHome()) {
		t.Fatalf("ICEBATH_HOME pointed at the operator's real home %s", homeAtTestMain)
	}
}

func TestDefaultSetupDoesNotTargetTheRealLogs(t *testing.T) {
	resetLogging(t)
	mustSetup(t, Options{})
	realLogs := filepath.Join(realIcebathHome(), "logs")
	queueState.mu.Lock()
	var seen []string
	for _, h := range queueState.handlers {
		switch v := h.(type) {
		case *managedRotatingFileHandler:
			seen = append(seen, v.baseFilename)
		case *profileRoutingFileHandler:
			seen = append(seen, v.baseFilename)
		}
	}
	queueState.mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no file destinations")
	}
	for _, p := range seen {
		if strings.HasPrefix(p, realLogs) {
			t.Fatalf("the test run writes into the operator's real logs: %s", p)
		}
	}
}

func TestLoggersCapturedBeforeSetupKeepNameAndLevel(t *testing.T) {
	h := testHome(t)
	captured := earlySlog.With("logger", "agent.captured_early")
	mustSetup(t, Options{Home: h})
	captured.Warn("early logger warning", "k", "v")
	log.Print("plain std log line")
	FlushLogQueue()
	text := readText(t, filepath.Join(h, "logs", "agent.log"))
	if !strings.Contains(text, " WARNING agent.captured_early: early logger warning k=v") {
		t.Fatalf("agent.log %q", text)
	}
	if !strings.Contains(text, " INFO root: plain std log line") {
		t.Fatalf("std log line missing %q", text)
	}
	if !strings.Contains(readText(t, filepath.Join(h, "logs", "errors.log")), "early logger warning") {
		t.Fatal("warning missing from errors.log")
	}
}
