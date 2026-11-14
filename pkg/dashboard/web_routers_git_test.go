package dashboard

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func init() {
	dir := os.Getenv("ICEBATH_FAKE_GH_PROBE_DIR")
	if dir == "" {
		return
	}
	_, err := os.Stat(filepath.Join(dir, "logged_in"))
	loggedIn := err == nil
	appendProbeLog(dir, "start\n")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(3)
		}
		time.Sleep(5 * time.Millisecond)
	}
	appendProbeLog(dir, "end\n")
	if loggedIn {
		os.Exit(0)
	}
	os.Exit(1)
}

// Appends one line to the fake gh probe log
func appendProbeLog(dir, line string) {
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(4)
	}
	f.WriteString(line)
	f.Close()
}

// A gh on PATH that blocks until released and answers logged in when the marker exists at its start
func fakeBlockingGh(t *testing.T) string {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name = "gh.exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(bin, name)
	if err := os.Link(self, target); err != nil {
		src, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
		dst.Close()
	}
	t.Setenv("PATH", bin)
	t.Setenv("ICEBATH_FAKE_GH_PROBE_DIR", state)
	ghAuth.mu.Lock()
	ghAuth.cached, ghAuth.cachedAt, ghAuth.probe = nil, 0, nil
	ghAuth.mu.Unlock()
	t.Cleanup(func() {
		ghAuth.mu.Lock()
		ghAuth.cached, ghAuth.cachedAt, ghAuth.probe = nil, 0, nil
		ghAuth.mu.Unlock()
	})
	return state
}

// Probe runs and their peak overlap read from the fake gh log
func probeCounts(t *testing.T, state string) (int, int) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(state, "log"))
	calls, running, peak := 0, 0, 0
	for _, line := range strings.Fields(string(raw)) {
		if line == "start" {
			calls++
			running++
			peak = max(peak, running)
		} else {
			running--
		}
	}
	return calls, peak
}

// Waits until the fake gh has started a probe
func waitProbeStarted(t *testing.T, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _ := probeCounts(t, state); calls > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gh probe never started")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// JSON text of a probe answer
func ghAnswer(t *testing.T, v any) string {
	t.Helper()
	text, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestGhAuthConcurrentCacheMissesShareOneProbe(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	state := fakeBlockingGh(t)
	results := make(chan any, 5)
	for range 5 {
		go func() {
			payload, err := ghAuthStatus(context.Background(), false)
			if err != nil {
				t.Error(err)
			}
			results <- payload
		}()
	}
	waitProbeStarted(t, state)
	if err := os.WriteFile(filepath.Join(state, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if got := ghAnswer(t, <-results); got != `{"available": true, "authenticated": false}` {
			t.Fatal(got)
		}
	}
	if calls, peak := probeCounts(t, state); calls != 1 || peak != 1 {
		t.Fatalf("calls %d peak %d", calls, peak)
	}
}

func TestGhAuthRefreshWaitsOutAProbeStartedBeforeIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	state := fakeBlockingGh(t)
	ask := func(refresh bool) chan any {
		out := make(chan any, 1)
		go func() {
			payload, err := ghAuthStatus(context.Background(), refresh)
			if err != nil {
				t.Error(err)
			}
			out <- payload
		}()
		return out
	}
	stale := ask(false)
	waitProbeStarted(t, state)
	if err := os.WriteFile(filepath.Join(state, "logged_in"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	refreshed := ask(true)
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(state, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	staleAnswer, refreshedAnswer := ghAnswer(t, <-stale), ghAnswer(t, <-refreshed)
	cached, err := ghAuthStatus(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if staleAnswer != `{"available": true, "authenticated": false}` {
		t.Fatal(staleAnswer)
	}
	if refreshedAnswer != `{"available": true, "authenticated": true}` {
		t.Fatal(refreshedAnswer)
	}
	if got := ghAnswer(t, cached); got != `{"available": true, "authenticated": true}` {
		t.Fatal(got)
	}
	if calls, peak := probeCounts(t, state); calls != 2 || peak != 1 {
		t.Fatalf("calls %d peak %d", calls, peak)
	}
}
