package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Drops every session, the dict clear of hermes test setup and teardown
func (t *whatsappSessionTable) clear() {
	t.ids = nil
	t.byID = map[string]*whatsappOnboardingSession{}
}

// Expiry timer the fixture fires by hand
type fakeWhatsappTimer struct {
	fn        func()
	cancelled bool
}

func (f *fakeWhatsappTimer) Stop() bool {
	f.cancelled = true
	return true
}

// Runs the expiry callback unless the timer was stopped
func (f *fakeWhatsappTimer) fire() {
	if !f.cancelled {
		f.fn()
	}
}

// Arguments one pairing worker was started with
type whatsappWorkerStart struct {
	ctx                          context.Context
	pairingID, sessionPath, mode string
}

// Default home with a linked WhatsApp session, hand fired timers and recorded worker starts
type whatsappFixture struct {
	t       *testing.T
	home    string
	session string
	creds   string
	starts  []whatsappWorkerStart
	timers  []*fakeWhatsappTimer
}

const whatsappLinkedCreds = `{"me":{"id":"15550001111:1@s.whatsapp.net"},"registered":true}`

func newWhatsappFixture(t *testing.T) *whatsappFixture {
	t.Helper()
	tmp := t.TempDir()
	f := &whatsappFixture{t: t, home: filepath.Join(tmp, "home"), creds: whatsappLinkedCreds}
	f.session = filepath.Join(f.home, "platforms", "whatsapp", "session")
	t.Setenv("ICEBATH_HOME", f.home)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "locks"))
	writeTestFile(t, filepath.Join(f.session, "creds.json"), f.creds)
	writeTestFile(t, filepath.Join(f.session, "app-state.json"), "preserve")
	whatsappOnboardingMu.Lock()
	whatsappOnboardingSessions.clear()
	whatsappOnboardingMu.Unlock()
	savedTimer, savedWorker, savedSpawn := whatsappExpiryTimer, whatsappStartWorker, spawnWhatsappPairingProcess
	savedRestart, savedRename, savedStart := restartGatewayAfterWhatsappOnboarding, whatsappRename, whatsappProcessStartTime
	whatsappExpiryTimer = func(d time.Duration, fn func()) whatsappTimer {
		timer := &fakeWhatsappTimer{fn: fn}
		f.timers = append(f.timers, timer)
		return timer
	}
	whatsappStartWorker = func(ctx context.Context, pairingID, sessionPath, mode string) error {
		f.starts = append(f.starts, whatsappWorkerStart{ctx, pairingID, sessionPath, mode})
		return nil
	}
	t.Cleanup(func() {
		whatsappOnboardingMu.Lock()
		for _, id := range whatsappOnboardingSessions.order() {
			record := whatsappOnboardingSessions.get(id)
			if record.expiryTimer != nil {
				record.expiryTimer.Stop()
			}
			gateway.ReleaseScopedLock(whatsappPairingLockScope, record.sessionPath)
		}
		whatsappOnboardingSessions.clear()
		whatsappOnboardingMu.Unlock()
		whatsappExpiryTimer, whatsappStartWorker, spawnWhatsappPairingProcess = savedTimer, savedWorker, savedSpawn
		restartGatewayAfterWhatsappOnboarding, whatsappRename, whatsappProcessStartTime = savedRestart, savedRename, savedStart
	})
	return f
}

// Start route call with the model defaults overridden by the given fields
func (f *whatsappFixture) start(profile string, fields ...any) (*pyjson.Object, error) {
	body := pyjson.ObjectOf("mode", "bot", "allowed_users", "", "profile", nil, "reset_session", false)
	for i := 0; i+1 < len(fields); i += 2 {
		body.Set(fields[i].(string), fields[i+1])
	}
	return startWhatsappOnboarding(context.Background(), body, profile)
}

// Start that must succeed
func (f *whatsappFixture) mustStart(fields ...any) *pyjson.Object {
	f.t.Helper()
	result, err := f.start("", fields...)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

// Status code of a refused start
func (f *whatsappFixture) refusedStart(fields ...any) *HTTPException {
	f.t.Helper()
	_, err := f.start("", fields...)
	refusal, ok := asHTTPException(err)
	if !ok {
		f.t.Fatalf("start error %v is no HTTPException", err)
	}
	return refusal
}

// Whether another process can take the session lock the gateway takes
func (f *whatsappFixture) gatewayCanAcquire() bool {
	f.t.Helper()
	return gatewayCanAcquireSession(f.t, f.session)
}

// Whether a process with the extra environment can take the session lock of the session dir
func gatewayCanAcquireSession(t *testing.T, session string, env ...string) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWhatsappLockProbe$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), env...), "ICEBATH_TEST_WHATSAPP_LOCK_PROBE="+session)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lock probe: %v\n%s", err, out)
	}
	switch {
	case strings.Contains(string(out), "probe-acquired=true"):
		return true
	case strings.Contains(string(out), "probe-acquired=false"):
		return false
	}
	t.Fatalf("lock probe printed no verdict:\n%s", out)
	return false
}

// Child process entry of the lock probe, a no op in the parent test run
func TestHelperWhatsappLockProbe(t *testing.T) {
	session := os.Getenv("ICEBATH_TEST_WHATSAPP_LOCK_PROBE")
	if session == "" {
		t.Skip("runs only as the lock probe process the pairing cases start")
	}
	acquired, _, err := gateway.AcquireScopedLock(context.Background(), whatsappPairingLockScope, session, nil)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("probe-acquired=%v\n", acquired)
}

// Session record of a pairing id
func whatsappRecord(t *testing.T, pairingID string) *whatsappOnboardingSession {
	t.Helper()
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	record := whatsappOnboardingSessions.get(pairingID)
	if record == nil {
		t.Fatalf("no session %q", pairingID)
	}
	return record
}

// Pairing bridge double with a scripted stdout and a settable exit code
type fakeBridge struct {
	mu         sync.Mutex
	stdout     io.Reader
	returncode *int
	pollNone   bool
	wait       func(timeout time.Duration) (int, bool)
}

func (b *fakeBridge) Stdout() io.Reader { return b.stdout }

func (b *fakeBridge) Poll() *int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pollNone || b.returncode == nil {
		return nil
	}
	code := *b.returncode
	return &code
}

func (b *fakeBridge) setCode(code int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.returncode = &code
}

func (b *fakeBridge) Wait(timeout time.Duration) (int, bool) {
	if b.wait != nil {
		return b.wait(timeout)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.returncode == nil {
		code := 0
		b.returncode = &code
	}
	return *b.returncode, true
}

func (b *fakeBridge) Terminate() { b.setCode(-15) }

// Stdout of a generator, each string a line and each func run when the reader reaches it
type scriptedStdout struct {
	steps []any
	buf   []byte
}

func (s *scriptedStdout) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if len(s.steps) == 0 {
			return 0, io.EOF
		}
		step := s.steps[0]
		s.steps = s.steps[1:]
		switch v := step.(type) {
		case string:
			s.buf = []byte(v + "\n")
		case func():
			v()
		}
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// Start time fingerprint of this process as an integer
func ownStartTime(t *testing.T) int64 {
	t.Helper()
	started := gateway.GetProcessStartTime(os.Getpid())
	if started == nil {
		t.Fatal("no start time for this process")
	}
	n, err := pyint.Int64(started)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRepairPreservesCredentialsAndStartsFreshPairing(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	if result.GetOr("status", nil) != "starting" || result.GetOr("qr_payload", 1) != nil || result.GetOr("account_id", 1) != nil {
		t.Fatalf("result %v", result)
	}
	if len(f.starts) != 1 || f.starts[0].sessionPath != f.session {
		t.Fatalf("starts %v", f.starts)
	}
	if _, err := os.Stat(filepath.Join(f.session, "creds.json")); err == nil {
		t.Fatal("creds.json still in the session")
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(f.session), "session-backup-*", "session"))
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if got, _ := os.ReadFile(filepath.Join(backups[0], "creds.json")); string(got) != f.creds {
		t.Fatalf("backup creds %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(backups[0], "app-state.json")); string(got) != "preserve" {
		t.Fatalf("backup app state %q", got)
	}
	if !regexp.MustCompile(`^session-backup-\d{8}T\d{6}Z-.+$`).MatchString(filepath.Base(filepath.Dir(backups[0]))) {
		t.Fatalf("backup dir %s", backups[0])
	}
}

func TestReservationBlocksGatewayUntilCancelBeforeWorkerStart(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	if f.gatewayCanAcquire() {
		t.Fatal("gateway took the reserved session")
	}
	cancelWhatsappOnboarding(result.GetOr("pairing_id", "").(string))
	if !f.gatewayCanAcquire() {
		t.Fatal("cancel kept the session reserved")
	}
	if !f.timers[0].cancelled {
		t.Fatal("expiry timer still armed")
	}
}

func TestUnattendedSetupExpiresAndReleasesReservation(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	if f.gatewayCanAcquire() {
		t.Fatal("gateway took the reserved session")
	}
	f.timers[0].fire()
	if status := whatsappRecord(t, result.GetOr("pairing_id", "").(string)).status; status != "expired" {
		t.Fatalf("status %s", status)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("expiry kept the session reserved")
	}
}

func TestFailedProcessStartReleasesReservation(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		if f.gatewayCanAcquire() {
			t.Error("gateway took the session while the bridge was starting")
		}
		return nil, errors.New("cannot start bridge")
	}
	start := f.starts[0]
	runWhatsappPairing(context.Background(), start.pairingID, start.sessionPath, start.mode)
	if status := whatsappRecord(t, result.GetOr("pairing_id", "").(string)).status; status != "error" {
		t.Fatalf("status %s", status)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("failed start kept the session reserved")
	}
}

func TestRecycledPidDoesNotBlockRepair(t *testing.T) {
	f := newWhatsappFixture(t)
	started := ownStartTime(t)
	writeTestFile(t, filepath.Join(f.session, "bridge.pid"), fmt.Sprintf("%d\n%d\n19877", os.Getpid(), started-1))
	if status := f.mustStart("reset_session", true).GetOr("status", nil); status != "starting" {
		t.Fatalf("status %v", status)
	}
}

func TestMatchingPidFingerprintRefusesRepair(t *testing.T) {
	f := newWhatsappFixture(t)
	writeTestFile(t, filepath.Join(f.session, "bridge.pid"), fmt.Sprintf("%d\n%d", os.Getpid(), ownStartTime(t)))
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
		t.Fatalf("refusal %v", refusal)
	}
	if _, err := os.Stat(filepath.Join(f.session, "creds.json")); err != nil {
		t.Fatal("refused repair moved the credentials")
	}
}

func TestReservationCoversConnectedEventAndCredentialFlush(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	pairingID := result.GetOr("pairing_id", "").(string)
	record := whatsappRecord(t, pairingID)
	proc := &fakeBridge{}
	proc.stdout = &scriptedStdout{steps: []any{
		`{"event":"connected","user":{"id":"15550001111"}}`,
		func() {
			if record.status != "connected" {
				t.Errorf("status %s", record.status)
			}
			if f.gatewayCanAcquire() {
				t.Error("gateway took the session before the bridge exited")
			}
			if _, err := getWhatsappOnboardingStatus(pairingID); err != nil {
				t.Error(err)
			}
			if f.gatewayCanAcquire() {
				t.Error("a status poll released the session before the bridge exited")
			}
		},
	}}
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		return proc, nil
	}
	start := f.starts[0]
	runWhatsappPairing(context.Background(), start.pairingID, start.sessionPath, start.mode)
	if record.status != "connected" || record.sessionReserved {
		t.Fatalf("status %s reserved %v", record.status, record.sessionReserved)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("exited bridge kept the session reserved")
	}
	if !f.timers[0].cancelled {
		t.Fatal("expiry timer still armed")
	}
}

func TestExpiryStopsActiveWriterAndReleasesReservation(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	proc := &fakeBridge{}
	proc.stdout = &scriptedStdout{steps: []any{
		`{"event":"qr","qr":"test"}`,
		func() {
			if f.gatewayCanAcquire() {
				t.Error("gateway took the session during pairing")
			}
			f.timers[0].fire()
			if proc.Poll() == nil {
				t.Error("expiry left the bridge running")
			}
		},
		`{"event":"connected"}`,
	}}
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		return proc, nil
	}
	start := f.starts[0]
	runWhatsappPairing(context.Background(), start.pairingID, start.sessionPath, start.mode)
	if status := whatsappRecord(t, result.GetOr("pairing_id", "").(string)).status; status != "expired" {
		t.Fatalf("status %s", status)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("expiry kept the session reserved")
	}
}

func TestExpiryDuringSpawnKeepsReservationUntilLateProcessStops(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	proc := &fakeBridge{}
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		f.timers[0].fire()
		if f.gatewayCanAcquire() {
			t.Error("expiry released the session while the bridge was starting")
		}
		return proc, nil
	}
	start := f.starts[0]
	runWhatsappPairing(context.Background(), start.pairingID, start.sessionPath, start.mode)
	if proc.Poll() == nil {
		t.Fatal("late bridge left running")
	}
	if status := whatsappRecord(t, result.GetOr("pairing_id", "").(string)).status; status != "expired" {
		t.Fatalf("status %s", status)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("stopped late bridge kept the session reserved")
	}
}

func TestThreadStartFailureReleasesReservation(t *testing.T) {
	f := newWhatsappFixture(t)
	whatsappStartWorker = func(ctx context.Context, pairingID, sessionPath, mode string) error {
		return errors.New("cannot start thread")
	}
	if _, err := f.start("", "reset_session", true); err == nil || !strings.Contains(err.Error(), "cannot start thread") {
		t.Fatalf("start error %v", err)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("failed launch kept the session reserved")
	}
	if !f.timers[0].cancelled {
		t.Fatal("expiry timer still armed")
	}
}

func TestApplyWaitsForFlushBeforeRestartingGateway(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	pairingID := result.GetOr("pairing_id", "").(string)
	record := whatsappRecord(t, pairingID)
	proc := &fakeBridge{}
	proc.wait = func(timeout time.Duration) (int, bool) {
		if f.gatewayCanAcquire() {
			t.Error("gateway took the session before the bridge flushed")
		}
		proc.setCode(0)
		return 0, true
	}
	whatsappOnboardingMu.Lock()
	record.status, record.proc = "connected", proc
	whatsappOnboardingMu.Unlock()
	restartGatewayAfterWhatsappOnboarding = func(ctx context.Context, profile string) (*pyjson.Object, error) {
		if code := proc.Poll(); code == nil || *code != 0 {
			t.Error("restart before the bridge exited")
		}
		if !f.gatewayCanAcquire() {
			t.Error("restart before the session was released")
		}
		return pyjson.ObjectOf("restart_started", true), nil
	}
	applied, err := applyWhatsappOnboarding(context.Background(), pairingID, pyjson.ObjectOf("mode", nil, "allowed_users", nil, "profile", nil), "")
	if err != nil || applied.GetOr("ok", nil) != true {
		t.Fatalf("apply %v %v", applied, err)
	}
}

func TestUnverifiableLivePidStaysBlocked(t *testing.T) {
	f := newWhatsappFixture(t)
	writeTestFile(t, filepath.Join(f.session, "bridge.pid"), fmt.Sprintf("%d\n123", os.Getpid()))
	whatsappProcessStartTime = func(pid int) any { return nil }
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
		t.Fatalf("refusal %v", refusal)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("refused repair kept the session reserved")
	}
}

func TestRegularStartKeepsExistingLink(t *testing.T) {
	f := newWhatsappFixture(t)
	if status := f.mustStart().GetOr("status", nil); status != "connected" {
		t.Fatalf("status %v", status)
	}
	if len(f.starts) != 0 {
		t.Fatalf("starts %v", f.starts)
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("creds %q", got)
	}
	if backups, _ := filepath.Glob(filepath.Join(filepath.Dir(f.session), "session-backup-*")); len(backups) != 0 {
		t.Fatalf("backups %v", backups)
	}
}

func TestRepairRefusesLiveBridgeWithoutMovingCredentials(t *testing.T) {
	f := newWhatsappFixture(t)
	writeTestFile(t, filepath.Join(f.session, "bridge.pid"), fmt.Sprint(os.Getpid()))
	refusal := f.refusedStart("reset_session", true)
	if refusal.StatusCode != 409 || !strings.Contains(strings.ToLower(refusal.Detail.(string)), "bridge") {
		t.Fatalf("refusal %v", refusal)
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("creds %q", got)
	}
	if len(f.starts) != 0 {
		t.Fatalf("starts %v", f.starts)
	}
}

func TestRepairRefusesPendingPairing(t *testing.T) {
	f := newWhatsappFixture(t)
	record := &whatsappOnboardingSession{mode: "bot", sessionPath: f.session, expiresAt: "2099-01-01T00:00:00Z",
		expiresAtTS: epochNow() + 600, status: "installing"}
	whatsappOnboardingMu.Lock()
	whatsappOnboardingSessions.set("pending", record)
	whatsappOnboardingMu.Unlock()
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
		t.Fatalf("refusal %v", refusal)
	}
	if record.status != "installing" {
		t.Fatalf("status %s", record.status)
	}
	if _, err := os.Stat(filepath.Join(f.session, "creds.json")); err != nil {
		t.Fatal("refused repair moved the credentials")
	}
}

func TestRepairRefusesSessionLockOwnedByLiveGateway(t *testing.T) {
	f := newWhatsappFixture(t)
	acquired, _, err := gateway.AcquireScopedLock(context.Background(), whatsappPairingLockScope, f.session, nil)
	if err != nil || !acquired {
		t.Fatalf("own lock %v %v", acquired, err)
	}
	t.Cleanup(func() { gateway.ReleaseScopedLock(whatsappPairingLockScope, f.session) })
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
		t.Fatalf("refusal %v", refusal)
	}
	if f.gatewayCanAcquire() {
		t.Fatal("refused repair released the live owner's lock")
	}
	if _, err := os.Stat(filepath.Join(f.session, "creds.json")); err != nil {
		t.Fatal("refused repair moved the credentials")
	}
}

func TestFailedBackupAbortsBeforePairing(t *testing.T) {
	f := newWhatsappFixture(t)
	whatsappRename = func(oldpath, newpath string) error {
		if oldpath == f.session {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrPermission}
		}
		return os.Rename(oldpath, newpath)
	}
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 500 {
		t.Fatalf("refusal %v", refusal)
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("creds %q", got)
	}
	if len(f.starts) != 0 {
		t.Fatalf("starts %v", f.starts)
	}
}

func TestOpenCredentialsRefuseBackupOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms(windows)")
	}
	f := newWhatsappFixture(t)
	held, err := os.Open(filepath.Join(f.session, "creds.json"))
	if err != nil {
		t.Fatal(err)
	}
	refusal := f.refusedStart("reset_session", true)
	held.Close()
	if refusal.StatusCode != 500 {
		t.Fatalf("refusal %v", refusal)
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("creds %q", got)
	}
	if len(f.starts) != 0 {
		t.Fatalf("starts %v", f.starts)
	}
}

func TestApplyRetainsFlushingBridgeForResetGuard(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart()
	pairingID := result.GetOr("pairing_id", "").(string)
	record := whatsappRecord(t, pairingID)
	whatsappOnboardingMu.Lock()
	record.proc = &fakeBridge{pollNone: true}
	whatsappOnboardingMu.Unlock()
	restartGatewayAfterWhatsappOnboarding = func(ctx context.Context, profile string) (*pyjson.Object, error) {
		return pyjson.ObjectOf("restart_started", true), nil
	}
	if _, err := applyWhatsappOnboarding(context.Background(), pairingID, pyjson.ObjectOf("mode", nil, "allowed_users", nil, "profile", nil), ""); err != nil {
		t.Fatal(err)
	}
	if whatsappRecord(t, pairingID) != record {
		t.Fatal("apply dropped the flushing bridge's session")
	}
	if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
		t.Fatalf("refusal %v", refusal)
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("creds %q", got)
	}
}

func TestCancelDuringProcessStartCannotRaceSessionBackup(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	pairingID := result.GetOr("pairing_id", "").(string)
	record := whatsappRecord(t, pairingID)
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		cancelWhatsappOnboarding(pairingID)
		if refusal := f.refusedStart("reset_session", true); refusal.StatusCode != 409 {
			t.Errorf("refusal %v", refusal)
		}
		return nil, errors.New("pairing cancelled during process startup")
	}
	start := f.starts[0]
	runWhatsappPairing(context.Background(), start.pairingID, start.sessionPath, start.mode)
	if record.status != "cancelled" || record.startingProcess {
		t.Fatalf("status %s starting %v", record.status, record.startingProcess)
	}
	if status := f.mustStart("reset_session", true).GetOr("status", nil); status != "starting" {
		t.Fatalf("status %v", status)
	}
}

// Work profile beside the default home with its own linked WhatsApp session
func workProfileSession(t *testing.T, f *whatsappFixture) (string, string) {
	t.Helper()
	other := filepath.Join(f.home, "profiles", "work")
	writeTestFile(t, filepath.Join(other, "config.yaml"), "model: test\n")
	session := filepath.Join(other, "platforms", "whatsapp", "session")
	writeTestFile(t, filepath.Join(session, "creds.json"), `{"registered":true}`)
	return other, session
}

func TestRepairSelectedProfileLeavesDefaultSessionUntouched(t *testing.T) {
	f := newWhatsappFixture(t)
	other, session := workProfileSession(t, f)
	result, err := f.start("", "profile", "work", "reset_session", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.GetOr("status", nil) != "starting" || f.starts[0].sessionPath != session {
		t.Fatalf("result %v starts %v", result, f.starts)
	}
	if _, err := os.Stat(filepath.Join(session, "creds.json")); err == nil {
		t.Fatal("work session creds not backed up")
	}
	if got, _ := os.ReadFile(filepath.Join(f.session, "creds.json")); string(got) != f.creds {
		t.Fatalf("default creds %q", got)
	}
	var observed []string
	spawnWhatsappPairingProcess = func(ctx context.Context, sessionPath, mode string) (whatsappPairingProc, error) {
		observed = append(observed, home.Dir(ctx))
		return nil, errors.New("stop after checking the worker profile")
	}
	start := f.starts[0]
	runWhatsappPairing(start.ctx, start.pairingID, start.sessionPath, start.mode)
	if len(observed) != 1 || observed[0] != other {
		t.Fatalf("worker home %v want %s", observed, other)
	}
}

func TestQueryProfileIsHonoredForRepair(t *testing.T) {
	f := newWhatsappFixture(t)
	_, session := workProfileSession(t, f)
	app := NewFastAPI("", "", nil, nil)
	app.IncludeRouter(messagingRouter(), "")
	c := newTestClient(t, app)
	response := c.request("POST", "/api/messaging/whatsapp/onboarding/start?profile=work", []byte(`{"reset_session": true}`),
		"content-type", "application/json")
	if response.Status != 200 {
		t.Fatalf("status %d body %s err %v", response.Status, response.Body, response.AppErr)
	}
	if status := response.JSON(t).(*pyjson.Object).GetOr("status", nil); status != "starting" {
		t.Fatalf("status %v", status)
	}
	if f.starts[0].sessionPath != session {
		t.Fatalf("starts %v", f.starts)
	}
	if _, err := os.Stat(filepath.Join(f.session, "creds.json")); err != nil {
		t.Fatal("default session creds moved")
	}
}

// Pairing process double of the onboarding cases, stopped only by terminate
type fakeOnboardingProc struct {
	fakeBridge
	terminated bool
}

func (p *fakeOnboardingProc) Poll() *int {
	if !p.terminated {
		return nil
	}
	code := *p.returncode
	return &code
}

func (p *fakeOnboardingProc) Terminate() { p.terminated = true }

func (p *fakeOnboardingProc) Wait(timeout time.Duration) (int, bool) { return *p.returncode, true }

func TestApplyWhatsappOnboardingSavesPairingPolicy(t *testing.T) {
	dashboardTestApp(t)
	h := home.Process()
	saved := restartGatewayAfterWhatsappOnboarding
	t.Cleanup(func() { restartGatewayAfterWhatsappOnboarding = saved })
	restartGatewayAfterWhatsappOnboarding = func(ctx context.Context, profile string) (*pyjson.Object, error) {
		return pyjson.ObjectOf("restart_started", true, "restart_pid", 12345), nil
	}
	record := &whatsappOnboardingSession{mode: "bot", sessionPath: filepath.Join(t.TempDir(), "session"),
		expiresAt: "2099-01-01T00:00:00Z", expiresAtTS: epochNow() + 600, status: "connected"}
	whatsappOnboardingMu.Lock()
	whatsappOnboardingSessions.clear()
	whatsappOnboardingSessions.set("pairing", record)
	whatsappOnboardingMu.Unlock()
	writeTestFile(t, filepath.Join(h, ".env"), "WHATSAPP_ALLOWED_USERS=15551234567\n")
	result, err := applyWhatsappOnboarding(context.Background(), "pairing", pyjson.ObjectOf("mode", "bot", "allowed_users", "", "profile", nil), "")
	if err != nil || result.GetOr("ok", nil) != true {
		t.Fatalf("apply %v %v", result, err)
	}
	env := config.LoadEnv(h)
	if env["WHATSAPP_MODE"] != "bot" || env["WHATSAPP_DM_POLICY"] != "pairing" || env["WHATSAPP_ENABLED"] != "true" {
		t.Fatalf("env %v", env)
	}
	if env["WHATSAPP_ALLOWED_USERS"] != "15551234567" {
		t.Fatalf("allowlist removed %v", env)
	}
	cfg, err := config.ReadRawConfig(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	platforms, _ := cfg["platforms"].(map[string]any)
	whatsapp, _ := platforms["whatsapp"].(map[string]any)
	if whatsapp["enabled"] != true {
		t.Fatalf("config %v", cfg)
	}
	whatsappOnboardingMu.Lock()
	defer whatsappOnboardingMu.Unlock()
	if whatsappOnboardingSessions.get("pairing") != nil {
		t.Fatal("applied pairing still listed")
	}
}

func TestStartWhatsappOnboardingExistingCredsReturnsLinkedAccount(t *testing.T) {
	dashboardTestApp(t)
	sessionDir := filepath.Join(t.TempDir(), "session")
	writeTestFile(t, filepath.Join(sessionDir, "creds.json"), `{"me":{"id":"15551234567:1@s.whatsapp.net","name":"Hermes Bot"}}`)
	code := 1
	oldProc := &fakeOnboardingProc{fakeBridge: fakeBridge{returncode: &code}}
	oldRecord := &whatsappOnboardingSession{proc: oldProc, mode: "bot", sessionPath: sessionDir,
		expiresAt: "2099-01-01T00:00:00Z", expiresAtTS: epochNow() + 600, status: "starting"}
	whatsappOnboardingMu.Lock()
	whatsappOnboardingSessions.clear()
	whatsappOnboardingSessions.set("old", oldRecord)
	whatsappOnboardingMu.Unlock()
	savedPath, savedToken := whatsappSessionPath, whatsappTokenURLSafe
	t.Cleanup(func() {
		whatsappSessionPath, whatsappTokenURLSafe = savedPath, savedToken
		whatsappOnboardingMu.Lock()
		whatsappOnboardingSessions.clear()
		whatsappOnboardingMu.Unlock()
	})
	whatsappSessionPath = func(context.Context) string { return sessionDir }
	whatsappTokenURLSafe = func(int) string { return "existing-creds" }
	result, err := startWhatsappOnboarding(context.Background(),
		pyjson.ObjectOf("mode", "self-chat", "allowed_users", "", "profile", nil, "reset_session", false), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"pairing_id": "existing-creds", "status": "connected", "qr_payload": nil,
		"account_id": "15551234567:1@s.whatsapp.net", "account_name": "Hermes Bot", "account_phone": "15551234567"}
	for k, v := range want {
		if got := result.GetOr(k, "missing"); got != v {
			t.Fatalf("%s = %v want %v", k, got, v)
		}
	}
	if oldRecord.status != "cancelled" || !oldProc.terminated {
		t.Fatalf("old status %s terminated %v", oldRecord.status, oldProc.terminated)
	}
	if phone := whatsappRecord(t, "existing-creds").accountPhone; phone != "15551234567" {
		t.Fatalf("stored phone %v", phone)
	}
}

// Arguments one pairing transport run was started with
type pairingTransportCall struct {
	session, mode string
	env           map[string]string
	pairJSON      bool
}

// Replaces the pairing transport for the case, restoring the in process bridge after
func installPairingTransport(t *testing.T, fn func(ctx context.Context, session, mode string, env map[string]string, out io.Writer, pairJSON bool) int) {
	t.Helper()
	saved := whatsappRunPairing
	t.Cleanup(func() { whatsappRunPairing = saved })
	whatsappRunPairing = fn
}

// Everything the pairing pipe carries until the bridge releases it, failing when it stays open
func drainPairingOutput(t *testing.T, proc whatsappPairingProc) string {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	read := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(proc.Stdout())
		read <- result{data, err}
	}()
	select {
	case r := <-read:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return string(r.data)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing kept its output pipe open")
		return ""
	}
}

func TestWhatsappPairingTerminateReleasesTheProcessItStopped(t *testing.T) {
	cancelled := make(chan struct{})
	installPairingTransport(t, func(ctx context.Context, session, mode string, env map[string]string, out io.Writer, pairJSON bool) int {
		io.WriteString(out, `{"event":"qr","qr":"scan-me"}`+"\n")
		<-ctx.Done()
		close(cancelled)
		return -15
	})
	pairing, err := spawnWhatsappPairingBridge(context.Background(), filepath.Join(t.TempDir(), "session"), "bot")
	if err != nil {
		t.Fatal(err)
	}
	if pairing.Poll() != nil {
		t.Fatal("pairing stopped before terminate")
	}
	terminateWhatsappPairing(pairing)
	code := pairing.Poll()
	if code == nil {
		t.Fatal("terminate left the pairing running")
	}
	if *code != -15 {
		t.Fatalf("exit code %d want -15", *code)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the pairing transport never saw the cancellation")
	}
	if output := drainPairingOutput(t, pairing); output != `{"event":"qr","qr":"scan-me"}`+"\n" {
		t.Fatalf("output %q", output)
	}
	terminateWhatsappPairing(pairing)
	if again := pairing.Poll(); again == nil || *again != -15 {
		t.Fatalf("second terminate changed the exit code to %v", again)
	}
}

func TestDashboardPairingRunsPairJSONBridgeInProcess(t *testing.T) {
	var calls []pairingTransportCall
	installPairingTransport(t, func(ctx context.Context, session, mode string, env map[string]string, out io.Writer, pairJSON bool) int {
		calls = append(calls, pairingTransportCall{session, mode, env, pairJSON})
		line, _ := pyjson.Dumps(pyjson.ObjectOf("event", "started", "session", session), pyjson.Separators(",", ":"))
		io.WriteString(out, line+"\n")
		return 0
	})
	session := filepath.Join(t.TempDir(), "nested", "session")
	child, err := spawnWhatsappPairingBridge(context.Background(), session, "bot")
	if err != nil {
		t.Fatal(err)
	}
	output := drainPairingOutput(t, child)
	code, exited := child.Wait(10 * time.Second)
	if !exited || code != 0 {
		t.Fatalf("bridge exited %v with %d output %s", exited, code, output)
	}
	if info, err := os.Stat(session); err != nil || !info.IsDir() {
		t.Fatalf("session dir not prepared: %v", err)
	}
	want := pairingTransportCall{session, "bot", map[string]string{"WHATSAPP_MODE": "bot", "WHATSAPP_DM_POLICY": "pairing"}, true}
	if len(calls) != 1 || calls[0].session != want.session || calls[0].mode != want.mode ||
		calls[0].pairJSON != want.pairJSON || !maps.Equal(calls[0].env, want.env) {
		t.Fatalf("transport calls %+v want %+v", calls, want)
	}
	result, err := pyjson.LoadsDict(strings.TrimSpace(output))
	if err != nil {
		t.Fatalf("output %q: %v", output, err)
	}
	if result.GetOr("event", nil) != "started" || result.GetOr("session", nil) != session {
		t.Fatalf("event %v", result)
	}
}

func TestDashboardReportsPairingTransportFailure(t *testing.T) {
	f := newWhatsappFixture(t)
	result := f.mustStart("reset_session", true)
	pairingID := result.GetOr("pairing_id", "").(string)
	record := whatsappRecord(t, pairingID)
	start := f.starts[0]
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWhatsappPairing(start.ctx, start.pairingID, start.sessionPath, start.mode)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("pairing against an unreachable server never finished")
	}
	whatsappOnboardingMu.Lock()
	status, message, proc, reserved := record.status, record.err, record.proc, record.sessionReserved
	whatsappOnboardingMu.Unlock()
	if status != "error" || message != "WhatsApp pairing process exited with code 1." {
		t.Fatalf("status %s error %v", status, message)
	}
	if code := proc.Poll(); code == nil || *code != 1 {
		t.Fatalf("bridge exit %v", code)
	}
	if reserved || !f.gatewayCanAcquire() {
		t.Fatal("failed pairing kept the session reserved")
	}
	if !f.timers[0].cancelled {
		t.Fatal("expiry timer still armed")
	}
}

func TestCancelStopsInProcessPairingAndReleasesReservation(t *testing.T) {
	f := newWhatsappFixture(t)
	cancelled := make(chan struct{})
	installPairingTransport(t, func(ctx context.Context, session, mode string, env map[string]string, out io.Writer, pairJSON bool) int {
		io.WriteString(out, `{"event":"qr","qr":"scan-me"}`+"\n")
		<-ctx.Done()
		close(cancelled)
		return -15
	})
	result := f.mustStart("reset_session", true)
	pairingID := result.GetOr("pairing_id", "").(string)
	start := f.starts[0]
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWhatsappPairing(start.ctx, start.pairingID, start.sessionPath, start.mode)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := getWhatsappOnboardingStatus(pairingID)
		if err != nil {
			t.Fatal(err)
		}
		if status.GetOr("status", nil) == "waiting" && status.GetOr("qr_payload", nil) == "scan-me" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("qr event never reached the pairing: %v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.gatewayCanAcquire() {
		t.Fatal("gateway took the session during pairing")
	}
	if ok := cancelWhatsappOnboarding(pairingID).GetOr("ok", nil); ok != true {
		t.Fatalf("cancel ok %v", ok)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("cancel left the pairing transport running")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pairing worker never finished after cancel")
	}
	whatsappOnboardingMu.Lock()
	listed := whatsappOnboardingSessions.get(pairingID)
	whatsappOnboardingMu.Unlock()
	if listed != nil {
		t.Fatalf("cancelled pairing still listed with status %s", listed.status)
	}
	if !f.gatewayCanAcquire() {
		t.Fatal("cancelled pairing kept the session reserved")
	}
	if !f.timers[0].cancelled {
		t.Fatal("expiry timer still armed")
	}
}
