package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEOFCloseSaysTheProcessExited(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("logout"), nil), 1024)
	s.Start()
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	ptyWaitUntil(t, "close", func() bool { c, _ := ws.closed(); return c != 0 })
	if c, r := ws.closed(); c != 4410 || r != "Process exited" {
		t.Fatalf("close %d %q", c, r)
	}
	s.Close(ptyBg)
}

func TestAttachingToAShellThatExitedWhileDetachedSaysItExited(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge([]byte("logout"), nil), 1024)
	s.Start()
	ptyWaitUntil(t, "eof", func() bool { return !s.Alive() })
	ws := &fakePtyViewer{}
	if ok, _ := s.Attach(ptyBg, ws, PtyAttachOptions{InitialText: PtyStaticText("meta")}); ok {
		t.Fatal("attach to an exited shell succeeded")
	}
	f := ws.frames()
	if len(f) != 2 || f[0] != (ptySent{"text", "meta"}) || f[1] != (ptySent{"bytes", "logout"}) {
		t.Fatalf("frames %v", f)
	}
	if c, r := ws.closed(); c != 4410 || r != "Process exited" {
		t.Fatalf("close %d %q", c, r)
	}
	s.Close(ptyBg)
}

func TestAServerCloseKeeps4410WithoutTheExitReason(t *testing.T) {
	s := newTestPtySession("k", newFakePtyBridge(), 1024)
	s.Start()
	ws := &fakePtyViewer{}
	s.Attach(ptyBg, ws, PtyAttachOptions{})
	s.Close(ptyBg)
	if c, r := ws.closed(); c != 4410 || r == "Process exited" {
		t.Fatalf("close %d %q", c, r)
	}
}

func TestMetadataReplayLiveOrderAndSupersededInputFence(t *testing.T) {
	bridge := newFakePtyBridge()
	session := newTestPtySession("k", bridge, 1024)
	session.Buffer.Append([]byte("replay"))
	entered, release := make(chan struct{}), make(chan struct{})
	old := &fakePtyViewer{sendText: func(ctx context.Context, _ string) error {
		close(entered)
		<-release
		return nil
	}}
	attaching := make(chan bool, 1)
	go func() {
		ok, _ := session.Attach(ptyBg, old, PtyAttachOptions{InitialText: PtyStaticText("meta")})
		attaching <- ok
	}()
	<-entered
	bridge.push([]byte("live"))
	session.Start()
	ptyWaitUntil(t, "live buffered", func() bool { return string(session.Buffer.Snapshot()) == "replaylive" })
	if len(old.frames()) != 0 {
		t.Fatal("live output overtook the metadata")
	}
	close(release)
	if !<-attaching {
		t.Fatal("attach failed")
	}
	ptyWaitUntil(t, "three frames", func() bool { return len(old.frames()) == 3 })
	if f := old.frames(); f[0] != (ptySent{"text", "meta"}) || f[1] != (ptySent{"bytes", "replay"}) || f[2] != (ptySent{"bytes", "live"}) {
		t.Fatalf("frames %v", f)
	}
	replacement := &fakePtyViewer{}
	if ok, _ := session.Attach(ptyBg, replacement, PtyAttachOptions{InitialText: PtyStaticText("resume")}); !ok {
		t.Fatal("replacement attach failed")
	}
	if f := replacement.frames(); len(f) != 2 || f[0] != (ptySent{"text", "resume"}) || f[1] != (ptySent{"bytes", "replaylive"}) {
		t.Fatalf("replacement frames %v", f)
	}
	if c, _ := old.closed(); c != 4409 {
		t.Fatalf("old close %d", c)
	}
	session.Write(ptyBg, old, []byte("stale"), PtyWriteTimeout, nil)
	session.Resize(old, 5, 6)
	session.Detach(old)
	if session.currentViewer() != replacement || bridge.writtenBytes() != "" || bridge.resized != [2]int{} {
		t.Fatal("superseded socket reached the PTY")
	}
	session.Write(ptyBg, replacement, []byte("current"), PtyWriteTimeout, nil)
	session.Resize(replacement, 80, 24)
	if bridge.writtenBytes() != "current" || bridge.resized != [2]int{80, 24} {
		t.Fatalf("written %q resized %v", bridge.writtenBytes(), bridge.resized)
	}
	session.Close(ptyBg)
}

func TestConcurrentAttachClaimsBeforeSlowOldClose(t *testing.T) {
	session := newTestPtySession("k", newFakePtyBridge(), 1024)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	old := &fakePtyViewer{closeHook: func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	first, winner := &fakePtyViewer{}, &fakePtyViewer{}
	session.Attach(ptyBg, old, PtyAttachOptions{})
	one := make(chan bool, 1)
	go func() {
		ok, _ := session.Attach(ptyBg, first, PtyAttachOptions{InitialText: PtyStaticText("first")})
		one <- ok
	}()
	<-entered
	if ok, _ := session.Attach(ptyBg, winner, PtyAttachOptions{InitialText: PtyStaticText("winner")}); !ok {
		t.Fatal("winner attach failed")
	}
	close(release)
	if <-one {
		t.Fatal("superseded attach succeeded")
	}
	c, _ := first.closed()
	if session.currentViewer() != winner || c != 4409 || len(first.frames()) != 0 {
		t.Fatalf("first close %d frames %v", c, first.frames())
	}
	if f := winner.frames(); len(f) != 2 || f[0] != (ptySent{"text", "winner"}) || f[1] != (ptySent{"bytes", ""}) {
		t.Fatalf("winner frames %v", f)
	}
	session.Close(ptyBg)
}

func TestFailedInitialDeliveryHasFiniteRetention(t *testing.T) {
	for _, failure := range []string{"metadata", "replay"} {
		t.Run(failure, func(t *testing.T) {
			reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 30 * time.Second, MaxSessions: 1, BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
			bridge := newFakePtyBridge()
			session, _, _ := reg.AttachOrSpawn(ptyBg, "k", spawnBridge(bridge), "")
			session.Buffer.Append([]byte("buffer"))
			broken := &fakePtyViewer{
				sendText: func(context.Context, string) error {
					if failure == "metadata" {
						return errors.New("lost metadata")
					}
					return nil
				},
				sendBytes: func(context.Context, []byte) error { return errors.New("lost replay") },
			}
			if ok, _ := session.Attach(ptyBg, broken, PtyAttachOptions{InitialText: PtyStaticText("meta")}); ok {
				t.Fatal("broken socket attached")
			}
			last, has := session.LastDetachedAt()
			if session.Attached() || !has {
				t.Fatal("broken socket kept the session attached")
			}
			reg.ReapIdle(ptyBg, last.Add(31*time.Second))
			if !bridge.isClosed() || len(reg.Keys()) != 0 {
				t.Fatal("session retained forever")
			}
			reg.CloseAll(ptyBg)
		})
	}
}

func TestConcurrentSpawnReservesKeyCapacityAndShutdown(t *testing.T) {
	reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 30 * time.Second, MaxSessions: 1, BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
	bridge := newFakePtyBridge()
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	var current *fakePtyBridge
	spawn := func() (TerminalBridge, error) {
		mu.Lock()
		calls++
		b := current
		mu.Unlock()
		entered <- struct{}{}
		<-release
		return b, nil
	}
	current = bridge
	type res struct {
		s    *PtySession
		made bool
	}
	one, two := make(chan res, 1), make(chan res, 1)
	go func() { s, m, _ := reg.AttachOrSpawn(ptyBg, "k", spawn, ""); one <- res{s, m} }()
	<-entered
	go func() { s, m, _ := reg.AttachOrSpawn(ptyBg, "k", spawn, ""); two <- res{s, m} }()
	close(release)
	a, b := <-one, <-two
	if a.s != b.s || !a.made || b.made || calls != 1 {
		t.Fatalf("made %v %v calls %d", a.made, b.made, calls)
	}
	a.s.Attach(ptyBg, &fakePtyViewer{}, PtyAttachOptions{})
	var full *RegistryFullError
	if _, _, err := reg.AttachOrSpawn(ptyBg, "other", spawn, ""); !errors.As(err, &full) {
		t.Fatalf("err %v", err)
	}
	reg.CloseAll(ptyBg)
	if !bridge.isClosed() {
		t.Fatal("bridge left open")
	}
	reg = NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 30 * time.Second, MaxSessions: 1, BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
	bridge = newFakePtyBridge()
	mu.Lock()
	current = bridge
	mu.Unlock()
	release = make(chan struct{})
	spawning, closing := make(chan struct{}), make(chan struct{})
	go func() { reg.AttachOrSpawn(ptyBg, "k", spawn, ""); close(spawning) }()
	<-entered
	go func() { reg.CloseAll(ptyBg); close(closing) }()
	time.Sleep(time.Millisecond)
	close(release)
	<-spawning
	<-closing
	if !bridge.isClosed() || len(reg.Keys()) != 0 {
		t.Fatal("shutdown left the admitted spawn running")
	}
}

func TestCancelledSpawnRetainsCapacityUntilCleanup(t *testing.T) {
	for _, spawnFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "spawned", true: "spawn_fails"}[spawnFails], func(t *testing.T) {
			reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 30 * time.Second, MaxSessions: 1, BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
			entered, release := make(chan struct{}), make(chan struct{})
			bridge, replacement := newFakePtyBridge(), newFakePtyBridge()
			var mu sync.Mutex
			var events []string
			spawn := func() (TerminalBridge, error) {
				close(entered)
				<-release
				mu.Lock()
				events = append(events, "spawn finished")
				mu.Unlock()
				if spawnFails {
					return nil, errors.New("fork failed after caller disconnected")
				}
				return bridge, nil
			}
			nextSpawn := func() (TerminalBridge, error) {
				mu.Lock()
				events = append(events, "next spawn")
				mu.Unlock()
				if !spawnFails && !bridge.isClosed() {
					t.Error("capacity released before abandoned child was closed")
				}
				return replacement, nil
			}
			ctx, cancel := context.WithCancel(ptyBg)
			spawning := make(chan error, 1)
			go func() { _, _, err := reg.AttachOrSpawn(ctx, "k", spawn, ""); spawning <- err }()
			<-entered
			cancel()
			if err := <-spawning; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled spawn returned %v", err)
			}
			type res struct {
				s    *PtySession
				made bool
				err  error
			}
			next := make(chan res, 1)
			go func() { s, m, err := reg.AttachOrSpawn(ptyBg, "k", nextSpawn, ""); next <- res{s, m, err} }()
			time.Sleep(time.Millisecond)
			close(release)
			r := <-next
			if r.err != nil || !r.made || r.s.Bridge != TerminalBridge(replacement) {
				t.Fatalf("next request %v %v", r.made, r.err)
			}
			mu.Lock()
			got := strings.Join(events, ",")
			mu.Unlock()
			if got != "spawn finished,next spawn" || (!spawnFails && !bridge.isClosed()) {
				t.Fatalf("events %s", got)
			}
			last, has := r.s.LastDetachedAt()
			if !has {
				t.Fatal("no detach time")
			}
			reg.ReapIdle(ptyBg, last.Add(31*time.Second))
			if !replacement.isClosed() || len(reg.Keys()) != 0 {
				t.Fatal("replacement not reaped")
			}
			reg.CloseAll(ptyBg)
		})
	}
}

func TestSpawnFailureReachesCallerAndReleasesCapacity(t *testing.T) {
	reg := NewPtySessionRegistry(PtySessionRegistryOptions{TTL: 30 * time.Second, MaxSessions: 1, BufferCap: 32, ReadTimeout: 10 * time.Millisecond})
	failure := errors.New("fork failed with caller still connected")
	bridge := newFakePtyBridge()
	if _, _, err := reg.AttachOrSpawn(ptyBg, "k", func() (TerminalBridge, error) { return nil, failure }, ""); err != failure {
		t.Fatalf("err %v", err)
	}
	ctx, cancel := context.WithTimeout(ptyBg, 2*time.Second)
	defer cancel()
	session, made, err := reg.AttachOrSpawn(ctx, "k", spawnBridge(bridge), "")
	if err != nil || !made || session.Bridge != TerminalBridge(bridge) {
		t.Fatalf("retry %v %v", made, err)
	}
	reg.CloseAll(ptyBg)
	if !bridge.isClosed() {
		t.Fatal("bridge left open")
	}
}

func TestSupersedeCancelsInflightInputBeforeReplacementWrites(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	bridge := newFakePtyBridge()
	bridge.writeHook = func(ctx context.Context, data []byte) (bool, error) {
		if string(data) == "old" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		bridge.appendWritten(data)
		return true, nil
	}
	session := newTestPtySession("k", bridge, 32)
	old, next := &fakePtyViewer{}, &fakePtyViewer{}
	session.Attach(ptyBg, old, PtyAttachOptions{})
	writing := make(chan bool, 1)
	go func() { ok, _ := session.Write(ptyBg, old, []byte("old"), PtyWriteTimeout, nil); writing <- ok }()
	<-entered
	session.Attach(ptyBg, next, PtyAttachOptions{})
	close(release)
	if <-writing {
		t.Fatal("superseded input delivered")
	}
	if ok, _ := session.Write(ptyBg, next, []byte("new"), PtyWriteTimeout, nil); !ok || bridge.writtenBytes() != "new" {
		t.Fatalf("written %q", bridge.writtenBytes())
	}
}

func TestCancelledCloseStillReapsChild(t *testing.T) {
	bridge := newFakePtyBridge()
	session := newTestPtySession("k", bridge, 32)
	entered, release := make(chan struct{}), make(chan struct{})
	session.Attach(ptyBg, &fakePtyViewer{closeHook: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}, PtyAttachOptions{})
	ctx, cancel := context.WithCancel(ptyBg)
	closing := make(chan error, 1)
	go func() { closing <- session.Close(ctx) }()
	<-entered
	cancel()
	if err := <-closing; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close returned %v", err)
	}
	close(release)
	session.Close(ptyBg)
	if !bridge.isClosed() {
		t.Fatal("child not reaped")
	}
}
