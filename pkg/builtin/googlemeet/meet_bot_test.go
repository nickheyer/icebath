package googlemeet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsSafeMeetURLAcceptsStandardMeetCodes(t *testing.T) {
	for _, u := range []string{"https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij?pli=1",
		"https://meet.google.com/new", "https://meet.google.com/lookup/ABC123"} {
		if !isSafeMeetURL(u) {
			t.Errorf("%s refused", u)
		}
	}
	for _, u := range []string{"https://evil.example.com/abc-defg-hij", "http://meet.google.com/abc-defg-hij", "https://meet.google.com/ab"} {
		if isSafeMeetURL(u) {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestMeetingIDExtraction(t *testing.T) {
	if got := meetingIDFromURL("https://meet.google.com/abc-defg-hij"); got != "abc-defg-hij" {
		t.Fatal(got)
	}
	if got := meetingIDFromURL("https://meet.google.com/abc-defg-hij?pli=1"); got != "abc-defg-hij" {
		t.Fatal(got)
	}
	if got := meetingIDFromURL("https://meet.google.com/new"); !strings.HasPrefix(got, "meet-") {
		t.Fatal(got)
	}
}

func TestBotStateDedupesCaptionsAndFlushesStatus(t *testing.T) {
	out := filepath.Join(t.TempDir(), "session")
	state, err := newBotState(context.Background(), out, "abc-defg-hij", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"Alice", "Hey everyone"}, {"Alice", "Hey everyone"}, {"Bob", "Let's start"}} {
		if err := state.recordCaption(context.Background(), c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(out, "transcript.txt"))
	transcript := string(raw)
	if !strings.Contains(transcript, "Alice: Hey everyone") || !strings.Contains(transcript, "Bob: Let's start") {
		t.Fatal(transcript)
	}
	if strings.Count(transcript, "Alice: Hey everyone") != 1 {
		t.Fatal(transcript)
	}
	var status map[string]any
	raw, _ = os.ReadFile(filepath.Join(out, "status.json"))
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status["meetingId"] != "abc-defg-hij" || status["transcriptLines"] != 2.0 || !strings.HasSuffix(status["transcriptPath"].(string), "transcript.txt") {
		t.Fatal(status)
	}
}

func TestParseDuration(t *testing.T) {
	for raw, want := range map[string]float64{"30m": 1800, "2h": 7200, "90s": 90, "90": 90} {
		if got, ok := parseDuration(raw); !ok || got != want {
			t.Errorf("%s -> %v %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"", "bogus"} {
		if _, ok := parseDuration(raw); ok {
			t.Errorf("%q parsed", raw)
		}
	}
}

func TestLooksLikeHumanSpeaker(t *testing.T) {
	for _, s := range []string{"", "   ", "Unknown", "unknown", "You", "you", "Icebath Agent", "icebath agent"} {
		if looksLikeHumanSpeaker(s, "Icebath Agent") {
			t.Errorf("%q should NOT be human", s)
		}
	}
	for _, s := range []string{"Alice", "Bob Lee", "@teknium"} {
		if !looksLikeHumanSpeaker(s, "Icebath Agent") {
			t.Errorf("%q SHOULD be human", s)
		}
	}
}

// Page whose every evaluation fails
type failingPage struct{ *fakePage }

func (failingPage) Evaluate(string) (any, error) { return nil, errors.New("boom") }

func TestDetectAdmissionReturnsFalseOnError(t *testing.T) {
	if probe(failingPage{&fakePage{}}, admissionProbeJS) {
		t.Fatal("probe true on error")
	}
}

// Locator stand in visible while present, recording clicks on its page
type toggle struct {
	page    *fakePage
	present func() bool
	label   string
}

func (l *toggle) First() meetLocator { return l }
func (l *toggle) Count() (int, error) {
	if l.present() {
		return 1, nil
	}
	return 0, nil
}
func (l *toggle) IsVisible() (bool, error) { return l.present(), nil }
func (l *toggle) Click(time.Duration) error {
	l.page.clicked = append(l.page.clicked, l.label)
	return nil
}
func (l *toggle) Fill(string, time.Duration) error { return nil }

// Meet page stand in with a late Join now button and a mic toggle
type fakePage struct {
	readyAt  time.Time
	micMuted bool
	stop     *atomic.Bool
	clicked  []string
}

func (p *fakePage) Locator(sel string) meetLocator {
	muted := strings.Contains(sel, "Turn on microphone")
	return &toggle{page: p, present: func() bool { return p.micMuted == muted }, label: sel}
}

func (p *fakePage) GetByRole(role, name string, exact bool) meetLocator {
	return &toggle{page: p, present: func() bool { return name == "Join now" && !time.Now().Before(p.readyAt) }, label: name}
}

func (p *fakePage) Evaluate(js string) (any, error) {
	if js == admissionProbeJS {
		return true, nil
	}
	p.stop.Store(true)
	return []any{}, nil
}

func (p *fakePage) IsClosed() bool { return false }

func TestJoinPollsForLateButtonAndAdmissionUnmutesMic(t *testing.T) {
	dir := t.TempDir()
	admitted := func(micMuted bool) (*fakePage, *botState) {
		stop := &atomic.Bool{}
		page := &fakePage{micMuted: micMuted, stop: stop}
		state, err := newBotState(context.Background(), filepath.Join(dir, map[bool]string{true: "True", false: "False"}[micMuted]), "abc-defg-hij", "https://meet.google.com/abc-defg-hij")
		if err != nil {
			t.Fatal(err)
		}
		restore := botSleep
		botSleep = func(time.Duration) {}
		drainLoop(context.Background(), page, botConfig{GuestName: "Bot", LobbyTimeout: 30}, state, &realtimeRes{}, stop)
		botSleep = restore
		if state.get("inCall") != true {
			t.Fatal("not in call")
		}
		return page, state
	}
	state, err := newBotState(context.Background(), dir, "abc-defg-hij", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	page := &fakePage{readyAt: time.Now().Add(600 * time.Millisecond), micMuted: true, stop: &atomic.Bool{}}
	join(context.Background(), page, botConfig{GuestName: "Bot"}, state, 5*time.Second)
	if len(page.clicked) != 1 || page.clicked[0] != "Join now" {
		t.Fatal(page.clicked)
	}
	page, state = admitted(true)
	if state.get("micState") != "unmuted_clicked" {
		t.Fatal(state.get("micState"))
	}
	found := false
	for _, c := range page.clicked {
		found = found || strings.Contains(c, "Turn on microphone")
	}
	var status map[string]any
	raw, _ := os.ReadFile(state.statusPath)
	_ = json.Unmarshal(raw, &status)
	if !found || status["micState"] != "unmuted_clicked" {
		t.Fatal(page.clicked, status)
	}
	live, state := admitted(false)
	if state.get("micState") != "unmuted" || len(live.clicked) != 0 {
		t.Fatal(state.get("micState"), live.clicked)
	}
}

func TestPCMPumpReceivesAudioAppendedAfterStart(t *testing.T) {
	dir := t.TempDir()
	pcm, sink := filepath.Join(dir, "speaker.pcm"), filepath.Join(dir, "device.bin")
	if err := os.WriteFile(pcm, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sinkFile, err := os.Create(sink)
	if err != nil {
		t.Fatal(err)
	}
	defer sinkFile.Close()
	restore := pumpCommand
	defer func() { pumpCommand = restore }()
	pumpCommand = func(argv []string) *exec.Cmd {
		if argv[0] != "paplay" || argv[len(argv)-1] != "-" {
			t.Errorf("argv %v", argv)
		}
		cmd := exec.Command("cat")
		cmd.Stdout = sinkFile
		return cmd
	}
	rt, stop := &realtimeRes{}, &atomic.Bool{}
	state, err := newBotState(context.Background(), dir, "abc-defg-hij", "https://meet.google.com/abc-defg-hij")
	if err != nil {
		t.Fatal(err)
	}
	if err := startPCMPump(context.Background(), rt, map[string]any{"platform": "linux", "write_target": "sink"}, pcm, state, stop); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	f, _ := os.OpenFile(pcm, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.Write([]byte(strings.Repeat("\x01\x02", 2000)))
	f.Close()
	size := func() int64 {
		info, _ := os.Stat(sink)
		return info.Size()
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && size() < 4000; {
		time.Sleep(50 * time.Millisecond)
	}
	if !rt.pcmPump.Alive() || size() != 4000 {
		t.Fatal(rt.pcmPump.Alive(), size())
	}
	stop.Store(true)
	teardownRealtime(rt)
	select {
	case <-rt.pcmTailDone:
	default:
		t.Fatal("tail goroutine still running")
	}
	raw, _ := os.ReadFile(state.statusPath)
	if state.get("micState") != nil || !strings.Contains(string(raw), "micState") {
		t.Fatal(string(raw))
	}
}

// Pump stand in whose stdin write fails with err
type failingPump struct{ err error }

func (p failingPump) Alive() bool           { return true }
func (p failingPump) Stdin() io.WriteCloser { return failingWriter(p) }

type failingWriter failingPump

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
func (w failingWriter) Close() error              { return nil }

func TestPCMTailLoopSwallowsOnlyPipeErrors(t *testing.T) {
	pcm := filepath.Join(t.TempDir(), "speaker.pcm")
	_ = os.WriteFile(pcm, make([]byte, 16), 0o644)
	if err := pcmTailLoop(failingPump{&os.PathError{Op: "write", Path: "|1", Err: errors.New("broken pipe")}}, pcm, &atomic.Bool{}, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := pcmTailLoop(failingPump{errors.New("bug")}, pcm, &atomic.Bool{}, 50*time.Millisecond); err == nil {
		t.Fatal("non pipe error swallowed")
	}
}

func TestRealtimeSessionCancelResponseWhenDisconnected(t *testing.T) {
	if newRealtimeSession("sk-test", "gpt-realtime", "alloy", "", "", 24000).cancelResponse() {
		t.Fatal("cancel without a socket")
	}
}
