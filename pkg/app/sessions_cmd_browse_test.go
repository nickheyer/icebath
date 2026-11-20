package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Screen of thirty rows and a hundred twenty columns replaying a key sequence, colors off
type browseFakeScreen struct {
	keys  []int
	lines map[int]string
}

func (f *browseFakeScreen) Getch() (int, error) {
	if len(f.keys) == 0 {
		return 0, errors.New("key sequence exhausted")
	}
	key := f.keys[0]
	f.keys = f.keys[1:]
	return key, nil
}
func (f *browseFakeScreen) Timeout(int)          {}
func (f *browseFakeScreen) Clear()               { f.lines = map[int]string{} }
func (f *browseFakeScreen) GetMaxYX() (int, int) { return 30, 120 }
func (f *browseFakeScreen) AddNStr(y, x int, text string, n int, _ cliui.Attr) error {
	f.lines[y] = text
	return nil
}
func (f *browseFakeScreen) Refresh() error              { return nil }
func (f *browseFakeScreen) HasColors() bool             { return false }
func (f *browseFakeScreen) InitColors(bool) error       { return nil }
func (f *browseFakeScreen) InitPairs(map[int]int) error { return nil }
func (f *browseFakeScreen) Colors() int                 { return 0 }

// Rich session rows the way the listing returns them, every third one untitled
func makeBrowseSessions(n int) []*pyjson.Object {
	now := float64(time.Now().UnixNano()) / 1e9
	var sessions []*pyjson.Object
	for i := range n {
		source, title := "telegram", any(fmt.Sprintf("Session %d", i))
		if i%2 == 0 {
			source = "cli"
		}
		if i%3 == 0 {
			title = nil
		}
		sessions = append(sessions, pyjson.ObjectOf("id", fmt.Sprintf("20260308_%06d_abcdef", i), "source", source, "model", "test/model",
			"title", title, "preview", fmt.Sprintf("Hello from session %d", i), "last_active", now-float64(i)*3600,
			"started_at", now-float64(i)*3600-60, "message_count", (i+1)*5))
	}
	return sessions
}

// Runs the picker with full screen support absent, the answer typed on stdin
func browseFallback(t *testing.T, sessions []*pyjson.Object, stdin string) (string, string) {
	t.Helper()
	r, out := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, stdin)
	console().OpenScreen = func() (cliui.Screen, func(), error) { return nil, nil, cliui.ErrCursesInit }
	selected, err := sessionBrowsePicker(r, sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	return selected, out.String()
}

// Runs the full screen picker over a simulated key sequence
func browseWithKeys(t *testing.T, sessions []*pyjson.Object, keys []int) string {
	t.Helper()
	r, _ := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, "")
	screen := &browseFakeScreen{keys: keys, lines: map[int]string{}}
	console().OpenScreen = func() (cliui.Screen, func(), error) { return screen, func() {}, nil }
	selected, err := sessionBrowsePicker(r, sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen.lines[0], "Browse sessions") {
		t.Fatalf("the picker never drew its header: %v", screen.lines)
	}
	return selected
}

func TestSessionBrowsePickerEmptySessionsReturnsNone(t *testing.T) {
	r, out := sessionsTestRun(t, sessionsTestHome(t), map[string]any{}, "")
	selected, err := sessionBrowsePicker(r, nil, nil)
	if err != nil || selected != "" || !strings.Contains(out.String(), "No sessions found") {
		t.Fatalf("selected %q err %v output %s", selected, err, out.String())
	}
}

func TestSessionBrowsePickerFallbackModeValidSelection(t *testing.T) {
	sessions := makeBrowseSessions(3)
	selected, _ := browseFallback(t, sessions, "2\n")
	if want, _ := sessions[1].Get("id"); selected != want {
		t.Fatalf("selected %q, want %v", selected, want)
	}
}

func TestSessionBrowsePickerFallbackShowsPreviewWhenNoTitle(t *testing.T) {
	sessions := []*pyjson.Object{pyjson.ObjectOf("id", "test_002", "source", "cli", "title", nil,
		"preview", "Hello world test message", "last_active", float64(time.Now().Unix()))}
	if _, out := browseFallback(t, sessions, "q\n"); !strings.Contains(out, "Hello world test message") {
		t.Fatalf("output %s", out)
	}
}

func TestCursesBrowseEscapeCancels(t *testing.T) {
	if selected := browseWithKeys(t, makeBrowseSessions(3), []int{27}); selected != "" {
		t.Fatalf("selected %q", selected)
	}
}

func TestCursesBrowseTypeToFilterThenEnter(t *testing.T) {
	now := float64(time.Now().Unix())
	sessions := []*pyjson.Object{
		pyjson.ObjectOf("id", "s1", "source", "cli", "title", "Alpha project", "preview", "", "last_active", now),
		pyjson.ObjectOf("id", "s2", "source", "cli", "title", "Beta project", "preview", "", "last_active", now),
		pyjson.ObjectOf("id", "s3", "source", "cli", "title", "Gamma project", "preview", "", "last_active", now),
	}
	keys := []int{'B', 'e', 't', 'a', 10}
	if selected := browseWithKeys(t, sessions, keys); selected != "s2" {
		t.Fatalf("selected %q", selected)
	}
}

func TestEdgeCasesRelativeTimeFormatting(t *testing.T) {
	now := float64(time.Now().UnixNano()) / 1e9
	sessions := []*pyjson.Object{
		pyjson.ObjectOf("id", "recent", "source", "cli", "title", nil, "preview", "just now test", "last_active", now),
		pyjson.ObjectOf("id", "hour_ago", "source", "cli", "title", nil, "preview", "hour ago test", "last_active", now-7200),
		pyjson.ObjectOf("id", "days_ago", "source", "cli", "title", nil, "preview", "days ago test", "last_active", now-259200),
	}
	_, out := browseFallback(t, sessions, "q\n")
	for _, want := range []string{"just now", "2h ago", "3d ago"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q: %s", want, out)
		}
	}
}

func TestAnnotateSessionStatuses(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	ctx := context.Background()
	sessionsTestSeed(t, db, state.NewSession{ID: "s1", Source: "cli"}, "hi")
	if _, err := db.AppendMessage(ctx, "s1", map[string]any{"role": "assistant", "content": "hello", "finish_reason": "stop"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	sessionsTestSeed(t, db, state.NewSession{ID: "s2", Source: "cli"}, "hi")
	r, _ := sessionsTestRun(t, h, map[string]any{}, "")
	rows := []*pyjson.Object{pyjson.ObjectOf("id", "s1"), pyjson.ObjectOf("id", "s2")}
	annotateSessionStatuses(r, rows, db)
	if got, _ := rows[0].Get("_status"); got != state.SessionStatusComplete {
		t.Fatalf("s1 status %v", got)
	}
	if got, _ := rows[1].Get("_status"); got != state.SessionStatusInterrupted {
		t.Fatalf("s2 status %v", got)
	}
	bare := []*pyjson.Object{pyjson.ObjectOf("id", "s1")}
	annotateSessionStatuses(r, bare, nil)
	if _, ok := bare[0].Get("_status"); ok {
		t.Fatal("a row was annotated without a store")
	}
	if tag := sessionStatusTag(rowText(bare[0], "_status")); tag != "-" {
		t.Fatalf("bare tag %q", tag)
	}
	for status, want := range map[string]string{state.SessionStatusComplete: "done", state.SessionStatusInterrupted: "intr",
		state.SessionStatusError: "err", state.SessionStatusEmpty: "empty"} {
		if tag := sessionStatusTag(status); tag != want {
			t.Fatalf("tag of %q is %q, want %q", status, tag, want)
		}
	}
}
