package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func resetStartupWatchdogForTests() {
	startupWatchdogHandleMu.Lock()
	handle := startupWatchdogHandle
	startupWatchdogHandle = nil
	state.SetStartupProgressLease(nil)
	startupWatchdogHandleMu.Unlock()
	if handle != nil {
		handle.Disarm()
	}
}

// Fresh singleton, own home, frozen CPU time and the stderr stack dump sent to the null device
func isolateStartupWatchdog(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv(EnvStartupWatchdog, "")
	t.Setenv(EnvStartupWatchdogTimeoutS, "")
	os.Unsetenv(EnvStartupWatchdog)
	os.Unsetenv(EnvStartupWatchdogTimeoutS)
	resetStartupWatchdogForTests()
	saved := startupWatchdogDefaults
	startupWatchdogDefaults.cpuSeconds = func() (float64, bool) { return 0.0, true }
	startupWatchdogDefaults.dumpTraceback = func(f *os.File) error {
		if f != nil {
			return dumpStartupWatchdogTraceback(f)
		}
		null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer null.Close()
		return dumpStartupWatchdogTraceback(null)
	}
	t.Cleanup(func() {
		resetStartupWatchdogForTests()
		startupWatchdogDefaults = saved
	})
	return tmp
}

type watchdogExitCapture struct {
	mu    sync.Mutex
	codes []int
	fired chan struct{}
	once  sync.Once
}

func (c *watchdogExitCapture) exit(code int) {
	c.mu.Lock()
	c.codes = append(c.codes, code)
	c.mu.Unlock()
	c.once.Do(func() { close(c.fired) })
}

func (c *watchdogExitCapture) got() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.codes)
}

func (c *watchdogExitCapture) wait(timeout time.Duration) bool {
	select {
	case <-c.fired:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (c *watchdogExitCapture) isSet() bool { return c.wait(0) }

func captureWatchdogExit(t *testing.T) *watchdogExitCapture {
	t.Helper()
	c := &watchdogExitCapture{fired: make(chan struct{})}
	startupWatchdogDefaults.exit = c.exit
	return c
}

func busyWatchdogProbe() func() (float64, bool) {
	var mu sync.Mutex
	cpu := 0.0
	return func() (float64, bool) {
		mu.Lock()
		defer mu.Unlock()
		cpu += 10.0
		return cpu, true
	}
}

func waitForWatchdogDump(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), "\n") {
			return string(raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("dump %s never appeared: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContractsRestartCodeParityWithGatewayRestart(t *testing.T) {
	if StartupWatchdogRestartExitCode != GatewayServiceRestartExitCode {
		t.Fatalf("%d != %d", StartupWatchdogRestartExitCode, GatewayServiceRestartExitCode)
	}
}

func TestConfigResolutionDefaultTimeout(t *testing.T) {
	isolateStartupWatchdog(t)
	if got := ResolveStartupWatchdogTimeout(); got != DefaultStartupWatchdogTimeoutS {
		t.Fatal(got)
	}
}

func TestConfigResolutionEnvOverride(t *testing.T) {
	isolateStartupWatchdog(t)
	t.Setenv(EnvStartupWatchdogTimeoutS, "120")
	if got := ResolveStartupWatchdogTimeout(); got != 120.0 {
		t.Fatal(got)
	}
}

func TestConfigResolutionEnvOverrideClampedToFloor(t *testing.T) {
	isolateStartupWatchdog(t)
	t.Setenv(EnvStartupWatchdogTimeoutS, "5")
	if got := ResolveStartupWatchdogTimeout(); got != startupWatchdogMinTimeoutS {
		t.Fatal(got)
	}
}

func TestConfigResolutionGarbageEnvFallsBackToDefault(t *testing.T) {
	isolateStartupWatchdog(t)
	t.Setenv(EnvStartupWatchdogTimeoutS, "soon")
	if got := ResolveStartupWatchdogTimeout(); got != DefaultStartupWatchdogTimeoutS {
		t.Fatal(got)
	}
}

func TestConfigResolutionNonpositiveEnvFallsBackToDefault(t *testing.T) {
	isolateStartupWatchdog(t)
	t.Setenv(EnvStartupWatchdogTimeoutS, "-1")
	if got := ResolveStartupWatchdogTimeout(); got != DefaultStartupWatchdogTimeoutS {
		t.Fatal(got)
	}
}

func TestConfigResolutionDisabledValues(t *testing.T) {
	isolateStartupWatchdog(t)
	for _, raw := range []string{"0", "false", "no", "off", "FALSE", "Off"} {
		t.Setenv(EnvStartupWatchdog, raw)
		if !StartupWatchdogDisabled() {
			t.Fatalf("%q must disable", raw)
		}
	}
}

func TestConfigResolutionEnabledValues(t *testing.T) {
	isolateStartupWatchdog(t)
	for _, raw := range []string{"", "1", "true", "yes"} {
		t.Setenv(EnvStartupWatchdog, raw)
		if StartupWatchdogDisabled() {
			t.Fatalf("%q must not disable", raw)
		}
	}
}

func TestArmDisarmArmReturnsLiveHandle(t *testing.T) {
	isolateStartupWatchdog(t)
	handle := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	if handle == nil || !handle.IsAlive() || handle.Disarmed() {
		t.Fatalf("handle %+v", handle)
	}
	DisarmStartupWatchdog()
	handle.Join(2 * time.Second)
	if handle.IsAlive() {
		t.Fatal("still alive after disarm")
	}
}

func TestArmDisarmArmIsIdempotent(t *testing.T) {
	isolateStartupWatchdog(t)
	first := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	second := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	if first == nil || first != second {
		t.Fatalf("%p != %p", first, second)
	}
	DisarmStartupWatchdog()
}

func TestArmDisarmDisarmPreventsFire(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	handle := armStartupWatchdog(0.2, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	DisarmStartupWatchdog()
	handle.Join(2 * time.Second)
	if capture.isSet() || len(capture.got()) != 0 {
		t.Fatalf("fired %v", capture.got())
	}
}

func TestArmDisarmDisarmIsIdempotent(t *testing.T) {
	isolateStartupWatchdog(t)
	armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	DisarmStartupWatchdog()
	DisarmStartupWatchdog()
}

func TestArmDisarmDisabledViaEnv(t *testing.T) {
	isolateStartupWatchdog(t)
	t.Setenv(EnvStartupWatchdog, "0")
	if handle := armStartupWatchdog(60, StartupWatchdogRestartExitCode); handle != nil {
		t.Fatal("armed while disabled")
	}
}

func TestArmDisarmRearmAfterDisarmStartsFreshThread(t *testing.T) {
	isolateStartupWatchdog(t)
	first := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	DisarmStartupWatchdog()
	first.Join(2 * time.Second)
	second := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	if second == nil || second == first || !second.IsAlive() {
		t.Fatalf("second %p first %p", second, first)
	}
	DisarmStartupWatchdog()
}

func TestArmDisarmDisarmAfterDeadlineExpiryWinsOverFire(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	inProbe, releaseProbe := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	startupWatchdogDefaults.cpuSeconds = func() (float64, bool) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n >= 2 {
			if n == 2 {
				close(inProbe)
			}
			select {
			case <-releaseProbe:
			case <-time.After(10 * time.Second):
			}
		}
		return 0.0, true
	}
	handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	select {
	case <-inProbe:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never reached")
	}
	DisarmStartupWatchdog()
	close(releaseProbe)
	handle.Join(5 * time.Second)
	if capture.isSet() || len(capture.got()) != 0 {
		t.Fatalf("fired %v", capture.got())
	}
}

func TestKickKickExtendsDeadline(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	if handle := armStartupWatchdog(0.3, StartupWatchdogRestartExitCode); handle == nil {
		t.Fatal("no handle")
	}
	KickStartupWatchdog(60)
	time.Sleep(600 * time.Millisecond)
	if capture.isSet() {
		t.Fatal("fired despite the kick")
	}
	DisarmStartupWatchdog()
}

func TestKickKickWithoutArmIsSafe(t *testing.T) {
	isolateStartupWatchdog(t)
	KickStartupWatchdog(30)
}

func TestKickKickWithGarbageExtraIsSafe(t *testing.T) {
	isolateStartupWatchdog(t)
	armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	KickStartupWatchdog("nonsense")
	DisarmStartupWatchdog()
}

func TestCpuProgressExtensionCpuProgressExtendsInsteadOfFiring(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.maxCPUExtensions = 10_000
	startupWatchdogDefaults.cpuSeconds = busyWatchdogProbe()
	handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	time.Sleep(600 * time.Millisecond)
	if capture.isSet() {
		t.Fatal("fired despite CPU progress")
	}
	if handle.cpuExtensions() < 1 {
		t.Fatalf("extensions %d", handle.cpuExtensions())
	}
	DisarmStartupWatchdog()
}

func TestCpuProgressExtensionCpuExtensionsAreCapped(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.cpuSeconds = busyWatchdogProbe()
	handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	if !capture.wait(10 * time.Second) {
		t.Fatal("never fired")
	}
	if handle.cpuExtensions() != startupWatchdogDefaultMaxCPUExt {
		t.Fatalf("extensions %d", handle.cpuExtensions())
	}
	if got := capture.got(); !slices.Equal(got, []int{StartupWatchdogRestartExitCode}) {
		t.Fatal(got)
	}
}

func TestCpuProgressExtensionNoCpuProgressFires(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	if got := capture.got(); !slices.Equal(got, []int{StartupWatchdogRestartExitCode}) {
		t.Fatal(got)
	}
}

func TestCpuProgressExtensionProbeFailureFailsTowardFiring(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.cpuSeconds = func() (float64, bool) { return 0, false }
	armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
}

func TestProgressLeaseLeasePreventsFiringWithZeroCpu(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	if handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode); handle == nil {
		t.Fatal("no handle")
	}
	ReportStartupProgress(60.0, "state_db_repair")
	time.Sleep(600 * time.Millisecond)
	if capture.isSet() {
		t.Fatal("fired despite the lease")
	}
	DisarmStartupWatchdog()
}

func TestProgressLeaseExpiredLeaseNoLongerProtects(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	if handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode); handle == nil {
		t.Fatal("no handle")
	}
	ReportStartupProgress(0.2, "short_phase")
	if !capture.wait(10 * time.Second) {
		t.Fatal("never fired")
	}
	if got := capture.got(); !slices.Equal(got, []int{StartupWatchdogRestartExitCode}) {
		t.Fatal(got)
	}
}

func TestProgressLeaseLeaseDurationIsClamped(t *testing.T) {
	isolateStartupWatchdog(t)
	handle := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	ReportStartupProgress(1_000_000_000, "greedy")
	handle.mu.Lock()
	remaining := time.Until(handle.leaseUntil).Seconds()
	handle.mu.Unlock()
	if remaining > startupWatchdogMaxLeaseS+1 || remaining <= 0 {
		t.Fatalf("remaining %v", remaining)
	}
	DisarmStartupWatchdog()
}

func TestProgressLeaseLeaseOutranksCpuExtensionCap(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.maxCPUExtensions = 0
	startupWatchdogDefaults.cpuSeconds = busyWatchdogProbe()
	if handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode); handle == nil {
		t.Fatal("no handle")
	}
	ReportStartupProgress(60.0, "post_cap_migration")
	time.Sleep(600 * time.Millisecond)
	if capture.isSet() {
		t.Fatal("fired despite the lease")
	}
	DisarmStartupWatchdog()
}

func TestProgressLeaseLeaseWithoutArmIsSafe(t *testing.T) {
	isolateStartupWatchdog(t)
	ReportStartupProgress(30.0, "x")
	state.ReportStartupProgress(30.0, "x")
}

func TestProgressLeaseLeaseWithGarbageIsSafe(t *testing.T) {
	isolateStartupWatchdog(t)
	handle := armStartupWatchdog(60, StartupWatchdogRestartExitCode)
	ReportStartupProgress("nonsense", "")
	ReportStartupProgress(-5, "")
	handle.mu.Lock()
	count := handle.leaseCount
	handle.mu.Unlock()
	if count != 0 {
		t.Fatalf("lease count %d", count)
	}
	DisarmStartupWatchdog()
}

func TestProgressLeaseLeaseVisibleInDumpRecord(t *testing.T) {
	tmp := isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	state.ReportStartupProgress(0.15, "brief_phase")
	if !capture.wait(10 * time.Second) {
		t.Fatal("never fired")
	}
	content := waitForWatchdogDump(t, GetStartupWatchdogDumpPath(tmp))
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(content, "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record["lease_count"].(float64) < 1 || record["last_lease_phase"] != "brief_phase" {
		t.Fatalf("record %v", record)
	}
}

func TestFireFiresAfterDeadlineWithRestartCode(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	if handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode); handle == nil {
		t.Fatal("no handle")
	}
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	if got := capture.got(); !slices.Equal(got, []int{StartupWatchdogRestartExitCode}) {
		t.Fatal(got)
	}
}

func TestFireFireWritesDumpRecordAndStacks(t *testing.T) {
	tmp := isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	content := waitForWatchdogDump(t, GetStartupWatchdogDumpPath(tmp))
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(content, "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record["tag"] != "startup_watchdog.fired" || record["exit_code"].(float64) != StartupWatchdogRestartExitCode {
		t.Fatalf("record %v", record)
	}
	if math.Abs(record["timeout_s"].(float64)-0.1) > 1e-6 {
		t.Fatalf("timeout_s %v", record["timeout_s"])
	}
	if record["pid"].(float64) != float64(os.Getpid()) || record["last_lease_phase"] != nil {
		t.Fatalf("record %v", record)
	}
	if !strings.Contains(content, "goroutine ") {
		t.Fatalf("no stacks in dump: %.200s", content)
	}
}

func TestFireFileDumpPrecedesStderrDump(t *testing.T) {
	isolateStartupWatchdog(t)
	var mu sync.Mutex
	var writes []string
	startupWatchdogDefaults.dumpTraceback = func(f *os.File) error {
		mu.Lock()
		defer mu.Unlock()
		if f != nil {
			writes = append(writes, "file")
		} else {
			writes = append(writes, "stderr")
		}
		return nil
	}
	startupWatchdogDefaults.exit = func(int) {}
	startupWatchdogDefaults.sleep = func(float64) {}
	newStartupWatchdogHandle(60, StartupWatchdogRestartExitCode).fire()
	mu.Lock()
	defer mu.Unlock()
	if len(writes) < 2 || writes[0] != "file" || writes[1] != "stderr" {
		t.Fatal(writes)
	}
}

func TestFireFireMarksLifecycleExit(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	var mu sync.Mutex
	marked := map[string]any{}
	startupWatchdogDefaults.markExited = func(_ context.Context, code *int, reason string, _ string) {
		mu.Lock()
		defer mu.Unlock()
		marked["code"], marked["reason"] = *code, reason
	}
	armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	mu.Lock()
	defer mu.Unlock()
	if marked["code"] != StartupWatchdogRestartExitCode || marked["reason"] != "startup_liveness_watchdog" {
		t.Fatal(marked)
	}
}

func TestFireCustomExitCode(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	armStartupWatchdog(0.1, 42)
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	if got := capture.got(); !slices.Equal(got, []int{42}) {
		t.Fatal(got)
	}
}

type blockedLogHandler struct{ release chan struct{} }

func (h blockedLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h blockedLogHandler) Handle(context.Context, slog.Record) error {
	<-h.release
	return nil
}
func (h blockedLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h blockedLogHandler) WithGroup(string) slog.Handler      { return h }

func TestBoundedExitExitsEvenWhenLoggingLockIsHeld(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.sleep = func(float64) { time.Sleep(200 * time.Millisecond) }
	blocker := blockedLogHandler{release: make(chan struct{})}
	prev := slog.Default()
	slog.SetDefault(slog.New(blocker))
	var handle *StartupWatchdogHandle
	defer func() {
		close(blocker.release)
		if handle != nil {
			handle.Join(10 * time.Second)
		}
		slog.SetDefault(prev)
	}()
	handle = armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	if !capture.wait(10 * time.Second) {
		t.Fatal("escort never exited")
	}
	if !slices.Contains(capture.got(), StartupWatchdogRestartExitCode) {
		t.Fatal(capture.got())
	}
	if handle.IsAlive() == false {
		t.Fatal("fire path finished although its log call is blocked")
	}
}

func TestBoundedExitExitsEvenWhenDumpWriteHangs(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.sleep = func(float64) { time.Sleep(200 * time.Millisecond) }
	forever := make(chan struct{})
	startupWatchdogDefaults.writeDumpRecord = func(*pyjson.Object) {
		select {
		case <-forever:
		case <-time.After(30 * time.Second):
		}
	}
	handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	if !capture.wait(10 * time.Second) {
		t.Fatal("escort never exited")
	}
	if !slices.Contains(capture.got(), StartupWatchdogRestartExitCode) {
		t.Fatal(capture.got())
	}
	close(forever)
	handle.Join(10 * time.Second)
}

func TestBoundedExitEscortStandsDownWhenFireCompletes(t *testing.T) {
	isolateStartupWatchdog(t)
	capture := captureWatchdogExit(t)
	startupWatchdogDefaults.sleep = func(float64) { time.Sleep(500 * time.Millisecond) }
	handle := armStartupWatchdog(0.1, StartupWatchdogRestartExitCode)
	if handle == nil {
		t.Fatal("no handle")
	}
	if !capture.wait(5 * time.Second) {
		t.Fatal("never fired")
	}
	time.Sleep(800 * time.Millisecond)
	if got := capture.got(); !slices.Equal(got, []int{StartupWatchdogRestartExitCode}) {
		t.Fatal(got)
	}
}

func TestDumpPathDumpWriteFailureIsSwallowed(t *testing.T) {
	isolateStartupWatchdog(t)
	saved := startupWatchdogDumpPathFn
	startupWatchdogDumpPathFn = func() string { return "/dev/null/nope" }
	t.Cleanup(func() { startupWatchdogDumpPathFn = saved })
	writeStartupWatchdogDumpRecord(pyjson.ObjectOf("tag", "x"))
}
