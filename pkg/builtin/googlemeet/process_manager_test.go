package googlemeet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestStartRefusesUnsafeURL(t *testing.T) {
	isolateHome(t)
	res, err := pmStart(context.Background(), "https://evil.example.com/abc-defg-hij", newStartOptions())
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := res.Get("ok")
	msg, _ := res.Get("error")
	if ok != false || !strings.Contains(msg.(string), "refusing") {
		t.Fatal(res)
	}
}

func TestStatusReportsNoActiveMeeting(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	st, _ := pmStatus(ctx)
	tr, _ := pmTranscript(ctx, 0)
	sp, _ := pmStop(ctx, "requested")
	for _, r := range []*pyjson.Object{st, tr, sp} {
		if ok, _ := r.Get("ok"); ok != false {
			t.Fatal(r)
		}
	}
}

func TestTranscriptReadsLastNLines(t *testing.T) {
	h := isolateHome(t)
	ctx := context.Background()
	dir := filepath.Join(h, "workspace", "meetings", "abc-defg-hij")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "transcript.txt"), []byte("[10:00:00] Alice: one\n[10:00:01] Bob: two\n[10:00:02] Alice: three\n"), 0o644)
	if err := writeActive(ctx, pyjson.ObjectOf("pid", 0, "meeting_id", "abc-defg-hij", "out_dir", dir, "url", "https://meet.google.com/abc-defg-hij", "started_at", 0)); err != nil {
		t.Fatal(err)
	}
	res, err := pmTranscript(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := res.Get("ok")
	total, _ := res.Get("total")
	lines, _ := res.Get("lines")
	l := lines.([]any)
	if ok != true || total != 3 || len(l) != 2 || !strings.HasSuffix(l[1].(string), "Alice: three") {
		t.Fatal(res)
	}
}

func TestStopSignalsProcessAndClearsPointer(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	_ = writeActive(ctx, pyjson.ObjectOf("pid", 11111, "meeting_id", "x-y-z", "out_dir", filepath.Join(t.TempDir(), "x-y-z"), "url", "https://meet.google.com/x-y-z", "started_at", 0))
	seq := []bool{true, true, false}
	var sent [][2]int
	ra, rk, rs := pidAlive, killProcess, pmSleep
	defer func() { pidAlive, killProcess, pmSleep = ra, rk, rs }()
	pidAlive = func(int) bool {
		if len(seq) == 0 {
			return false
		}
		v := seq[0]
		seq = seq[1:]
		return v
	}
	killProcess = func(pid int, sig syscall.Signal) error {
		sent = append(sent, [2]int{pid, int(sig)})
		return nil
	}
	pmSleep = func(time.Duration) {}
	res, err := pmStop(ctx, "requested")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := res.Get("ok"); ok != true {
		t.Fatal(res)
	}
	if len(sent) != 1 || sent[0] != [2]int{11111, int(syscall.SIGTERM)} {
		t.Fatal(sent)
	}
	if readActive(ctx) != nil {
		t.Fatal("active pointer kept")
	}
}

func TestEnqueueSayRequiresText(t *testing.T) {
	isolateHome(t)
	for _, text := range []string{"", "   "} {
		res, _ := pmEnqueueSay(context.Background(), text)
		if ok, _ := res.Get("ok"); ok != false {
			t.Fatal(res)
		}
	}
}

// Swaps the bot spawn for a recorder of the child env
func captureSpawn(t *testing.T, pid int) *[]string {
	t.Helper()
	var env []string
	restore := botPopen
	t.Cleanup(func() { botPopen = restore })
	botPopen = func(argv, e []string, _ *os.File) (int, error) {
		if argv[1] != MeetBotEntry {
			t.Errorf("argv %v", argv)
		}
		env = e
		return pid, nil
	}
	return &env
}

// Value of name in a KEY=VALUE environment list
func envValue(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

func TestChildEnvCarriesScopedOpenAIKey(t *testing.T) {
	isolateHome(t)
	t.Setenv("OPENAI_API_KEY", "")
	os.Unsetenv("OPENAI_API_KEY")
	t.Setenv("ICEBATH_MEET_REALTIME_KEY", "")
	os.Unsetenv("ICEBATH_MEET_REALTIME_KEY")
	root := t.TempDir()
	restore := pmRoot
	defer func() { pmRoot = restore }()
	pmRoot = func(context.Context) string { return root }
	env := captureSpawn(t, 4242)
	ctx := provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), map[string]string{"OPENAI_API_KEY": "scoped-openai-key"}, root)
	o := newStartOptions()
	o.OutDir, o.Mode = filepath.Join(root, "meeting"), "realtime"
	res, err := pmStart(ctx, "https://meet.google.com/abc-defg-hij", o)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := res.Get("ok"); ok != true {
		t.Fatal(res)
	}
	if got := envValue(*env, "ICEBATH_MEET_REALTIME_KEY"); got != "scoped-openai-key" {
		t.Fatal(got)
	}
}

func TestExplicitKeyArgumentStillWins(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	restore := pmRoot
	defer func() { pmRoot = restore }()
	pmRoot = func(context.Context) string { return root }
	env := captureSpawn(t, 4243)
	ctx := provider.WithSecretScope(provider.WithMultiplexContext(context.Background(), true), map[string]string{"OPENAI_API_KEY": "scoped-openai-key"}, root)
	o := newStartOptions()
	o.OutDir, o.RealtimeAPIKey = filepath.Join(root, "meeting2"), "explicit-key"
	if _, err := pmStart(ctx, "https://meet.google.com/abc-defg-hij", o); err != nil {
		t.Fatal(err)
	}
	if got := envValue(*env, "ICEBATH_MEET_REALTIME_KEY"); got != "explicit-key" {
		t.Fatal(got)
	}
}
