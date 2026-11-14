package backend

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func TestProcessStartMarkerResolvesARealMarkerForTheCurrentProcess(t *testing.T) {
	m, err := ProcessStartMarker(os.Getpid())
	if err != nil || m == "" || IsPidOnlyStartMarker(m) {
		t.Fatal(m, err)
	}
}

func TestAMissingPIDIsClassifiedAsESRCHSoReapOrphansCanDropTheRecord(t *testing.T) {
	_, err := ProcessStartMarker(0x7ffffff0)
	if !errors.Is(err, ErrNoProcess) {
		t.Fatal(err)
	}
}

func TestIsPidOnlyStartMarkerRecognizesDegradedPIDOnlyOwnershipRecords(t *testing.T) {
	if !IsPidOnlyStartMarker("pid-only:123") || IsPidOnlyStartMarker("linux:1") || IsPidOnlyStartMarker(42) || IsPidOnlyStartMarker(nil) {
		t.Fatal("pid only markers")
	}
}

type entry struct{ id int }

func TestALateDialFailureForgetsOnlyItsOwnEntryNeverANewerDescriptorInstalledUnderTheSameKey(t *testing.T) {
	var mu sync.Mutex
	stale, fresh := &entry{1}, &entry{2}
	cache := map[string]*entry{"research": fresh}
	ForgetFailedDescriptor(&mu, cache, "research", stale)
	if cache["research"] != fresh {
		t.Fatal("newer descriptor forgotten")
	}
	ForgetFailedDescriptor(&mu, cache, "research", fresh)
	if _, ok := cache["research"]; ok {
		t.Fatal("own entry kept")
	}
}

func TestADialWhoseEntryWasForgottenMidFlightRefusesToHandBackADescriptor(t *testing.T) {
	var mu sync.Mutex
	e := &entry{1}
	cache := map[string]*entry{"research": e}
	if err := AssertDescriptorStillOwned(&mu, cache, "research", e); err != nil {
		t.Fatal(err)
	}
	delete(cache, "research")
	if err := AssertDescriptorStillOwned(&mu, cache, "research", e); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal(err)
	}
	cache["research"] = &entry{2}
	if err := AssertDescriptorStillOwned(&mu, cache, "research", e); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatal(err)
	}
}

type fakeChild struct {
	pid     int
	exited  atomic.Bool
	killed  bool
	signals []string
	exitOn  string
}

func (c *fakeChild) Pid() int     { return c.pid }
func (c *fakeChild) Killed() bool { return c.killed }
func (c *fakeChild) Exited() bool { return c.exited.Load() }
func (c *fakeChild) Kill(s string) error {
	c.signals = append(c.signals, s)
	c.killed = true
	if s == c.exitOn {
		c.exited.Store(true)
	}
	return nil
}
func (c *fakeChild) WaitExit(d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if c.exited.Load() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return c.exited.Load()
}

func TestBackendExitEscalationRejectsWithinItsBoundWhenNoExitOrCloseArrives(t *testing.T) {
	c := &fakeChild{pid: 4242}
	start := time.Now()
	err := WaitForBackendExit(c, StopDeps{KillGroup: func(int, string) error { return nil }}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not exit after SIGKILL") || time.Since(start) > 3*time.Second {
		t.Fatal(err)
	}
}

func TestAFailedEscalationIsNotEvidenceThatTheChildExited(t *testing.T) {
	c := &fakeChild{pid: 4242}
	err := WaitForBackendExit(c, StopDeps{IsWindows: true, ForceKillProcessTree: func(int) {}}, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "retaining ownership") {
		t.Fatal(err)
	}
	group := &fakeChild{pid: 77, exitOn: "SIGKILL"}
	if err := WaitForBackendExit(group, StopDeps{KillGroup: func(int, string) error { return errors.New("ESRCH") }}, 10*time.Millisecond); err != nil || !reflect.DeepEqual(group.signals, []string{"SIGKILL"}) {
		t.Fatal(err, group.signals)
	}
}

func TestDesktopBackendSpawnEnvKeepsTheBaseEnvAndStampsStayAliveLast(t *testing.T) {
	env := DesktopBackendSpawnEnv(map[string]string{"PATH": "/bin", "GATEWAY_ON_ALL_ADAPTERS_DOWN": "exit"})
	if !reflect.DeepEqual(env, map[string]string{"PATH": "/bin", "GATEWAY_ON_ALL_ADAPTERS_DOWN": "stay_alive"}) {
		t.Fatal(env)
	}
}

type runtimeT struct{ name string }

func gate(env map[string]string) (*InstalledRuntimeGate[runtimeT], *[]string) {
	var log []string
	return NewInstalledRuntimeGate[runtimeT](func(k string) string { return env[k] }, func(l string) { log = append(log, l) }), &log
}

func TestInstalledRuntimeGateUsesTheInstalledRuntimeWithoutTheFlag(t *testing.T) {
	g, _ := gate(nil)
	active := &runtimeT{"active"}
	calls := 0
	got, _ := g.Resolve("/root", func() (*runtimeT, error) { calls++; return active, nil })
	if got != active || calls != 1 {
		t.Fatal(got, calls)
	}
}

func TestInstalledRuntimeGateSkipsTheInstalledRuntimeWithoutProbingItWhenIgnoreExistingIs1(t *testing.T) {
	g, log := gate(map[string]string{"ICEBATH_DESKTOP_IGNORE_EXISTING": "1"})
	calls := 0
	got, _ := g.Resolve("/root", func() (*runtimeT, error) { calls++; return &runtimeT{}, nil })
	if got != nil || calls != 0 || len(*log) != 1 || !strings.Contains((*log)[0], "/root") {
		t.Fatal(got, calls, *log)
	}
}

func TestInstalledRuntimeGateHonoursOnlyTheValueTheCLIExports(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes", " 1"} {
		g, _ := gate(map[string]string{"ICEBATH_DESKTOP_IGNORE_EXISTING": v})
		active := &runtimeT{}
		if got, _ := g.Resolve("/root", func() (*runtimeT, error) { return active, nil }); got != active {
			t.Fatal(v)
		}
	}
}

func TestInstalledRuntimeGateUsesTheRuntimeThisLaunchInstalledSoThePostInstallReResolveDoesNotReinstall(t *testing.T) {
	g, _ := gate(map[string]string{"ICEBATH_DESKTOP_IGNORE_EXISTING": "1"})
	active := &runtimeT{"active"}
	calls := 0
	probe := func() (*runtimeT, error) { calls++; return active, nil }
	resolve := func() (*runtimeT, error) { return g.Resolve("/root", probe) }
	if got, _ := resolve(); got != nil || calls != 0 {
		t.Fatal("resolved before install")
	}
	if got, _ := g.AfterInstall(resolve); got != active {
		t.Fatal("after install")
	}
	if got, _ := resolve(); got != active || calls != 2 {
		t.Fatal(calls)
	}
}

func TestInstalledRuntimeGateReadsTheFlagAtResolveTime(t *testing.T) {
	env := map[string]string{}
	g, _ := gate(env)
	env["ICEBATH_DESKTOP_IGNORE_EXISTING"] = "1"
	if got, _ := g.Resolve("/root", func() (*runtimeT, error) { return &runtimeT{}, nil }); got != nil {
		t.Fatal("flag read at construction")
	}
}

func TestRecycleOwnedBackendTargetTreatsAnEmptyOrMatchingProfileAsThePrimaryBackend(t *testing.T) {
	for _, p := range []string{"", "default"} {
		if RecycleOwnedBackendTarget(p, "default") != RecyclePrimary {
			t.Fatal(p)
		}
	}
}

func TestRecycleOwnedBackendTargetTreatsAnyOtherNamedProfileAsAPooledBackend(t *testing.T) {
	if RecycleOwnedBackendTarget("paid-ads", "default") != RecyclePool {
		t.Fatal("pool")
	}
}

func recycleDeps(events *[]string, profile string, sshDelay time.Duration) RecycleDeps {
	var mu sync.Mutex
	add := func(e string) { mu.Lock(); *events = append(*events, e); mu.Unlock() }
	return RecycleDeps{
		NotifyApplied:       func() { add("applied") },
		PrimaryProfile:      "default",
		Profile:             profile,
		RestartLocalGateway: func(p string) error { add("restart:" + p); return nil },
		TeardownPool:        func(p string) error { add("pool:" + p); return nil },
		TeardownPrimary:     func() error { add("primary"); return nil },
		TeardownSSH: func(p string) error {
			if sshDelay > 0 {
				add("ssh-start")
				time.Sleep(sshDelay)
				add("ssh-done")
				return nil
			}
			add("ssh:" + p)
			return nil
		},
	}
}

func TestRecycleOwnedBackendKillsTheOwnedSSHServeBeforeThePrimaryChildThenNotifiesApply(t *testing.T) {
	var events []string
	target, _ := RecycleOwnedBackend(recycleDeps(&events, "", 0))
	if target != RecyclePrimary || !reflect.DeepEqual(events, []string{"ssh:", "restart:default", "primary", "applied"}) {
		t.Fatal(target, events)
	}
}

func TestRecycleOwnedBackendRecyclesAPooledProfileWithoutTearingDownThePrimary(t *testing.T) {
	var events []string
	target, _ := RecycleOwnedBackend(recycleDeps(&events, "paid-ads", 0))
	if target != RecyclePool || !reflect.DeepEqual(events, []string{"ssh:paid-ads", "restart:paid-ads", "pool:paid-ads"}) {
		t.Fatal(target, events)
	}
}

func TestRecycleOwnedBackendAwaitsSSHTeardownBeforeTheLocalChildEvenWhenSSHIsSlow(t *testing.T) {
	var events []string
	d := recycleDeps(&events, "", 30*time.Millisecond)
	d.RestartLocalGateway = func(string) error { return nil }
	RecycleOwnedBackend(d)
	if !reflect.DeepEqual(events, []string{"ssh-start", "ssh-done", "primary", "applied"}) {
		t.Fatal(events)
	}
}

func TestBackendDialClaimsCoalescesTwoConcurrentDialsForTheSameScopeOntoOneBackendSpawn(t *testing.T) {
	var d DialClaims
	var spawns atomic.Int32
	release := make(chan struct{})
	type desc struct{ BaseURL string }
	dial := func() (*desc, error) { spawns.Add(1); <-release; return &desc{"http://127.0.0.1:53150"}, nil }
	var a, b *desc
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a, _ = RunDial(&d, "default", dial) }()
	time.Sleep(20 * time.Millisecond)
	go func() { defer wg.Done(); b, _ = RunDial(&d, "default", dial) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if spawns.Load() != 1 || a != b || a.BaseURL != "http://127.0.0.1:53150" {
		t.Fatal(spawns.Load(), a, b)
	}
}

func TestBackendDialClaimsScopesClaimsByKeyDifferentPairsDialIndependently(t *testing.T) {
	var d DialClaims
	a, _ := RunDial(&d, BackendScopeKey("", "default"), func() (string, error) { return "a", nil })
	b, _ := RunDial(&d, BackendScopeKey("office-ssh", "default"), func() (string, error) { return "b", nil })
	if a != "a" || b != "b" {
		t.Fatal(a, b)
	}
}

func TestBackendDialClaimsReleasesTheClaimOnceTheDialSettlesSoALaterReconnectCanDialAgain(t *testing.T) {
	var d DialClaims
	calls := 0
	dial := func() (int, error) { calls++; return calls, nil }
	RunDial(&d, "default", dial)
	if d.InFlight("default") {
		t.Fatal("claim latched")
	}
	RunDial(&d, "default", dial)
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestBackendDialClaimsPropagatesAFailedDialToEveryCoalescedWaiterAndNeverCachesTheRejection(t *testing.T) {
	var d DialClaims
	var calls atomic.Int32
	release := make(chan struct{})
	failing := func() (string, error) { calls.Add(1); <-release; return "", errors.New("ssh dial failed") }
	key := "conn:office-ssh::default"
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = RunDial(&d, key, failing) }(i)
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 || errs[0] == nil || errs[1] == nil || errs[0].Error() != "ssh dial failed" {
		t.Fatal(calls.Load(), errs)
	}
	if v, err := RunDial(&d, key, func() (string, error) { return "recovered", nil }); v != "recovered" || err != nil {
		t.Fatal(v, err)
	}
}

func TestBackendDialClaimsASynchronouslyThrowingDialRejectsTheClaimInsteadOfEscapingTheCoalescingSeam(t *testing.T) {
	var d DialClaims
	_, err := RunDial(&d, "default", func() (string, error) { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") || d.InFlight("default") {
		t.Fatal(err)
	}
}

func TestParseBackendScopeKeyRoundTripsTheCompositePoolKeyBackToConnectionAndProfile(t *testing.T) {
	for _, c := range []struct{ key, conn, profile string }{{"conn:office-ssh::default", "office-ssh", "default"}, {"conn:office-ssh::work", "office-ssh", "work"}} {
		r := ParseBackendScopeKey(c.key)
		if r.ConnectionID == nil || *r.ConnectionID != c.conn || r.Profile != c.profile || BackendScopeKey(c.conn, c.profile) != c.key {
			t.Fatal(c)
		}
	}
}

func TestParseBackendScopeKeyTreatsABareProfileKeyAsTheLocalPrimaryScope(t *testing.T) {
	for _, p := range []string{"default", "work"} {
		if r := ParseBackendScopeKey(p); r.ConnectionID != nil || r.Profile != p {
			t.Fatal(r)
		}
	}
}

func TestResolvedWindowRoutesShareOneDialClaimWithoutAbsorbingASameNamedSource(t *testing.T) {
	for _, conn := range []string{"", "office-ssh"} {
		var d DialClaims
		key := BackendScopeKey(conn, "work")
		var calls, others atomic.Int32
		release := make(chan struct{})
		dial := func() (*string, error) { calls.Add(1); <-release; s := "http://localhost:53150"; return &s, nil }
		other := func() (*string, error) { others.Add(1); s := "http://localhost:53151"; return &s, nil }
		res := make([]*string, 3)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); res[0], _ = RunDial(&d, key, dial) }()
		time.Sleep(10 * time.Millisecond)
		go func() { defer wg.Done(); res[1], _ = RunDial(&d, key, dial) }()
		res[2], _ = RunDial(&d, BackendScopeKey("another-source", "work"), other)
		time.Sleep(10 * time.Millisecond)
		close(release)
		wg.Wait()
		if res[0] != res[1] || res[0] == res[2] || calls.Load() != 1 || others.Load() != 1 {
			t.Fatal(conn)
		}
	}
}

type proc struct{ id int }

func TestAnInvalidatedRemoteAttemptCannotPublishALateDescriptor(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	s.Invalidate()
	if s.SetPromise(a, NewFuture(func() (string, error) { return "late", nil })) || s.Promise() != nil {
		t.Fatal("late descriptor published")
	}
}

func TestAStaleBackendExitCannotClearANewerConnectionAttempt(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	old := s.AttachProcess(a, &proc{1})
	s.Invalidate()
	b, _ := s.StartAttempt()
	s.AttachProcess(b, &proc{2})
	f := NewFuture(func() (string, error) { return "new", nil })
	s.SetPromise(b, f)
	if s.ClearForCurrentProcess(old) {
		t.Fatal("stale exit cleared the new attempt")
	}
	if p, _ := s.Process(); p.id != 2 || s.Promise() != f {
		t.Fatal("new attempt lost")
	}
}

func TestTheCurrentBackendExitClearsItsProcessAndConnectionPromise(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	owner := s.AttachProcess(a, &proc{1})
	s.SetPromise(a, NewFuture(func() (string, error) { return "c", nil }))
	if !s.ClearForCurrentProcess(owner) || s.Promise() != nil {
		t.Fatal("not cleared")
	}
	if _, had := s.Process(); had {
		t.Fatal("process kept")
	}
}

func TestAStaleRejectedAttemptCannotClearANewerConnectionPromise(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	s.SetPromise(a, NewFuture(func() (string, error) { return "", errors.New("x") }))
	s.Invalidate()
	b, _ := s.StartAttempt()
	f := NewFuture(func() (string, error) { return "b", nil })
	s.SetPromise(b, f)
	if s.ClearPromiseForAttempt(a) || s.Promise() != f {
		t.Fatal("stale rejection cleared the newer promise")
	}
}

func TestAnInvalidatedAttemptCannotAttachALateSpawnedProcess(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	s.Invalidate()
	if s.AttachProcess(a, &proc{1}) != nil {
		t.Fatal("late attach")
	}
}

func TestAFailedPrimaryStopRetainsItsChildAndBlocksAReplacementUntilRetryExits(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	child := &proc{1}
	s.AttachProcess(a, child)
	if err := s.StopProcess(func(*proc) error { return errors.New("did not exit") }); err == nil {
		t.Fatal("stop error swallowed")
	}
	if _, err := s.StartAttempt(); err == nil {
		t.Fatal("replacement allowed while the child lives")
	}
	var retried *proc
	if err := s.StopProcess(func(p *proc) error { retried = p; return nil }); err != nil || retried != child {
		t.Fatal(err, retried)
	}
	if _, err := s.StartAttempt(); err != nil {
		t.Fatal(err)
	}
}

func TestDistinguishesAPendingConnectionAttemptFromACachedSettledDescriptor(t *testing.T) {
	var s ConnectionState[*proc, string]
	a, _ := s.StartAttempt()
	release := make(chan struct{})
	f := NewFuture(func() (string, error) { <-release; return "d", nil })
	s.SetPromise(a, f)
	if s.PendingPromise() != f || s.Promise() != f {
		t.Fatal("pending")
	}
	close(release)
	f.Wait()
	deadline := time.Now().Add(time.Second)
	for s.PendingPromise() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.PendingPromise() != nil || s.Promise() != f {
		t.Fatal("settled descriptor still pending")
	}
}

func TestLatchesALocalBackendFailureSoTheInstallRetryLoopIsBroken(t *testing.T) {
	if !ShouldLatchBackendStartFailure(BackendStartFailureContext{}) {
		t.Fatal("local not latched")
	}
}

func TestNeverLatchesARemoteFailureSoRecoveryStaysRetryableWithoutARestart(t *testing.T) {
	if ShouldLatchBackendStartFailure(BackendStartFailureContext{AttemptedRemote: true}) {
		t.Fatal("remote latched")
	}
}

func TestNeverLatchesASupervisorOwnedRespawnFailure(t *testing.T) {
	if ShouldLatchBackendStartFailure(BackendStartFailureContext{SupervisorRecovery: true}) {
		t.Fatal("supervisor latched")
	}
}

func TestLatchesAConfirmedRemoteReauthFailureSoTheOverlayStaysClickable(t *testing.T) {
	if !ShouldLatchRemoteReauthFailure(RemoteBootRetryContext{AttemptedRemote: true, IsReauth: true}) || ShouldLatchRemoteReauthFailure(RemoteBootRetryContext{AttemptedRemote: true}) || ShouldLatchRemoteReauthFailure(RemoteBootRetryContext{IsReauth: true}) {
		t.Fatal("reauth latch")
	}
}

func TestEveryRemoteFailurePicksExactlyOnePathRetryReauthLatchOrHostKeyLatch(t *testing.T) {
	for _, c := range []RemoteBootRetryContext{
		{AttemptedRemote: true}, {AttemptedRemote: true, IsReauth: true}, {AttemptedRemote: true, IsHostKeyChanged: true},
		{AttemptedRemote: true, IsSSHAuthFailed: true}, {AttemptedRemote: true, IsSSHClientFailed: true},
	} {
		n := 0
		for _, b := range []bool{IsRetryableRemoteBootFailure(c), ShouldLatchRemoteReauthFailure(c), ShouldLatchHostKeyChangedFailure(c), ShouldLatchSSHAuthFailure(c), ShouldLatchSSHClientFailure(c)} {
			if b {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%+v picked %d paths", c, n)
		}
	}
	if IsRetryableRemoteBootFailure(RemoteBootRetryContext{}) {
		t.Fatal("local failure retried")
	}
}

func TestHostKeyChangeClassifiedFromTheKindTagAndFromStringifiedSSHBanners(t *testing.T) {
	for _, err := range []error{&KindError{Kind: "host-key-changed"}, errors.New("@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @"), errors.New("Host key verification failed."), errors.New("The ED25519 host key for box has CHANGED")} {
		if !IsHostKeyChangedBootFailure(err) {
			t.Fatal(err)
		}
	}
	if IsHostKeyChangedBootFailure(errors.New("connection reset")) {
		t.Fatal("ordinary fault")
	}
}

func TestARejectedSSHKeyLatchesTheBootFailureAndIsNeverAutoRetried(t *testing.T) {
	for _, err := range []error{&KindError{Kind: "auth-failed"}, &KindError{SSHError: "auth-failed"}, errors.New("SSH authentication to box failed"), errors.New("git@box: Permission denied (publickey).")} {
		if !IsSSHAuthFailedBootFailure(err) {
			t.Fatal(err)
		}
	}
	c := RemoteBootRetryContext{AttemptedRemote: true, IsSSHAuthFailed: true}
	if !ShouldLatchSSHAuthFailure(c) || IsRetryableRemoteBootFailure(c) {
		t.Fatal("auth latch")
	}
}

func TestWhileAReauthRejectionIsLatchedOnlyReEmitsOfThatFailureReachTheRenderer(t *testing.T) {
	same, other := "latched", "different"
	if ShouldHoldBootProgressForReauth("latched", &same) || !ShouldHoldBootProgressForReauth("latched", &other) || !ShouldHoldBootProgressForReauth("latched", nil) {
		t.Fatal("hold")
	}
	if ShouldHoldBootProgressForReauth("", &other) {
		t.Fatal("no latch held")
	}
}

func TestAFailedLocalSSHGProbeLatchesAndIsNeverAutoRetriedAndTheHintIsWindowsOnly(t *testing.T) {
	err := SSHClientFailedError("ssh", errors.New("bad option"), "win32")
	if !IsSSHClientFailedBootFailure(err) || !strings.Contains(err.Error(), "desktop.ssh_path") {
		t.Fatal(err)
	}
	if strings.Contains(SSHClientFailedError("ssh", errors.New("x"), "linux").Error(), "desktop.ssh_path") {
		t.Fatal("hint off Windows")
	}
	c := RemoteBootRetryContext{AttemptedRemote: true, IsSSHClientFailed: true}
	if !ShouldLatchSSHClientFailure(c) || IsRetryableRemoteBootFailure(c) {
		t.Fatal("client latch")
	}
}

func readyOpts(health func(string) error, status func(string) error) ReadyOptions {
	clock := time.Unix(0, 0)
	return ReadyOptions{
		ProbeHealth: func(_ context.Context, u string, _ time.Duration) error { return health(u) },
		FetchJSON:   func(_ context.Context, u, _ string) error { return status(u) },
		Timeout:     5 * time.Second, Poll: time.Second,
		Now:   func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil },
	}
}

func TestWaitForHermesReadyResolvesOnceHealthAnswers(t *testing.T) {
	n := 0
	err := WaitForHermesReady("http://127.0.0.1:9/", readyOpts(func(u string) error {
		n++
		if u != "http://127.0.0.1:9/api/health" {
			t.Fatal(u)
		}
		if n < 3 {
			return &ConnRefusedError{errors.New("ECONNREFUSED")}
		}
		return nil
	}, func(string) error { return errors.New("status") }))
	if err != nil || n != 3 {
		t.Fatal(err, n)
	}
}

func TestWaitForHermesReadyFallsBackToStatusWhenHealthIsMissing(t *testing.T) {
	var statusURL string
	err := WaitForHermesReady("http://h", readyOpts(func(string) error { return errors.New("404: Not Found") }, func(u string) error { statusURL = u; return nil }))
	if err != nil || statusURL != "http://h/api/status" {
		t.Fatal(err, statusURL)
	}
}

func TestWaitForHermesReadyMapsACredentialedAuthRejectionToReauth(t *testing.T) {
	o := readyOpts(func(string) error { return errors.New("401: Unauthorized") }, nil)
	o.ProbeIsCredentialed = true
	err := WaitForHermesReady("http://h", o)
	if !IsReauthRequiredError(err) || err.Error() != RemoteSessionExpiredMessage {
		t.Fatal(err)
	}
}

func TestWaitForHermesReadyFailsFastWhenTheAlreadyBoundPortRefuses(t *testing.T) {
	o := readyOpts(func(string) error { return &ConnRefusedError{errors.New("connect ECONNREFUSED")} }, nil)
	o.AlreadyBound = true
	if err := WaitForHermesReady("http://h", o); err == nil || err.Error() != "Icebath backend did not become ready: connect ECONNREFUSED" {
		t.Fatal(err)
	}
}

func TestWaitForHermesReadyExplainsANousCloudOutageAfterTheDeadline(t *testing.T) {
	err := WaitForHermesReady("https://box.agents.nousresearch.com", readyOpts(func(string) error { return &HTTPError{StatusCode: 503, Message: "503: Service Unavailable"} }, nil))
	var cloud *CloudBackendDownError
	if !errors.As(err, &cloud) || cloud.StatusCode != 503 || !strings.Contains(err.Error(), "Nous Cloud agent box.agents.nousresearch.com is down (HTTP 503") {
		t.Fatal(err)
	}
	if err := WaitForHermesReady("http://127.0.0.1:1", readyOpts(func(string) error { return errors.New("503: x") }, nil)); err == nil || err.Error() != "Icebath backend did not become ready: 503: x" {
		t.Fatal(err)
	}
}

func TestWaitForHermesReadyStopsWhenSuperseded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := readyOpts(func(string) error { return nil }, nil)
	o.Ctx = ctx
	if _, ok := WaitForHermesReady("http://h", o).(SupersededError); !ok {
		t.Fatal("not superseded")
	}
}

func TestIsServerSideHTTPErrorClassifiesStructuredAndPrefixedStatuses(t *testing.T) {
	if IsServerSideHTTPError(&HTTPError{StatusCode: 502, Message: "bad gateway"}) == nil || IsServerSideHTTPError(errors.New("504: timeout")) == nil || IsServerSideHTTPError(errors.New("500: boom")) != nil || IsServerSideHTTPError(nil) != nil {
		t.Fatal("classification")
	}
	if !IsGatedMissingHealthError(errors.New("401: no_cookie")) || IsGatedMissingHealthError(errors.New("401: expired")) {
		t.Fatal("gated")
	}
}

type memStore struct {
	contents    string
	writes      int
	quarantined bool
	fail        bool
}

func (m *memStore) Read() (string, error) { return m.contents, nil }
func (m *memStore) Write(c string) error {
	if m.fail {
		return errors.New("disk full")
	}
	m.writes++
	m.contents = c
	return nil
}
func (m *memStore) Quarantine() error { m.quarantined = true; return nil }

func boolp(b bool) *bool { return &b }

func id(pid int) OwnershipEntry {
	return OwnershipEntry{Nonce: "n", Pid: pid, Profile: "default", StartMarker: "linux:1"}
}

func TestBackendOwnershipClaimAndReleaseRoundTrip(t *testing.T) {
	store := &memStore{}
	o := NewOwnership(OwnershipDeps{Store: store, Stop: func(OwnershipEntry) error { return nil }})
	o.Claim(id(10))
	o.Claim(id(11))
	if got := ParseBackendOwnership(store.contents); len(got) != 2 || !strings.HasSuffix(store.contents, "\n") {
		t.Fatal(store.contents)
	}
	o.Release(id(10))
	if got := ParseBackendOwnership(store.contents); len(got) != 1 || got[0].Pid != 11 {
		t.Fatal(got)
	}
	writes := store.writes
	o.Release(id(99))
	if store.writes != writes {
		t.Fatal("release of an unknown identity wrote")
	}
}

func TestBackendOwnershipRefusesIncompleteIdentitiesAndStopsAChildItCannotRecord(t *testing.T) {
	stopped := 0
	store := &memStore{fail: true}
	o := NewOwnership(OwnershipDeps{Store: store, Stop: func(OwnershipEntry) error { stopped++; return nil }})
	if _, err := o.Claim(OwnershipEntry{Pid: 1}); err == nil {
		t.Fatal("incomplete identity owned")
	}
	if _, err := o.Claim(id(5)); err == nil || stopped != 1 {
		t.Fatal(err, stopped)
	}
}

func TestBackendOwnershipReapsOnlyOrphansWhoseIdentityStillMatches(t *testing.T) {
	store := &memStore{contents: SerializeBackendOwnership([]OwnershipEntry{id(1), id(2), id(3), id(4)})}
	var stopped []int
	o := NewOwnership(OwnershipDeps{
		Store:         store,
		MatchesParent: func(e OwnershipEntry) (*bool, error) { return boolp(e.Pid == 1), nil },
		MatchesIdentity: func(e OwnershipEntry) (*bool, error) {
			if e.Pid == 4 {
				return nil, nil
			}
			return boolp(e.Pid == 2), nil
		},
		Stop: func(e OwnershipEntry) error { stopped = append(stopped, e.Pid); return nil },
	})
	reaped := o.ReapOrphans()
	survivors := ParseBackendOwnership(store.contents)
	var pids []int
	for _, s := range survivors {
		pids = append(pids, s.Pid)
	}
	if !reflect.DeepEqual(reaped, []int{2}) || !reflect.DeepEqual(stopped, []int{2}) || !reflect.DeepEqual(pids, []int{1, 4}) {
		t.Fatal(reaped, stopped, pids)
	}
}

func TestBackendOwnershipQuarantinesACorruptFileWithoutReaping(t *testing.T) {
	store := &memStore{contents: "{not json"}
	o := NewOwnership(OwnershipDeps{Store: store})
	if got := o.ReapOrphans(); len(got) != 0 || !store.quarantined {
		t.Fatal(got)
	}
}

func TestParseBackendOwnershipAcceptsBothShapesAndDropsDuplicatesAndPartials(t *testing.T) {
	raw := `[{"nonce":"n","pid":1,"profile":"default","startMarker":"m","command":"icebath dashboard","parentPid":7},{"nonce":"n","pid":1,"profile":"default","startMarker":"m"},{"pid":2}]`
	got := ParseBackendOwnership(raw)
	if len(got) != 1 || got[0].ParentPid != 7 || got[0].Command == nil {
		t.Fatal(got)
	}
	if e, corrupt := ParseBackendOwnershipDetailed(""); corrupt || len(e) != 0 {
		t.Fatal("empty")
	}
	if len(ParseBackendOwnership(SerializeBackendOwnership(got))) != 1 {
		t.Fatal("round trip")
	}
}

func TestBackendCommandMatchesRecognizesServeAndDashboardCommandLines(t *testing.T) {
	for _, c := range []string{"/usr/bin/icebath dashboard --port 0", `"C:\icebath\icebath.exe" serve`, "icebath --profile work serve"} {
		if !BackendCommandMatches(c) {
			t.Fatal(c)
		}
	}
	for _, c := range []string{"icebath chat", "myicebath dashboard", ""} {
		if BackendCommandMatches(c) {
			t.Fatal(c)
		}
	}
}

func TestShutdownCoordinatorRunsTheTeardownOnce(t *testing.T) {
	n := 0
	c := NewShutdownCoordinator(func() error { n++; time.Sleep(10 * time.Millisecond); return nil })
	if c.HasStarted() || c.IsPending() {
		t.Fatal("started early")
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); c.Run() }()
	}
	wg.Wait()
	if n != 1 || !c.HasStarted() || c.IsPending() {
		t.Fatal(n)
	}
}

func TestReadyPortInMergedOutputIgnoresSentinelsGluedToAWord(t *testing.T) {
	if p, ok := ReadyPortInMergedOutput("log line\nICEBATH_DASHBOARD_READY port=8123\n"); !ok || p != 8123 {
		t.Fatal(p)
	}
	if _, ok := ReadyPortInMergedOutput("XICEBATH_BACKEND_READY port=1"); ok {
		t.Fatal("glued sentinel matched")
	}
	if p, ok := ReadyPortInMergedOutput("[ICEBATH_BACKEND_READY port=77]"); !ok || p != 77 {
		t.Fatal(p)
	}
}

func TestResolvePortAnnounceTimeoutHonoursTheFloor(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if ResolvePortAnnounceTimeout(env("")) != DefaultPortAnnounceTimeout || ResolvePortAnnounceTimeout(env("1000")) != MinPortAnnounceTimeout || ResolvePortAnnounceTimeout(env("120000")) != 120*time.Second || ResolvePortAnnounceTimeout(env("-5")) != DefaultPortAnnounceTimeout {
		t.Fatal("timeouts")
	}
}

func TestWaitForDashboardPortReadsTheSentinelLine(t *testing.T) {
	r := strings.NewReader("booting\nICEBATH_BACKEND_READY port=4040\n")
	port, err := WaitForDashboardPort(AnnounceSource{Stdout: r, Exited: make(chan string)}, time.Second, nil)
	if err != nil || port != 4040 {
		t.Fatal(port, err)
	}
}

func TestWaitForDashboardPortReportsAnExitBeforeTheAnnouncement(t *testing.T) {
	exited := make(chan string, 1)
	exited <- "1"
	pr, pw := ioPipe()
	defer pw.Close()
	_, err := WaitForDashboardPort(AnnounceSource{Stdout: pr, Exited: exited, OutputTail: func() string { return "\ntail" }}, time.Second, nil)
	if err == nil || err.Error() != "Icebath backend: exited before port announcement (1)\ntail" {
		t.Fatal(err)
	}
}

func TestWaitForDashboardPortTimesOutAndUsesBufferedOutput(t *testing.T) {
	pr, pw := ioPipe()
	defer pw.Close()
	_, err := WaitForDashboardPort(AnnounceSource{Stdout: pr, Exited: make(chan string)}, 20*time.Millisecond, nil)
	if err == nil || err.Error() != "Timed out waiting for Icebath backend port announcement (20ms)" {
		t.Fatal(err)
	}
	port, err := WaitForDashboardPort(AnnounceSource{Stdout: pr, Exited: make(chan string), Buffered: func() string { return "x ICEBATH_DASHBOARD_READY port=99" }}, time.Second, nil)
	if err != nil || port != 99 {
		t.Fatal(port, err)
	}
}

func TestReadDashboardReadyFileAcceptsOnlyPositiveIntegerPorts(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]int{`{"port":5050}`: 5050, `{"port":0}`: 0, `{"port":1.5}`: 0, `nope`: 0} {
		os.WriteFile(dir+"/ready.json", []byte(body), 0o644)
		if got := ReadDashboardReadyFile(dir + "/ready.json"); got != want {
			t.Fatal(body, got)
		}
	}
	if ReadDashboardReadyFile("") != 0 {
		t.Fatal("empty path")
	}
}

func TestBackendEnvScrubsPythonpathAndPythonhome(t *testing.T) {
	env := BuildDesktopBackendEnv(map[string]string{"PATH": "/usr/bin", "PYTHONPATH": "/x", "PYTHONHOME": "/y"}, "linux", HomeInputs{Home: "/home/u", Env: map[string]string{}, Platform: "linux"})
	if env["PYTHONPATH"] != "" || env["PYTHONHOME"] != "" {
		t.Fatal(env)
	}
}

func TestPosixBackendPATHKeepsTheInheritedPATHFirstAndAppendsMissingSaneEntries(t *testing.T) {
	env := BuildDesktopBackendEnv(map[string]string{"PATH": "/custom/bin:/usr/bin"}, "linux", HomeInputs{Home: "/home/u", Env: map[string]string{}, Platform: "linux"})
	want := "/custom/bin:/usr/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"
	if env["PATH"] != want {
		t.Fatal(env["PATH"])
	}
}

func TestBackendRunsTheStoreToolchainEvenAfterTheLoginShellPATHIsMergedInFrontOfIt(t *testing.T) {
	cur := map[string]string{"PATH": "/usr/local/bin:/home/u/.icebath/tools/node/bin:/usr/bin"}
	got := StoreFirstPath(cur["PATH"], cur, "linux", HomeInputs{Home: "/home/u", Env: map[string]string{}, Platform: "linux"})
	if got != "/home/u/.icebath/tools/node/bin:/usr/local/bin:/usr/bin" {
		t.Fatal(got)
	}
}

func TestIcebathRuntimeDirNamesTheStoreLookAlikePrefixesAreNotIcebathOwned(t *testing.T) {
	cur := map[string]string{"ICEBATH_RUNTIME_DIR": "/opt/rt"}
	got := StoreFirstPath("/usr/bin:/opt/rt-other/bin:/opt/rt/bin", cur, "linux", HomeInputs{Home: "/home/u", Env: map[string]string{}, Platform: "linux"})
	if got != "/opt/rt/bin:/usr/bin:/opt/rt-other/bin" {
		t.Fatal(got)
	}
}

func TestWindowsPATHCasingAndDelimiterArePreservedWithoutPosixSaneEntries(t *testing.T) {
	env := BuildDesktopBackendEnv(map[string]string{"Path": `C:\Windows;C:\Tools`}, "win32", HomeInputs{Home: `C:\Users\u`, Env: map[string]string{}, Platform: "win32", DirectoryExists: func(string) bool { return false }, ReadWindowsHome: func() string { return "" }})
	if env["Path"] != `C:\Windows;C:\Tools` {
		t.Fatal(env)
	}
	if _, ok := env["PATH"]; ok {
		t.Fatal("upper PATH added")
	}
}

func TestBuildDesktopBackendEnvForcesPYTHONUTF8UnlessTheUserSetItExplicitly(t *testing.T) {
	h := HomeInputs{Home: "/h", Env: map[string]string{}, Platform: "linux"}
	if BuildDesktopBackendEnv(map[string]string{}, "linux", h)["PYTHONUTF8"] != "1" || BuildDesktopBackendEnv(map[string]string{"PYTHONUTF8": "0"}, "linux", h)["PYTHONUTF8"] != "0" {
		t.Fatal("utf8")
	}
}

func TestNormalizeHermesHomeRootExpandsALiteralLeadingTildeAgainstTheHomeDirectoryNotCwd(t *testing.T) {
	if got := NormalizeHermesHomeRoot("~/.icebath", "linux", "/home/u"); got != "/home/u/.icebath" {
		t.Fatal(got)
	}
}

func TestNormalizeHermesHomeRootMapsProfileHomesBackToTheGlobalIcebathRoot(t *testing.T) {
	if got := NormalizeHermesHomeRoot("/home/u/.icebath/profiles/work", "linux", "/home/u"); got != "/home/u/.icebath" {
		t.Fatal(got)
	}
	if got := NormalizeHermesHomeRoot(`C:\Users\u\icebath\Profiles\work`, "win32", `C:\Users\u`); got != `C:\Users\u\icebath` {
		t.Fatal(got)
	}
}

func TestPathEnvKeyFindsThePlatformCasedPATHKey(t *testing.T) {
	if PathEnvKey(map[string]string{"Path": "x"}, "win32") != "Path" || PathEnvKey(map[string]string{"Path": "x"}, "linux") != "PATH" || PathEnvKey(map[string]string{}, "win32") != "PATH" {
		t.Fatal("keys")
	}
}

func TestAppendUniquePathEntriesFlattensDedupesAndPreservesFirstOccurrence(t *testing.T) {
	if got := AppendUniquePathEntries([][]string{{"/a", "", "/b"}, {"/b", "/c", "/a"}}, ":"); got != "/a:/b:/c" {
		t.Fatal(got)
	}
}

func dotenvReader(files map[string]string) FileReader {
	return func(n string) string { return files[n] }
}

func TestANamedProfileBackendDoesNotInheritSecretsTheRootDotenvLoadedIntoDesktop(t *testing.T) {
	files := map[string]string{"/h/.env": "OPENAI_API_KEY=sk\nexport HOME=/x\n", "/h/.op.env": "\uFEFFOP_TOKEN=1"}
	env := ProfileBackendParentEnv("/h", "work", map[string]string{"OPENAI_API_KEY": "sk", "OP_TOKEN": "1", "HOME": "/home/u", "KEEP": "1"}, "linux", dotenvReader(files))
	if !reflect.DeepEqual(env, map[string]string{"HOME": "/home/u", "KEEP": "1"}) {
		t.Fatal(env)
	}
}

func TestTheLaunchProfileBackendInheritsTheDesktopEnvUnchanged(t *testing.T) {
	cur := map[string]string{"OPENAI_API_KEY": "sk"}
	env := ProfileBackendParentEnv("/h", "default", cur, "linux", dotenvReader(map[string]string{"/h/.env": "OPENAI_API_KEY=sk"}))
	if !reflect.DeepEqual(env, cur) {
		t.Fatal(env)
	}
}

func TestAPrimaryBackendWithoutAnExplicitProfileFollowsTheStickyActiveProfile(t *testing.T) {
	files := map[string]string{"/h/active_profile": "work\n", "/h/.env": "SECRET=1"}
	env := ProfileBackendParentEnv("/h", "", map[string]string{"SECRET": "1"}, "linux", dotenvReader(files))
	if _, ok := env["SECRET"]; ok {
		t.Fatal("root secret leaked into the active profile backend")
	}
}

func TestWindowsMatchesProfileHomesAndDotenvNamesCaseInsensitively(t *testing.T) {
	files := map[string]string{`C:\h\.env`: "secret=1"}
	env := ProfileBackendParentEnv(`C:\h`, "work", map[string]string{"SECRET": "1", "Path": "x"}, "win32", dotenvReader(files))
	if !reflect.DeepEqual(env, map[string]string{"Path": "x"}) {
		t.Fatal(env)
	}
}

func TestParseSpawnLedgerKeepsAttachableLoopbackBackends(t *testing.T) {
	raw := `[{"pid":10,"port":9119,"purpose":"dashboard","host":"127.0.0.1","profile":"default","create_time":1.5,"registered_at":2},{"pid":11,"port":9120,"purpose":"serve","host":"0.0.0.0","isolated":true},{"pid":12,"port":70000,"purpose":"serve","host":""},{"pid":13,"port":9121,"purpose":"tool","host":""},{"pid":14,"port":9122,"purpose":"serve","host":"10.0.0.2"}]`
	got := ParseSpawnLedger(raw)
	if len(got) != 1 || got[0].Pid != 10 || *got[0].CreateTime != 1.5 || got[0].RegisteredAt != 2 {
		t.Fatal(got)
	}
	if len(ParseSpawnLedger("nope")) != 0 || len(ParseSpawnLedger(`{"pid":1}`)) != 0 {
		t.Fatal("garbage parsed")
	}
}

func TestWaitForBackendReleaseUnlocksOnceShimAndSignalledPIDsAreGone(t *testing.T) {
	clock := time.Unix(0, 0)
	alive := map[int]bool{5: true}
	var killed []int
	locked := true
	polls := 0
	res := WaitForBackendRelease([]int{5, 0}, ReleaseGateDeps{
		IsShimLocked:         func() bool { return locked },
		IsPidAlive:           func(p int) bool { return alive[p] },
		CollectStragglerPids: func() []int { return []int{6} },
		KillProcessTree:      func(p int) { killed = append(killed, p) },
		Sleep: func(d time.Duration) {
			clock = clock.Add(d)
			polls++
			if polls == 2 {
				locked, alive[5] = false, false
			}
		},
		Now: func() time.Time { return clock },
		Log: func(string) {},
	}, "update", ReleaseGateDeadline)
	if !res.Unlocked || len(res.LingeringPids) != 0 || !reflect.DeepEqual(killed, []int{6, 6}) {
		t.Fatal(res, killed)
	}
}

func TestWaitForBackendReleaseReportsLockedAfterTheDeadline(t *testing.T) {
	clock := time.Unix(0, 0)
	res := WaitForBackendRelease([]int{9}, ReleaseGateDeps{
		IsShimLocked: func() bool { return true }, IsPidAlive: func(int) bool { return true },
		CollectStragglerPids: func() []int { return nil }, KillProcessTree: func(int) {},
		Sleep: func(d time.Duration) { clock = clock.Add(d) }, Now: func() time.Time { return clock }, Log: func(string) {},
	}, "update", time.Second)
	if res.Unlocked || !reflect.DeepEqual(res.LingeringPids, []int{9}) {
		t.Fatal(res)
	}
}

func TestResolveProbeTimeoutCapsAndDefaults(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if ResolveProbeTimeout(env("")) != DefaultProbeTimeout || ResolveProbeTimeout(env("abc")) != DefaultProbeTimeout || ResolveProbeTimeout(env("0")) != DefaultProbeTimeout || ResolveProbeTimeout(env("500")) != 500*time.Millisecond || ResolveProbeTimeout(env("999999")) != 120*time.Second {
		t.Fatal("probe timeouts")
	}
}

func TestShouldTrustHermesOverrideAndTimeoutClassification(t *testing.T) {
	if ShouldTrustHermesOverride("  ") || !ShouldTrustHermesOverride("/x/icebath") {
		t.Fatal("override")
	}
	if !IsTimeoutError(&ProbeError{Killed: true}) || !IsTimeoutError(&ProbeError{Signal: "SIGTERM"}) || IsTimeoutError(&ProbeError{Code: 1}) || IsTimeoutError(errors.New("x")) {
		t.Fatal("timeouts")
	}
	if VerifyHermesCli("", time.Second) {
		t.Fatal("empty command verified")
	}
}

func TestRunBackendStartStepRejectsWithTheAbortReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("superseded")
	release := make(chan struct{})
	go func() { time.Sleep(10 * time.Millisecond); cancel(cause) }()
	_, err := RunBackendStartStep(ctx, func() (int, error) { <-release; return 1, nil })
	close(release)
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if v, err := RunBackendStartStep(context.Background(), func() (int, error) { return 7, nil }); v != 7 || err != nil {
		t.Fatal(v, err)
	}
}

func ioPipe() (*os.File, *os.File) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	return r, w
}
