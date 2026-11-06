package tools

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var ptyBg = context.Background()

func TestRingbufferKeepsEverythingUnderCapacity(t *testing.T) {
	rb := NewRingBuffer(100)
	rb.Append([]byte("hello"))
	rb.Append([]byte(" world"))
	if got := string(rb.Snapshot()); got != "hello world" || rb.Truncated() {
		t.Fatalf("snapshot %q truncated %v", got, rb.Truncated())
	}
}

func TestRingbufferDropsOldestOverCapacity(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("abcdef"))
	rb.Append([]byte("ghijkl"))
	if got := string(rb.Snapshot()); got != "efghijkl" || !rb.Truncated() {
		t.Fatalf("snapshot %q truncated %v", got, rb.Truncated())
	}
}

func TestAttachReplaysBufferThenStreamsLive(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("hello "), []byte("world"), nil), 1024)
	s.Start()
	ptyWaitUntil(t, "eof", func() bool { return !s.Alive() })
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	if got := ws.bytesSent(); got != "hello world" {
		t.Fatalf("replay %q", got)
	}
	s.Close(ptyBg)
}

func TestFailedReplayDetachesSessionSoReaperReclaimsIt(t *testing.T) {
	reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 0, MaxSessions: 2, BufferCap: 1024, ReadTimeout: 10 * time.Millisecond})
	bridge := newFakePtyBridge([]byte{})
	s, _, err := reg.AttachOrSpawn(ptyBg, "k", spawnBridge(bridge), "")
	if err != nil {
		t.Fatal(err)
	}
	s.Buffer.Append([]byte("replay"))
	if ok, _ := s.Attach(ptyBg, newFailingPtyViewer(false), PtyAttachOptions{}); ok {
		t.Fatal("attach over a dropped client succeeded")
	}
	if _, has := s.LastDetachedAt(); s.Attached() || !has {
		t.Fatal("failed replay left the session attached")
	}
	reg.ReapIdle(ptyBg, ptyMonotonic().Add(time.Second))
	if reg.Get("k") != nil || !bridge.isClosed() {
		t.Fatal("reaper did not reclaim the session")
	}
}

func TestDrainSendFailureDetachesCurrentSocketButNotAReplacement(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("live")), 1024)
	s.Start()
	ws := newFailingPtyViewer(false)
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	<-ws.started
	ptyWaitUntil(t, "detach", func() bool { return !s.Attached() })
	if _, has := s.LastDetachedAt(); !has {
		t.Fatal("no detach time")
	}
	s.Close(ptyBg)
	s = newTestPtySession("k", newFakePtyBridge([]byte("live")), 1024)
	s.Start()
	stale := newFailingPtyViewer(true)
	s.Attach(ptyBg, stale, PtyAttachOptions{})
	<-stale.started
	replacement := &fakePtyViewer{}
	if ok, _ := s.Attach(ptyBg, replacement, PtyAttachOptions{}); !ok {
		t.Fatal("replacement attach failed")
	}
	close(stale.release)
	time.Sleep(50 * time.Millisecond)
	if _, has := s.LastDetachedAt(); !s.Attached() || s.currentViewer() != replacement || has {
		t.Fatal("superseded send failure detached the replacement")
	}
	s.Close(ptyBg)
}

func TestViewerThatStopsReadingLiveOutputIsToldToReconnect(t *testing.T) {
	setPtyVar(t, &ptyViewerSendTimeout, 100*time.Millisecond)
	reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: time.Minute, MaxSessions: 2, BufferCap: 1024, ReadTimeout: 10 * time.Millisecond})
	bridge := newFakePtyBridge()
	session, _, _ := reg.AttachOrSpawn(ptyBg, "k", spawnBridge(bridge), "")
	stalled := newFailingPtyViewer(true)
	if ok, _ := session.Attach(ptyBg, stalled, PtyAttachOptions{}); !ok {
		t.Fatal("attach with empty history failed")
	}
	bridge.push([]byte("live output"))
	ptyWaitUntil(t, "stalled close", func() bool { c, _ := stalled.closed(); return c != 0 })
	if c, _ := stalled.closed(); c != 1001 || !session.Alive() || session.Attached() {
		t.Fatalf("close code %d alive %v attached %v", c, session.Alive(), session.Attached())
	}
	again, created, _ := reg.AttachOrSpawn(ptyBg, "k", spawnBridge(newFakePtyBridge()), "")
	viewer := &fakePtyViewer{}
	if again != session || created {
		t.Fatal("same key did not reattach the same PTY")
	}
	if ok, _ := again.Attach(ptyBg, viewer, PtyAttachOptions{}); !ok {
		t.Fatal("reattach failed")
	}
	if f := viewer.frames(); len(f) != 1 || f[0] != (ptySent{"bytes", "live output"}) {
		t.Fatalf("frames %v", f)
	}
	reg.CloseAll(ptyBg)
}

func TestAFullReplayOverASlowLinkIsNotAStalledViewer(t *testing.T) {
	setPtyVar(t, &ptyViewerSendTimeout, 50*time.Millisecond)
	history := bytes.Repeat([]byte("x"), 1024*1024)
	session := newTestPtySession("k", newFakePtyBridge(), len(history))
	session.Buffer.Append(history)
	viewer := newSlowLinkViewer(500 * time.Millisecond)
	if ok, _ := session.Attach(ptyBg, viewer, PtyAttachOptions{}); !ok {
		t.Fatal("slow link replay counted as stalled")
	}
	if f := viewer.frames(); len(f) != 1 || f[0].payload != string(history) || viewer.closeCode != 0 || session.currentViewer() != viewer {
		t.Fatal("slow link replay not delivered")
	}
	small := newTestPtySession("k2", newFakePtyBridge(), 1024)
	small.Buffer.Append([]byte("tail"))
	stalled := newFailingPtyViewer(true)
	defer close(stalled.release)
	if ok, _ := small.Attach(ptyBg, stalled, PtyAttachOptions{}); ok || small.currentViewer() != nil {
		t.Fatal("stalled replay kept the viewer")
	}
}

// Shrinks the replay timing to the narrow link tests
func narrowReplayVars(t *testing.T, floor int) {
	setPtyVar(t, &ptyViewerSendTimeout, 10*time.Millisecond)
	setPtyVar(t, &ptyReplayMinBytesPerSecond, 1024*1024*1024)
	setPtyVar(t, &ptyReplayFloorBytes, floor)
}

// History of every byte value repeated
func ptyHistory() []byte {
	var b []byte
	for i := 0; i < 256; i++ {
		for j := 0; j < 256; j++ {
			b = append(b, byte(j))
		}
	}
	return b
}

func TestRedialsOverALinkTooSlowForTheWholeReplayReachAReplayItCarries(t *testing.T) {
	narrowReplayVars(t, 1024)
	history := ptyHistory()
	session := newTestPtySession("k", newFakePtyBridge(), len(history))
	session.Buffer.Append(history)
	if ok, _ := session.Attach(ptyBg, newFailingPtyViewer(false), PtyAttachOptions{}); ok {
		t.Fatal("dropped client attached")
	}
	whole := &fakePtyViewer{}
	if ok, _ := session.Attach(ptyBg, whole, PtyAttachOptions{}); !ok || whole.bytesSent() != string(history) {
		t.Fatal("dropped client shrank the replay")
	}
	var viewer *fakePtyViewer
	attached := false
	for redial := 0; redial < 12 && !attached; redial++ {
		viewer = newNarrowLinkViewer(4096)
		attached, _ = session.Attach(ptyBg, viewer, PtyAttachOptions{})
	}
	if !attached {
		t.Fatal("no redial reached a replay the link carries")
	}
	replay := viewer.frames()[0].payload
	if len(replay) == 0 || len(replay) > 4096 || !strings.HasSuffix(string(history), replay) || session.currentViewer() != viewer {
		t.Fatalf("replay of %d bytes", len(replay))
	}
}

func TestAShrunkReplayKeepsAFloor(t *testing.T) {
	narrowReplayVars(t, 1024)
	history := ptyHistory()
	session := newTestPtySession("k", newFakePtyBridge(), len(history))
	session.Buffer.Append(history)
	for stall := 0; stall < 20; stall++ {
		session.Attach(ptyBg, newNarrowLinkViewer(0), PtyAttachOptions{})
	}
	viewer := &fakePtyViewer{}
	if ok, _ := session.Attach(ptyBg, viewer, PtyAttachOptions{}); !ok {
		t.Fatal("attach failed")
	}
	if f := viewer.frames(); len(f) != 1 || f[0].payload != string(history[len(history)-1024:]) {
		t.Fatal("replay fell under the floor")
	}
}

func TestAReplaySizeThatTimedOutIsNotReplayedAgain(t *testing.T) {
	narrowReplayVars(t, 1024)
	history := ptyHistory()
	session := newTestPtySession("k", newFakePtyBridge(), len(history))
	session.Buffer.Append(history)
	var attached []bool
	var replays []string
	for redial := 0; redial < 6; redial++ {
		viewer := newNarrowLinkViewer(len(history) / 2)
		ok, _ := session.Attach(ptyBg, viewer, PtyAttachOptions{})
		attached = append(attached, ok)
		r := ""
		if f := viewer.frames(); len(f) > 0 {
			r = f[0].payload
		}
		replays = append(replays, r)
	}
	if attached[0] {
		t.Fatal("first oversized replay attached")
	}
	for i := 1; i < 6; i++ {
		if !attached[i] || replays[i] != string(history[len(history)/2:]) {
			t.Fatalf("redial %d attached %v replay %d bytes", i, attached[i], len(replays[i]))
		}
	}
}

func TestReattachCanForceCompleteTuiRedrawAfterReplay(t *testing.T) {
	bridge := newFakePtyBridge([]byte("partial differential frame"), []byte{})
	s := newTestPtySession("k", bridge, 1024)
	s.Start()
	ptyWaitUntil(t, "output", func() bool { return s.Outputs() == 1 })
	ws := &fakePtyViewer{}
	if ok, err := s.Attach(ptyBg, ws, PtyAttachOptions{ForceRedraw: true}); !ok || err != nil {
		t.Fatalf("attach %v %v", ok, err)
	}
	if ws.bytesSent() != "partial differential frame" || bridge.writtenBytes() != "\x0c" {
		t.Fatalf("replay %q written %q", ws.bytesSent(), bridge.writtenBytes())
	}
	s.Close(ptyBg)
}

func TestFailedRedrawMarksSessionDeadForReplacement(t *testing.T) {
	bridge := newFakePtyBridge([]byte{})
	bridge.writeResult = false
	s := newTestPtySession("k", bridge, 1024)
	s.Start()
	if ok, _ := s.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{ForceRedraw: true}); ok || s.Alive() {
		t.Fatal("failed redraw left the session alive")
	}
	s.Close(ptyBg)
}

func TestSessionSerializesInputAcrossSocketTasks(t *testing.T) {
	bridge := newFakePtyBridge([]byte{})
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	var once sync.Once
	bridge.writeHook = func(ctx context.Context, data []byte) (bool, error) {
		if bridge.writtenBytes() == "" {
			once.Do(func() { close(firstStarted) })
			<-releaseFirst
		}
		bridge.appendWritten(data)
		return true, nil
	}
	s := newTestPtySession("k", bridge, 1024)
	s.Start()
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	first, second := make(chan bool, 1), make(chan bool, 1)
	go func() { ok, _ := s.Write(ptyBg, ws, []byte("first"), PtyWriteTimeout, nil); first <- ok }()
	<-firstStarted
	go func() { ok, _ := s.Write(ptyBg, ws, []byte("second"), PtyWriteTimeout, nil); second <- ok }()
	time.Sleep(10 * time.Millisecond)
	if bridge.writtenBytes() != "" {
		t.Fatal("second write overtook the first")
	}
	close(releaseFirst)
	if !<-first || !<-second || bridge.writtenBytes() != "firstsecond" {
		t.Fatalf("written %q", bridge.writtenBytes())
	}
	s.Close(ptyBg)
}

func TestSupersededFailedWriteDoesNotKillReplacementSession(t *testing.T) {
	bridge := newFakePtyBridge([]byte{})
	oldStarted, releaseOld := make(chan struct{}), make(chan struct{})
	var calls int
	var mu sync.Mutex
	bridge.writeHook = func(ctx context.Context, data []byte) (bool, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(oldStarted)
			select {
			case <-releaseOld:
				return false, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		bridge.appendWritten(data)
		return true, nil
	}
	s := newTestPtySession("k", bridge, 1024)
	s.Start()
	oldWs, newWs := &fakePtyViewer{}, &fakePtyViewer{}
	s.Attach(ptyBg, oldWs, PtyAttachOptions{})
	oldWrite := make(chan bool, 1)
	go func() { ok, _ := s.Write(ptyBg, oldWs, []byte("old input"), PtyWriteTimeout, nil); oldWrite <- ok }()
	<-oldStarted
	newAttach := make(chan bool, 1)
	go func() { ok, _ := s.Attach(ptyBg, newWs, PtyAttachOptions{ForceRedraw: true}); newAttach <- ok }()
	ptyWaitUntil(t, "replacement attach", func() bool { return s.currentViewer() == newWs })
	close(releaseOld)
	if <-oldWrite {
		t.Fatal("superseded write reported delivery")
	}
	if !<-newAttach || !s.Alive() {
		t.Fatal("replacement session died")
	}
	if ok, _ := s.Write(ptyBg, newWs, []byte("new input"), PtyWriteTimeout, nil); !ok || bridge.writtenBytes() != "\x0cnew input" {
		t.Fatalf("written %q", bridge.writtenBytes())
	}
	s.Close(ptyBg)
}

func TestDetachKeepsDrainingIntoBuffer(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("one"), []byte{}, []byte("two")), 1024)
	s.Start()
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	s.Detach(ws)
	if _, has := s.LastDetachedAt(); s.Attached() || !has {
		t.Fatal("detach did not mark the session idle")
	}
	ptyWaitUntil(t, "two outputs", func() bool { return s.Outputs() == 2 })
	ws2 := &fakePtyViewer{}
	s.Attach(ptyBg, ws2, PtyAttachOptions{})
	if ws2.bytesSent() != "onetwo" {
		t.Fatalf("replay %q", ws2.bytesSent())
	}
	s.Close(ptyBg)
}

func TestEOFMarksDeadAndClosesSocket4410(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("bye"), nil), 1024)
	s.Start()
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	ptyWaitUntil(t, "close", func() bool { c, _ := ws.closed(); return c != 0 })
	if c, _ := ws.closed(); s.Alive() || c != 4410 {
		t.Fatalf("alive %v code %d", s.Alive(), c)
	}
	s.Close(ptyBg)
}

func TestSameKeyReattachesSameSession(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	b1 := newFakePtyBridge([]byte{}, []byte{}, []byte{})
	s1, created1, _ := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(b1), "")
	s2, created2, _ := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(newFakePtyBridge()), "")
	if !created1 || created2 || s1 != s2 || s2.Bridge != TerminalBridge(b1) {
		t.Fatal("second attach did not reuse the session")
	}
	reg.CloseAll(ptyBg)
}

func TestNewKeyAtCapacityRaisesWhenNoneReapable(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 1)
	s, _, _ := reg.AttachOrSpawn(ptyBg, "a", spawnBridge(newFakePtyBridge([]byte{}, []byte{})), "")
	s.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{})
	_, _, err := reg.AttachOrSpawn(ptyBg, "b", spawnBridge(newFakePtyBridge()), "")
	var full *RegistryFullError
	if !errors.As(err, &full) || full.Message != RegistryFullMessage {
		t.Fatalf("err %v", err)
	}
	reg.CloseAll(ptyBg)
}

func TestConcurrentAttachOnOneTokenForksOnePty(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	var mu sync.Mutex
	var spawned []*fakePtyBridge
	spawn := func() (TerminalBridge, error) {
		b := newFakePtyBridge([]byte{}, []byte{})
		mu.Lock()
		spawned = append(spawned, b)
		mu.Unlock()
		return b, nil
	}
	type res struct {
		s       *PtySession
		created bool
	}
	out := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() { s, c, _ := reg.AttachOrSpawn(ptyBg, "tok", spawn, ""); out <- res{s, c} }()
	}
	r1, r2 := <-out, <-out
	if len(spawned) != 1 || r1.s != r2.s || r1.created == r2.created || r1.s.Bridge != TerminalBridge(spawned[0]) {
		t.Fatal("one token forked more than one PTY")
	}
	if keys := reg.Keys(); len(keys) != 1 || reg.Get("tok") != r1.s {
		t.Fatal("handed out session not tracked")
	}
	wsA, wsB := &fakePtyViewer{}, &fakePtyViewer{}
	r1.s.Attach(ptyBg, wsA, PtyAttachOptions{})
	r2.s.Attach(ptyBg, wsB, PtyAttachOptions{})
	if cb, _ := wsB.closed(); r1.s.currentViewer() != wsB || cb != 0 {
		t.Fatal("second tab not attached")
	}
	if ca, _ := wsA.closed(); ca != WSCloseSuperseded {
		t.Fatalf("first tab close code %d", ca)
	}
	reg.CloseAll(ptyBg)
}

// Registers a session directly like the tests that write reg._sessions
func registerPtySession(reg *PtySessionRegistry, s *PtySession) {
	reg.mu.Lock()
	reg.putLocked(s.Key, s)
	reg.mu.Unlock()
}

// Two idle sessions whose first close parks until release closes
func twoIdleSessionsFirstCloseGated(reg *PtySessionRegistry) ([]*fakePtyBridge, chan struct{}, chan struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	var bridges []*fakePtyBridge
	for i := 0; i < 2; i++ {
		b := newFakePtyBridge([]byte{})
		s := newTestPtySession("k"+strconv.Itoa(i), b, 1024)
		s.Start()
		s.Detach(nil)
		registerPtySession(reg, s)
		bridges = append(bridges, b)
	}
	bridges[0].closeHook = func() {
		close(entered)
		<-release
	}
	return bridges, entered, release
}

func allPtyBridgesClosed(bridges []*fakePtyBridge) bool {
	for _, b := range bridges {
		if !b.isClosed() {
			return false
		}
	}
	return true
}

func TestConcurrentReapIdleIsIdempotent(t *testing.T) {
	reg := newTestPtyRegistry(time.Minute, 16)
	bridges, entered, release := twoIdleSessionsFirstCloseGated(reg)
	farFuture := ptyMonotonic().Add(10000 * time.Second)
	first := make(chan struct{})
	go func() { reg.ReapIdle(ptyBg, farFuture); close(first) }()
	<-entered
	reg.ReapIdle(ptyBg, farFuture)
	close(release)
	<-first
	if len(reg.Keys()) != 0 || !allPtyBridgesClosed(bridges) {
		t.Fatal("concurrent reaps left sessions")
	}
}

func TestAClosedRegistryRefusesSpawnsUntilReopened(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	reg.CloseAll(ptyBg)
	_, _, err := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(newFakePtyBridge()), "")
	var full *RegistryFullError
	if !errors.As(err, &full) || full.Message != "Terminal service is shutting down." {
		t.Fatalf("err %v", err)
	}
	reg.Reopen()
	session, created, err := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(newFakePtyBridge([]byte{})), "")
	if err != nil || !created || !session.Alive() {
		t.Fatalf("reopened spawn %v %v", created, err)
	}
	reg.CloseAll(ptyBg)
}

func TestCloseAllSurvivesKeyPoppedByConcurrentReap(t *testing.T) {
	reg := newTestPtyRegistry(time.Minute, 16)
	bridges, entered, release := twoIdleSessionsFirstCloseGated(reg)
	closer := make(chan struct{})
	go func() { reg.CloseAll(ptyBg); close(closer) }()
	<-entered
	reg.ReapIdle(ptyBg, ptyMonotonic().Add(10000*time.Second))
	close(release)
	<-closer
	if len(reg.Keys()) != 0 || !allPtyBridgesClosed(bridges) {
		t.Fatal("close_all left sessions")
	}
}

func TestCloseAllDoesNotWaitOnOneSlowCloseBeforeTheNext(t *testing.T) {
	reg := newTestPtyRegistry(time.Minute, 16)
	bridges, entered, release := twoIdleSessionsFirstCloseGated(reg)
	closer := make(chan struct{})
	go func() { reg.CloseAll(ptyBg); close(closer) }()
	<-entered
	ptyWaitUntil(t, "second close", bridges[1].isClosed)
	close(release)
	<-closer
	if !allPtyBridgesClosed(bridges) {
		t.Fatal("bridges left open")
	}
}

func TestCloseAllWaitsForClosesAlreadyRunningInTheBackground(t *testing.T) {
	reg := newTestPtyRegistry(time.Minute, 16)
	bridges, entered, release := twoIdleSessionsFirstCloseGated(reg)
	reg.mu.Lock()
	reg.closeInBackground(reg.popLocked("k0"))
	reg.mu.Unlock()
	<-entered
	closer := make(chan struct{})
	go func() { reg.CloseAll(ptyBg); close(closer) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-closer:
		t.Fatal("close_all returned before the background close")
	default:
	}
	close(release)
	<-closer
	if !allPtyBridgesClosed(bridges) {
		t.Fatal("bridges left open")
	}
}

func TestCloseOtherSessionsRemovesOldProfileSession(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	oldBridge, currentBridge := newFakePtyBridge([]byte{}), newFakePtyBridge([]byte{})
	old := newTestPtySession("token\x00alpha\x00session-a", oldBridge, 1024)
	current := newTestPtySession("token\x00beta\x00session-b", currentBridge, 1024)
	old.Start()
	current.Start()
	oldWs := &fakePtyViewer{}
	if ok, _ := old.Attach(ptyBg, oldWs, PtyAttachOptions{}); !ok {
		t.Fatal("attach failed")
	}
	registerPtySession(reg, old)
	registerPtySession(reg, current)
	reg.CloseOtherSessions(ptyBg, "token", current.Key)
	if c, _ := oldWs.closed(); !oldBridge.isClosed() || reg.Get(old.Key) != nil || reg.Get(current.Key) != current || c != WSCloseSuperseded {
		t.Fatalf("old session not superseded, code %d", c)
	}
	reg.CloseAll(ptyBg)
}

// Starts sessions, detaches the listed ones and registers all of them
func startRegisteredPtySessions(reg *PtySessionRegistry, detach bool, sessions ...*PtySession) {
	for _, s := range sessions {
		s.Start()
		if detach {
			s.Detach(nil)
		}
		registerPtySession(reg, s)
	}
}

func TestCloseOrphanedSessionsClosesPtyStrandedByTokenRotation(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	strandedBridge := newFakePtyBridge([]byte{})
	stranded := newTestPtySession("old-token\x00\x00session-a", strandedBridge, 1024)
	current := newTestPtySession("new-token\x00\x00session-a", newFakePtyBridge([]byte{}), 1024)
	startRegisteredPtySessions(reg, true, stranded)
	startRegisteredPtySessions(reg, false, current)
	reg.CloseOrphanedSessions(ptyBg, "session-a", current.Key, nil)
	if !strandedBridge.isClosed() || reg.Get(stranded.Key) != nil || reg.Get(current.Key) != current {
		t.Fatal("stranded PTY kept")
	}
	reg.CloseAll(ptyBg)
}

func TestCloseOrphanedSessionsKeepsAPtySomeoneIsViewing(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	viewedBridge := newFakePtyBridge([]byte{})
	viewed := newTestPtySession("old-token\x00\x00session-a", viewedBridge, 1024)
	current := newTestPtySession("new-token\x00\x00session-a", newFakePtyBridge([]byte{}), 1024)
	startRegisteredPtySessions(reg, false, viewed, current)
	if ok, _ := viewed.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{}); !ok {
		t.Fatal("attach failed")
	}
	reg.CloseOrphanedSessions(ptyBg, "session-a", current.Key, nil)
	if viewedBridge.isClosed() || reg.Get(viewed.Key) != viewed || reg.Get(current.Key) != current {
		t.Fatal("viewed PTY closed")
	}
	reg.CloseAll(ptyBg)
}

func TestCloseOrphanedSessionsWithoutResumeTargetTouchesNothing(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	otherBridge := newFakePtyBridge([]byte{})
	other := newTestPtySession("old-token\x00\x00", otherBridge, 1024)
	startRegisteredPtySessions(reg, true, other)
	reg.CloseOrphanedSessions(ptyBg, "", "new-token\x00\x00", nil)
	if otherBridge.isClosed() || reg.Get(other.Key) != other {
		t.Fatal("session closed without a resume target")
	}
	reg.CloseAll(ptyBg)
}

func TestKeySegmentsReadProfileAndResume(t *testing.T) {
	cases := map[string][2]string{
		"token\x00profile\x00session-a": {"profile", "session-a"},
		"token\x00\x00session-b":        {"", "session-b"},
		"token\x00profile\x00":          {"profile", ""},
		"token":                         {"", ""},
	}
	for key, want := range cases {
		if p, r := keySegments(key); p != want[0] || r != want[1] {
			t.Errorf("keySegments(%q) = %q %q", key, p, r)
		}
	}
}

func TestCloseOrphanedSessionsLeavesTheSameSessionIDInAnotherProfile(t *testing.T) {
	reg := newTestPtyRegistry(1800*time.Second, 16)
	otherBridge := newFakePtyBridge([]byte{})
	other := newTestPtySession("old-token\x00beta\x00session-a", otherBridge, 1024)
	current := newTestPtySession("new-token\x00alpha\x00session-a", newFakePtyBridge([]byte{}), 1024)
	startRegisteredPtySessions(reg, true, other)
	startRegisteredPtySessions(reg, false, current)
	reg.CloseOrphanedSessions(ptyBg, "session-a", current.Key, nil)
	if otherBridge.isClosed() || reg.Get(other.Key) != other {
		t.Fatal("another profile's chat closed")
	}
	reg.CloseAll(ptyBg)
}

// Fake bridge reporting pid
func pidPtyBridge(pid int) *fakePtyBridge {
	b := newFakePtyBridge([]byte{})
	b.pid = pid
	return b
}

// Installs the process tree where 4243 runs under PTY leader 4242
func fakePtyProcessTree(t *testing.T) {
	tree := map[int]map[int]bool{4243: {4242: true, 1: true}, 5152: {5151: true, 1: true}}
	setPtyVar(t, &ptyProcessAncestors, func(pid int) map[int]bool {
		if a, ok := tree[pid]; ok {
			return a
		}
		return map[int]bool{}
	})
}

func ptyPid(pid int) *int { return &pid }

func TestCloseOrphanedSessionsFindsTheLeaseHolderWithoutAResumeKey(t *testing.T) {
	fakePtyProcessTree(t)
	reg := newTestPtyRegistry(1800*time.Second, 16)
	freshA := newTestPtySession("old-token", pidPtyBridge(4242), 1024)
	unrelated := newTestPtySession("other-token", pidPtyBridge(5151), 1024)
	current := newTestPtySession("new-token\x00\x00session-a", newFakePtyBridge([]byte{}), 1024)
	startRegisteredPtySessions(reg, true, freshA, unrelated, current)
	reg.CloseOrphanedSessions(ptyBg, "session-a", current.Key, ptyPid(4243))
	if !freshA.Bridge.(*fakePtyBridge).isClosed() || reg.Get(freshA.Key) != nil {
		t.Fatal("lease holder PTY kept")
	}
	if unrelated.Bridge.(*fakePtyBridge).isClosed() || reg.Get(unrelated.Key) != unrelated || reg.Get(current.Key) != current {
		t.Fatal("unrelated PTY closed")
	}
	reg.CloseAll(ptyBg)
}

func TestCloseOrphanedSessionsLeavesAHolderOutsideEveryPty(t *testing.T) {
	fakePtyProcessTree(t)
	reg := newTestPtyRegistry(1800*time.Second, 16)
	detached := newTestPtySession("old-token", pidPtyBridge(4242), 1024)
	startRegisteredPtySessions(reg, true, detached)
	reg.CloseOrphanedSessions(ptyBg, "session-a", "new-token\x00\x00session-a", ptyPid(9000))
	if detached.Bridge.(*fakePtyBridge).isClosed() || reg.Get(detached.Key) != detached {
		t.Fatal("PTY outside the holder's tree closed")
	}
	reg.CloseAll(ptyBg)
}

func TestReapReapsDeadProcessEvenWhenAttached(t *testing.T) {
	reg := newTestPtyRegistry(3600*time.Second, 16)
	b := newFakePtyBridge([]byte{}, []byte{}, []byte{})
	s, _, _ := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(b), "")
	s.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{})
	b.mu.Lock()
	b.dead = true
	b.mu.Unlock()
	reg.ReapIdle(ptyBg, time.Time{})
	if reg.Get("tok") != nil || !b.isClosed() {
		t.Fatal("dead attached PTY kept")
	}
	reg.CloseAll(ptyBg)
}

func TestCloseOrphanedSessionsLeavesALeaseHoldingPtyThatIsViewed(t *testing.T) {
	fakePtyProcessTree(t)
	reg := newTestPtyRegistry(1800*time.Second, 16)
	watched := newTestPtySession("old-token", pidPtyBridge(4242), 1024)
	current := newTestPtySession("new-token\x00\x00session-a", newFakePtyBridge([]byte{}), 1024)
	startRegisteredPtySessions(reg, false, watched, current)
	if ok, _ := watched.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{}); !ok {
		t.Fatal("attach failed")
	}
	reg.CloseOrphanedSessions(ptyBg, "session-a", current.Key, ptyPid(4243))
	if watched.Bridge.(*fakePtyBridge).isClosed() || reg.Get(watched.Key) != watched {
		t.Fatal("viewed lease holder closed")
	}
	reg.CloseAll(ptyBg)
}

func TestHostsPidToleratesABridgeWithoutOne(t *testing.T) {
	session := newTestPtySession("tok", newFakePtyBridge([]byte{}), 8)
	if session.HostsPid(nil) || session.HostsPid(ptyPid(123)) || pidPtyBridge(7).Pid() != 7 {
		t.Fatal("pidless bridge matched")
	}
}

func TestReapKeepsLiveAttachedSession(t *testing.T) {
	reg := newTestPtyRegistry(time.Second, 16)
	b := newFakePtyBridge([]byte{}, []byte{})
	s, _, _ := reg.AttachOrSpawn(ptyBg, "tok", spawnBridge(b), "")
	s.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{})
	reg.ReapIdle(ptyBg, ptyMonotonic().Add(10*time.Second))
	if reg.Get("tok") == nil || b.isClosed() {
		t.Fatal("live attached session reaped")
	}
	reg.CloseAll(ptyBg)
}

// ConPTY child of an idle shell whose reader polls with the bridge's bounded wait
type idleShellChild struct {
	output     chan []byte
	polled     chan struct{}
	polledOnce sync.Once
	closed     chan struct{}
	closeOnce  sync.Once
}

func newIdleShellChild() *idleShellChild {
	return &idleShellChild{output: make(chan []byte, 4), polled: make(chan struct{}), closed: make(chan struct{})}
}

func (c *idleShellChild) Pid() int      { return 1 }
func (c *idleShellChild) IsAlive() bool { return true }

func (c *idleShellChild) Read(timeout time.Duration) ([]byte, error) {
	c.polledOnce.Do(func() { close(c.polled) })
	select {
	case data := <-c.output:
		return data, nil
	case <-c.closed:
		return nil, errors.New("Pty is closed")
	case <-time.After(timeout):
		return nil, nil
	}
}

func (c *idleShellChild) WriteText(string) error          { return nil }
func (c *idleShellChild) SetWinSize(rows, cols int) error { return nil }

func (c *idleShellChild) Terminate(bool) error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// Executor of a fixed number of worker goroutines
func newTestPtyExecutor(t *testing.T, workers int) func(task func()) {
	tasks := make(chan func(), 64)
	stop := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			for {
				select {
				case task := <-tasks:
					task()
				case <-stop:
					return
				}
			}
		}()
	}
	t.Cleanup(func() { close(stop) })
	return func(task func()) { tasks <- task }
}

func TestIdleTerminalsLeaveTheDefaultExecutorFree(t *testing.T) {
	executor := newTestPtyExecutor(t, 2)
	var shells []*idleShellChild
	var sessions []*PtySession
	for n := 0; n < 4; n++ {
		shell := newIdleShellChild()
		shells = append(shells, shell)
		sessions = append(sessions, NewPtySession("terminal-"+strconv.Itoa(n), newWinPtyBridge(shell),
			PtySessionOptions{BufferCap: 64, ReadTimeout: 50 * time.Millisecond, DrainExecutor: executor}))
	}
	defer func() {
		for _, shell := range shells {
			shell.Terminate(false)
		}
		for _, s := range sessions {
			s.Close(ptyBg)
		}
	}()
	for _, s := range sessions {
		s.Start()
	}
	ptyWaitUntil(t, "every idle shell polled", func() bool {
		for _, shell := range shells {
			select {
			case <-shell.polled:
			default:
				return false
			}
		}
		return true
	})
	ran := make(chan string, 1)
	executor(func() { ran <- "ran" })
	select {
	case got := <-ran:
		if got != "ran" {
			t.Fatal(got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executor held by idle terminals")
	}
	shells[1].output <- []byte("héllo")
	ptyWaitUntil(t, "output buffered", func() bool { return string(sessions[1].Buffer.Snapshot()) == "héllo" })
}
