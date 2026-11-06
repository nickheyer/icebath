//go:build !windows

package tools

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSupersededInputCancellationWinsCompletedReadiness(t *testing.T) {
	spawned, err := SpawnPtyBridge(ptyBg, nil, []string{"sh", "-c", "stty raw -echo; exec cat"}, PtySpawnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bridge := spawned.(*PtyBridge)
	defer bridge.Close()
	time.Sleep(200 * time.Millisecond)
	realWrite := bridge.sysWrite
	entered, readiness := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var writes []string
	enteredSet := false
	bridge.sysWrite = func(p []byte) (int, error) {
		mu.Lock()
		set := enteredSet
		if set {
			writes = append(writes, string(p))
		}
		mu.Unlock()
		if !set {
			return 0, syscall.EAGAIN
		}
		return realWrite(p)
	}
	bridge.waitWritable = func(ctx context.Context, _ time.Duration) bool {
		mu.Lock()
		enteredSet = true
		mu.Unlock()
		close(entered)
		<-readiness
		<-ctx.Done()
		return true
	}
	session := newTestPtySession("k", bridge, 32)
	old, next := &fakePtyViewer{}, &fakePtyViewer{}
	session.Attach(ptyBg, old, PtyAttachOptions{})
	writing := make(chan bool, 1)
	go func() { ok, _ := session.Write(ptyBg, old, []byte("STALE"), PtyWriteTimeout, nil); writing <- ok }()
	<-entered
	close(readiness)
	session.Attach(ptyBg, next, PtyAttachOptions{})
	result := <-writing
	if ok, _ := session.Write(ptyBg, next, []byte("CURRENT"), PtyWriteTimeout, nil); !ok {
		t.Fatal("current write failed")
	}
	mu.Lock()
	got := strings.Join(writes, "|")
	mu.Unlock()
	if got != "CURRENT" || result || !session.Alive() {
		t.Fatalf("writes %q result %v alive %v", got, result, session.Alive())
	}
	out := ptbReadUntil(bridge, []byte("CURRENT"), 2*time.Second)
	if !strings.Contains(string(out), "CURRENT") || strings.Contains(string(out), "STALE") {
		t.Fatalf("child read %q", out)
	}
}

func TestHostsPidSeesTheLeaseHoldingGrandchildOfARealPty(t *testing.T) {
	if !TerminalBridgeAvailable() {
		t.Fatal("no pseudo terminal on this host")
	}
	bridge, err := SpawnTerminalBridge(ptyBg, nil, []string{"sh", "-c", `sh -c 'echo CHILD $$; sleep 20'; true`}, PtySpawnOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	out, deadline := "", time.Now().Add(10*time.Second)
	for !strings.Contains(out, "CHILD") && time.Now().Before(deadline) {
		chunk, _ := bridge.Read(200 * time.Millisecond)
		out += string(chunk)
	}
	ptyWaitUntil(t, "grandchild pid", func() bool {
		if chunk, _ := bridge.Read(50 * time.Millisecond); chunk != nil {
			out += string(chunk)
		}
		f := strings.Fields(strings.SplitN(out, "CHILD", 2)[1])
		return len(f) > 0
	})
	grandchild, err := strconv.Atoi(strings.Fields(strings.SplitN(out, "CHILD", 2)[1])[0])
	if err != nil {
		t.Fatal(err)
	}
	session := newTestPtySession("tok", bridge, 8)
	if grandchild == bridge.Pid() || !session.HostsPid(&grandchild) || !session.HostsPid(ptyPid(bridge.Pid())) || session.HostsPid(ptyPid(os.Getpid())) {
		t.Fatal("ancestry did not identify the PTY")
	}
}

func TestTakeoverRevokesWaitingWorkersAndWaitsForAnAdmittedWrite(t *testing.T) {
	for _, pauseAtSink := range []bool{true, false} {
		t.Run(map[bool]string{true: "pause_at_sink", false: "pause_in_fence"}[pauseAtSink], func(t *testing.T) {
			spawned, err := SpawnPtyBridge(ptyBg, nil, []string{"sh", "-c", "stty raw -echo; exec cat"}, PtySpawnOptions{})
			if err != nil {
				t.Fatal(err)
			}
			bridge := spawned.(*PtyBridge)
			defer bridge.Close()
			time.Sleep(200 * time.Millisecond)
			session := newTestPtySession("terminal", bridge, 32)
			old, next := &fakePtyViewer{}, &fakePtyViewer{}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce, finishedOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseAll()
			pause := func() {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					t.Error("write barrier was not released")
				}
			}
			realWrite := bridge.sysWrite
			bridge.sysWrite = func(p []byte) (int, error) {
				if pauseAtSink && string(p) == "OLD\n" {
					pause()
				}
				return realWrite(p)
			}
			admit := func(write PtyWriteAttempt) (int, bool, error) { return write() }
			oldFence := func(write PtyWriteAttempt) (int, bool, error) {
				defer finishedOnce.Do(func() { close(finished) })
				if !pauseAtSink {
					pause()
				}
				return admit(write)
			}
			if ok, _ := session.Attach(ptyBg, old, PtyAttachOptions{}); !ok {
				t.Fatal("attach failed")
			}
			writing, takeover := make(chan bool, 1), make(chan bool, 1)
			go func() { ok, _ := session.Write(ptyBg, old, []byte("OLD\n"), PtyWriteTimeout, oldFence); writing <- ok }()
			<-entered
			go func() { ok, _ := session.Attach(ptyBg, next, PtyAttachOptions{}); takeover <- ok }()
			if pauseAtSink {
				select {
				case <-takeover:
					t.Fatal("takeover did not wait for the admitted write")
				case <-time.After(50 * time.Millisecond):
				}
				releaseAll()
			}
			if !<-takeover {
				t.Fatal("takeover failed")
			}
			if <-writing {
				t.Fatal("superseded write reported delivery")
			}
			if c, _ := old.closed(); c != WSCloseSuperseded {
				t.Fatalf("old close %d", c)
			}
			if ok, _ := session.Write(ptyBg, next, []byte("NEW\n"), PtyWriteTimeout, admit); !ok {
				t.Fatal("new write failed")
			}
			releaseAll()
			<-finished
			if _, err := realWrite([]byte("END\n")); err != nil {
				t.Fatal(err)
			}
			expected := "NEW\n"
			if pauseAtSink {
				expected = "OLD\nNEW\n"
			}
			out := strings.ReplaceAll(string(ptbReadUntil(bridge, []byte("END\n"), 3*time.Second)), "\r\n", "\n")
			if out != expected+"END\n" || !session.Alive() {
				t.Fatalf("child read %q alive %v", out, session.Alive())
			}
		})
	}
}
