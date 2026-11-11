package tuiserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botdesktop"
	_ "github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Child mode of the test binary, a lease transition or a sleeping launcher stand in for another process
func runTestChild() {
	switch os.Getenv("ICEBATH_TEST_CHILD") {
	case "":
		return
	case "lease-acquire":
		if _, err := botdesktop.LeaseAcquire(os.Getenv("ICEBATH_TEST_VIEWER"), "", os.Getenv("ICEBATH_TEST_REASON")); err != nil {
			os.Exit(1)
		}
	case "lease-release":
		viewer := os.Getenv("ICEBATH_TEST_VIEWER")
		if _, err := botdesktop.LeaseRelease(botdesktop.LeaseReleaseOptions{ViewerID: &viewer}); err != nil {
			os.Exit(1)
		}
	case "sleep":
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

// Runs a lease transition in another process bound to home
func otherProcess(t *testing.T, h, mode, viewer, reason string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+h, "ICEBATH_TEST_CHILD="+mode, "ICEBATH_TEST_VIEWER="+viewer, "ICEBATH_TEST_REASON="+reason)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("other process %s: %v %s", mode, err, out)
	}
}

// Live stand in launcher process whose launcher.pid line matches its real start time
func startLauncher(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "ICEBATH_TEST_CHILD=sleep")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	born, err := proc.Birth(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, strconv.Itoa(cmd.Process.Pid) + " " + strconv.FormatFloat(born, 'f', -1, 64)
}

// Polls pred until it holds or the timeout passes
func waitFor(pred func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pred()
}

// One global event a fake broadcast recorded
type recordedEvent struct {
	name    string
	payload map[string]any
}

// Recorder of every global event the watcher broadcasts
type eventLog struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (l *eventLog) all() []recordedEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedEvent(nil), l.events...)
}

// Payloads of display.lease events whose lease carries every wanted field
func (l *eventLog) leaseEvents(want map[string]any) []map[string]any {
	var out []map[string]any
	for _, ev := range l.all() {
		if ev.name != "display.lease" {
			continue
		}
		lease := ev.payload["lease"].(*pyjson.Object)
		match := true
		for k, v := range want {
			if lease.GetOr(k, nil) != v {
				match = false
			}
		}
		if match {
			out = append(out, ev.payload)
		}
	}
	return out
}

// Payloads of every display.status event
func (l *eventLog) statuses() []map[string]any {
	var out []map[string]any
	for _, ev := range l.all() {
		if ev.name == "display.status" {
			out = append(out, ev.payload)
		}
	}
	return out
}

// Watches home as the launch home with a recording broadcast, waiting until the watcher seeded it
func watching(t *testing.T, h string) *eventLog {
	t.Helper()
	log := &eventLog{}
	prevBroadcast := broadcastGlobalEvent
	servedProfileMu.Lock()
	prevHome := hermesHome
	hermesHome = h
	broadcastGlobalEvent = func(event string, payload map[string]any) {
		log.mu.Lock()
		log.events = append(log.events, recordedEvent{event, payload})
		log.mu.Unlock()
	}
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		servedProfileMu.Lock()
		hermesHome, broadcastGlobalEvent = prevHome, prevBroadcast
		servedProfileMu.Unlock()
	})
	ensureLeaseWatcher()
	if !waitFor(func() bool {
		leaseWatchMu.Lock()
		defer leaseWatchMu.Unlock()
		_, seeded := leaseEpochs[home.Key(h)]
		return seeded
	}, 3*time.Second) {
		t.Fatal("watcher never seeded the home")
	}
	return log
}

func TestTakeoverInAnotherProcessIsBroadcast(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	events := watching(t, h)

	otherProcess(t, h, "lease-acquire", "cli-viewer", "probe")

	if !waitFor(func() bool { return len(events.leaseEvents(map[string]any{"reason": "probe"})) > 0 }, 3*time.Second) {
		t.Fatalf("events %v", events.all())
	}
	probe := events.leaseEvents(map[string]any{"reason": "probe"})
	if len(probe) != 1 || probe[0]["profile_key"] != home.Key(h) {
		t.Fatalf("probe events %v", probe)
	}
}

func TestReleaseInAnotherProcessIsBroadcastAndLocalTransitionNotDuplicated(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	events := watching(t, h)
	installLeaseListener()

	if _, err := botdesktop.LeaseAcquire("viewer-1", h, ""); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return len(events.leaseEvents(map[string]any{"holder": "human"})) > 0 }, 3*time.Second) {
		t.Fatalf("events %v", events.all())
	}
	before := len(events.all())
	pollLeaseFiles()
	if len(events.all()) != before {
		t.Fatalf("local transition re-broadcast %v", events.all())
	}

	otherProcess(t, h, "lease-release", "viewer-1", "")

	if !waitFor(func() bool { return len(events.leaseEvents(map[string]any{"holder": "agent"})) > 0 }, 3*time.Second) {
		t.Fatalf("events %v", events.all())
	}
	if got := events.leaseEvents(map[string]any{"holder": "agent"}); len(got) != 1 {
		t.Fatalf("agent events %v", got)
	}
}

func TestScreenStartedOrStoppedByAnotherProcessIsBroadcastAsStatus(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(h, "bot-desktop"), 0o700); err != nil {
		t.Fatal(err)
	}
	events := watching(t, h)
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	if len(events.statuses()) != 0 {
		t.Fatalf("seed broadcast %v", events.statuses())
	}

	writeFile(t, filepath.Join(h, "bot-desktop", "launcher.pid"), "424242 1.5")
	writeFile(t, filepath.Join(h, "bot-desktop", "env"), "DISPLAY=:77\n")
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	statuses := events.statuses()
	if len(statuses) == 0 || statuses[len(statuses)-1]["profile_key"] != h {
		t.Fatalf("statuses %v", statuses)
	}

	if err := os.Remove(filepath.Join(h, "bot-desktop", "env")); err != nil {
		t.Fatal(err)
	}
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	statuses = events.statuses()
	if len(statuses) != 2 || statuses[1]["running"] != false {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestLauncherDyingWithoutTouchingItsFilesIsBroadcastAsStopped(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(h, "bot-desktop"), 0o700); err != nil {
		t.Fatal(err)
	}
	launcher, line := startLauncher(t)
	writeFile(t, filepath.Join(h, "bot-desktop", "launcher.pid"), line)
	writeFile(t, filepath.Join(h, "bot-desktop", "env"), "DISPLAY=:77\n")
	events := watching(t, h)
	for range 2 {
		if err := pollRuntimeFiles(); err != nil {
			t.Fatal(err)
		}
	}
	if len(events.statuses()) != 0 {
		t.Fatalf("unchanged state was re-broadcast %v", events.statuses())
	}

	launcher.Process.Kill()
	launcher.Wait()
	for range 2 {
		if err := pollRuntimeFiles(); err != nil {
			t.Fatal(err)
		}
	}
	statuses := events.statuses()
	if len(statuses) != 1 || statuses[0]["running"] != false {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestScreenStartAndStopReachTheModelsTurnNotes(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(h, "bot-desktop"), 0o700); err != nil {
		t.Fatal(err)
	}
	events := watching(t, h)
	mine, other := testAgent(t, agent.AIAgentOptions{}), testAgent(t, agent.AIAgentOptions{})
	otherHome := filepath.Join(root, "other")
	if err := os.Mkdir(otherHome, 0o700); err != nil {
		t.Fatal(err)
	}
	tuigateway.SetSessionRecord("ws-a", tuigateway.NewSession(map[string]any{"agent": mine, "profile_home": h, "transport": tuigateway.DetachedWSTransport()}))
	tuigateway.SetSessionRecord("ws-b", tuigateway.NewSession(map[string]any{"agent": other, "profile_home": otherHome, "transport": tuigateway.DetachedWSTransport()}))
	t.Cleanup(func() {
		tuigateway.SetSessionRecord("ws-a", nil)
		tuigateway.SetSessionRecord("ws-b", nil)
	})
	_, line := startLauncher(t)
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	if mine.GatewayTurnContextNotes() != "" {
		t.Fatalf("seed staged %q", mine.GatewayTurnContextNotes())
	}

	writeFile(t, filepath.Join(h, "bot-desktop", "launcher.pid"), line)
	writeFile(t, filepath.Join(h, "bot-desktop", "env"), "DISPLAY=:77\n")
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	note := mine.GatewayTurnContextNotes()
	if !strings.Contains(note, "Bot Screen") || !strings.Contains(note, "display :77") || !strings.Contains(note, "RUNNING") {
		t.Fatalf("note %q", note)
	}
	if other.GatewayTurnContextNotes() != "" {
		t.Fatal("another profile's session was told")
	}

	if err := os.Remove(filepath.Join(h, "bot-desktop", "env")); err != nil {
		t.Fatal(err)
	}
	if err := pollRuntimeFiles(); err != nil {
		t.Fatal(err)
	}
	if note := mine.GatewayTurnContextNotes(); !strings.Contains(note, "no longer running") {
		t.Fatalf("note %q", note)
	}
	if len(events.statuses()) == 0 {
		t.Fatal("the broadcast never happened")
	}
}

// Writes text to a file the test owns
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
