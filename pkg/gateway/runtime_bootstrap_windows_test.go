//go:build windows

package gateway

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/sys/windows"
)

func TestNativePipeAuthenticatedPeerAndDeadline(t *testing.T) {
	dir := t.TempDir()
	stalled := make(chan struct{})
	server := NewNativeControlServer(dir, func(raw []byte, subject string) []byte {
		if !strings.HasPrefix(subject, "sid:S-1-") {
			t.Errorf("subject %q", subject)
		}
		if string(raw) == "stall" {
			select {
			case <-stalled:
			case <-time.After(10 * time.Second):
			}
		}
		out, _ := json.Marshal(map[string]string{"subject": subject})
		return append(out, '\n')
	})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(stalled)
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if server.Alive(0) {
			t.Error("first worker still alive")
		}
	}()
	for range 3 {
		data, err := queryRuntimeControl(dir, []byte("hello"), 5*time.Second)
		var got map[string]string
		if err != nil || json.Unmarshal(data, &got) != nil || !strings.HasPrefix(got["subject"], "sid:") {
			t.Fatalf("%q %v", data, err)
		}
	}
	if !server.Alive(0) || server.Err() != nil {
		t.Fatalf("worker stopped: %v", server.Err())
	}
	started := time.Now()
	if _, err := queryRuntimeControl(dir, []byte("stall"), 100*time.Millisecond); !errors.Is(err, ErrControlTimeout) {
		t.Fatalf("%v", err)
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatalf("took %s", time.Since(started))
	}
}

func TestNativePipeHandlerMayDialItsOwnPipe(t *testing.T) {
	dir := t.TempDir()
	server := NewNativeControlServer(dir, func(raw []byte, _ string) []byte {
		if string(raw) == "outer" {
			var inner any
			data, err := queryRuntimeControl(dir, []byte("inner"), 5*time.Second)
			if err == nil {
				err = json.Unmarshal(data, &inner)
			}
			if err != nil {
				inner = map[string]string{"error": err.Error()}
			}
			out, _ := json.Marshal(map[string]any{"outer": inner})
			return append(out, '\n')
		}
		time.Sleep(2500 * time.Millisecond)
		out, _ := json.Marshal(map[string]string{"inner": string(raw)})
		return append(out, '\n')
	})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	data, err := queryRuntimeControl(dir, []byte("outer"), 15*time.Second)
	if err != nil || strings.TrimSpace(string(data)) != `{"outer":{"inner":"inner"}}` {
		t.Fatalf("%q %v", data, err)
	}
	if server.Err() != nil {
		t.Fatal(server.Err())
	}
}

func TestClientReadsARealGatewayPipesOwnerDaclAndSession(t *testing.T) {
	dir := t.TempDir()
	server := NewNativeControlServer(dir, func([]byte, string) []byte { return []byte("{}\n") })
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	name, err := WindowsPipeName(dir)
	if err != nil {
		t.Fatal(err)
	}
	namePtr, _ := windows.UTF16PtrFromString(name)
	handle, err := windows.CreateFile(namePtr, 0xC0000000, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	facts := win32PipeFacts{}
	own, err := facts.processSID(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	sec, err := facts.pipeSecurity(uintptr(handle))
	if err != nil {
		t.Fatal(err)
	}
	if sec.Owner != own && !(sec.Owner == administratorsSID && home.ProcessIsElevated()) {
		t.Fatalf("owner %s", sec.Owner)
	}
	if len(sec.ACEs) != 1 || sec.ACEs[0] != (pipeACE{accessAllowedACEType, own}) {
		t.Fatalf("aces %v", sec.ACEs)
	}
	if s, o := facts.pipeServerSession(uintptr(handle)), sessionOf(uint32(os.Getpid())); s == nil || o == nil || *s != *o {
		t.Fatalf("session %v %v", s, o)
	}
	if got, err := peerSubject(facts, uintptr(handle), true); err != nil || got != "sid:"+own {
		t.Fatalf("%q %v", got, err)
	}
}

func TestOwnSessionAndElevationAreReadFromTheRealToken(t *testing.T) {
	if sessionOf(uint32(os.Getpid())) == nil {
		t.Fatal("own session unreadable")
	}
	own := uint32(os.Getpid())
	elevated, known := home.ProcessElevation(&own)
	self, selfKnown := home.ProcessElevation(nil)
	if !known || !selfKnown || elevated != self || self != home.ProcessIsElevated() {
		t.Fatalf("%v %v %v %v", elevated, known, self, selfKnown)
	}
}

func TestASameTokenOwnerIsNotMisnamed(t *testing.T) {
	child := exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >NUL")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if reason, _, ok, err := probeRefusedOwner(child.Process.Pid); err != nil || ok {
		t.Fatalf("%q %v %v", reason, ok, err)
	}
}

// First process above pid 4 that tasklist reports in session 0
func sessionZeroPid() (int, bool) {
	out, _ := exec.Command("tasklist", "/fo", "csv", "/nh", "/fi", "SESSION eq 0").Output()
	rows, _ := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	for _, row := range rows {
		if len(row) > 1 {
			if pid, err := strconv.Atoi(row[1]); err == nil && pid > 4 {
				return pid, true
			}
		}
	}
	return 0, false
}

func TestASessionZeroOwnerReadsAsAnotherLogonSession(t *testing.T) {
	if s := sessionOf(uint32(os.Getpid())); s != nil && *s == 0 {
		t.Skip("this runner itself runs in session 0")
	}
	pid, ok := sessionZeroPid()
	if !ok {
		t.Skip("no session-0 process visible")
	}
	reason, remedy, named, err := probeRefusedOwner(pid)
	if err != nil || !named || reason != "gateway_other_session" {
		t.Fatalf("%q %v %v", reason, named, err)
	}
	if !strings.Contains(remedy, fmt.Sprintf("PID %d", pid)) || !strings.Contains(remedy, "session 0") {
		t.Fatalf("%q", remedy)
	}
}
