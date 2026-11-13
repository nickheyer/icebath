package chrome_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/desktop/chrome"
)

type sent struct {
	name    string
	payload any
}

type fakeWindow struct {
	svc       *chrome.Service
	mu        sync.Mutex
	sent      []sent
	calls     []string
	destroyed []func()
	devtools  bool
	nextID    int
	early     bool
	name      string
	gone      bool
	onFind    func(id int)
}

func (w *fakeWindow) ID() string {
	if w.name != "" {
		return w.name
	}
	return "main"
}
func (w *fakeWindow) Send(name string, payload any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, sent{name, payload})
}
func (w *fakeWindow) OnDestroyed(f func())           { w.destroyed = append(w.destroyed, f) }
func (w *fakeWindow) IsDestroyed() bool              { return w.gone }
func (w *fakeWindow) EditingCommand(command string)  { w.calls = append(w.calls, "edit "+command) }
func (w *fakeWindow) CopyImageAt(x, y int)           { w.calls = append(w.calls, "copy-image") }
func (w *fakeWindow) ReplaceMisspelling(word string) { w.calls = append(w.calls, "replace "+word) }
func (w *fakeWindow) StopFindInPage()                { w.calls = append(w.calls, "stop") }
func (w *fakeWindow) DevToolsOpened() bool           { return w.devtools }
func (w *fakeWindow) OpenDevTools()                  { w.devtools = true }
func (w *fakeWindow) CloseDevTools()                 { w.devtools = false }

func (w *fakeWindow) FindInPage(query string, forward, findNext bool) (int, error) {
	w.nextID++
	id := w.nextID
	if query == "unsearchable" {
		return 0, errors.New("the webview has no find")
	}
	w.calls = append(w.calls, strings.Join([]string{"find", query, map[bool]string{true: "fwd", false: "back"}[forward], map[bool]string{true: "next", false: "new"}[findNext]}, " "))
	if w.onFind != nil {
		w.onFind(id)
	} else if w.early {
		w.svc.FoundInPage(w, chrome.FoundResult{RequestID: id, ActiveMatchOrdinal: 1, Matches: 3})
	} else {
		go w.svc.FoundInPage(w, chrome.FoundResult{RequestID: id, ActiveMatchOrdinal: 2, Matches: 3})
	}
	return id, nil
}

func (w *fakeWindow) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, s := range w.sent {
		out = append(out, s.name)
	}
	return out
}

type words struct{ added, removed []string }

func (d *words) AddWord(word string) bool    { d.added = append(d.added, word); return true }
func (d *words) RemoveWord(word string) bool { d.removed = append(d.removed, word); return true }

// Chrome service of a platform over a temp userData under a temp ICEBATH_HOME
func newChrome(t *testing.T, platform string) (*chrome.Service, *fakeWindow, chrome.Options, *[]string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := &[]string{}
	opts := chrome.Options{UserData: filepath.Join(t.TempDir(), "ud"), Platform: platform, Log: func(line string) { *logs = append(*logs, line) },
		Dictionary: &words{}, Guests: func(float64) chrome.Guest { return nil }}
	svc := chrome.NewService(opts)
	return svc, &fakeWindow{svc: svc}, opts, logs
}

func TestContextMenuEditRunsTheEditVerbOnTheSender(t *testing.T) {
	svc, w, _, _ := newChrome(t, "linux")
	for _, command := range []any{"copy", "cut", "paste", "selectAll", "constructor", "undo", nil, 1.0} {
		svc.ContextMenuEdit(w, command)
	}
	if got := strings.Join(w.calls, ","); got != "edit Copy,edit Cut,edit Paste,edit SelectAll" {
		t.Fatalf("editing commands %q", got)
	}
}

func TestSetDisableF12PersistsTheSettingAndBlocksOnlyF12(t *testing.T) {
	svc, w, opts, logs := newChrome(t, "linux")
	f12 := chrome.KeyInput{Type: "keyDown", Key: "F12", Code: "F12"}
	if !svc.BeforeInputEvent(w, f12) || !w.devtools {
		t.Fatal("F12 did not open DevTools before the setting")
	}
	svc.BeforeInputEvent(w, f12)
	if w.devtools {
		t.Fatal("a second F12 did not close DevTools")
	}
	svc.SetDisableF12(true)
	raw, err := os.ReadFile(filepath.Join(opts.UserData, "disable-f12.json"))
	if err != nil || string(raw) != "{\n  \"on\": true\n}" {
		t.Fatalf("disable-f12.json %q %v", raw, err)
	}
	if !svc.BeforeInputEvent(w, f12) || w.devtools {
		t.Fatal("disabled F12 was not swallowed or still opened DevTools")
	}
	if svc.BeforeInputEvent(w, chrome.KeyInput{Type: "keyUp", Key: "F12"}) || w.devtools {
		t.Fatal("an F12 keyup acted")
	}
	if !svc.BeforeInputEvent(w, chrome.KeyInput{Type: "keyDown", Key: "I", Control: true, Shift: true}) || !w.devtools {
		t.Fatal("Ctrl+Shift+I stopped toggling DevTools while F12 is disabled")
	}
	w.devtools = false
	svc.SetF12ShortcutActive(w, true)
	if !svc.BeforeInputEvent(w, chrome.KeyInput{Type: "keyDown", Key: "F12", Code: "F12", Shift: true, IsAutoRepeat: true}) || w.devtools {
		t.Fatal("an active renderer F12 binding did not take the key")
	}
	if got := strings.Join(w.names(), ","); got != "f12-shortcut" {
		t.Fatalf("events %q", got)
	}
	if p, ok := w.sent[0].payload.(chrome.F12KeyboardEvent); !ok || p != (chrome.F12KeyboardEvent{Code: "F12", Key: "F12", Shift: true, Repeat: true}) {
		t.Fatalf("f12-shortcut payload %#v", w.sent[0].payload)
	}
	svc.SetF12ShortcutActive(w, false)
	reopened := chrome.NewService(opts)
	if !reopened.BeforeInputEvent(w, f12) || w.devtools {
		t.Fatal("the persisted setting was not applied on start")
	}
	svc.SetDisableF12(false)
	if again := chrome.NewService(opts); !again.BeforeInputEvent(w, f12) || !w.devtools {
		t.Fatal("clearing the setting did not bring F12 back")
	}
	blocked := opts
	blocked.UserData = filepath.Join(opts.UserData, "disable-f12.json", "inner")
	chrome.NewService(blocked).SetDisableF12(true)
	if len(*logs) != 1 || !strings.HasPrefix((*logs)[0], "[disable-f12] write failed: ENOTDIR: not a directory, mkdir") {
		t.Fatalf("write failure log %q", *logs)
	}
}

func TestOnOpenFindBarRequestedFollowsCtrlFOnLinuxOnly(t *testing.T) {
	svc, w, _, _ := newChrome(t, "linux")
	for _, in := range []chrome.KeyInput{
		{Type: "keyDown", Key: "f", Control: true},
		{Type: "keyUp", Key: "F", Control: true},
	} {
		if !svc.BeforeInputEvent(w, in) {
			t.Fatalf("Ctrl+F %+v was not claimed", in)
		}
	}
	for _, in := range []chrome.KeyInput{
		{Type: "keyDown", Key: "f", Control: true, Shift: true},
		{Type: "keyDown", Key: "f", Control: true, Alt: true},
		{Type: "keyDown", Key: "f", Meta: true},
		{Type: "keyDown", Key: "g", Control: true},
	} {
		if svc.BeforeInputEvent(w, in) {
			t.Fatalf("%+v was claimed", in)
		}
	}
	if got := strings.Join(w.names(), ","); got != "open-find-bar,open-find-bar" {
		t.Fatalf("events %q", got)
	}
	for _, platform := range []string{"darwin", "win32"} {
		other, ow, _, _ := newChrome(t, platform)
		if other.BeforeInputEvent(ow, chrome.KeyInput{Type: "keyDown", Key: "f", Control: true}) || other.BeforeInputEvent(ow, chrome.KeyInput{Type: "keyDown", Key: "f", Meta: true}) || len(ow.names()) != 0 {
			t.Fatalf("Ctrl+F was claimed on %s", platform)
		}
	}
}

func TestChromeSpellingFindAndContextMenuLogic(t *testing.T) {
	svc, w, opts, _ := newChrome(t, "linux")
	dict := opts.Dictionary.(*words)
	svc.ContextMenuCopyImage(w)
	svc.ContextMenuShown(w, 10, 20, true, "teh", []string{"the", "ten"})
	svc.ContextMenuShown(w, 11, 21, false, "teh", nil)
	svc.ContextMenuCopyImage(w)
	svc.ContextMenuSpellcheck(w, map[string]any{"kind": "replace", "word": "the"})
	svc.ContextMenuSpellcheck(w, map[string]any{"kind": "add", "word": "icebath"})
	svc.ContextMenuSpellcheck(w, map[string]any{"kind": "add", "word": ""})
	if got := strings.Join(w.calls, ","); got != "copy-image,replace the" {
		t.Fatalf("window calls %q", got)
	}
	if got := strings.Join(w.names(), ","); got != "context-menu-spellcheck" {
		t.Fatalf("events %q", got)
	}
	if !svc.SpellcheckDictionary(map[string]any{"op": "remove", "word": " Cafe\u0301 "}) || svc.SpellcheckDictionary(map[string]any{"op": "add", "word": "two words"}) ||
		svc.SpellcheckDictionary(map[string]any{"op": "toString", "word": "fine"}) || svc.SpellcheckDictionary(map[string]any{"op": "add", "word": "1234"}) {
		t.Fatal("dictionary door verdicts")
	}
	if strings.Join(dict.added, ",") != "icebath" || len(dict.removed) != 1 || dict.removed[0] != "Caf\u00e9" {
		t.Fatalf("dictionary %+v", dict)
	}
	for _, f := range w.destroyed {
		f()
	}
}

func TestFindInPageAnswersOnceItsOwnRequestReportsAResult(t *testing.T) {
	svc, w, _, _ := newChrome(t, "linux")
	released := make(chan chrome.FindAck, 1)
	w.onFind = func(id int) {
		go func() {
			svc.FoundInPage(w, chrome.FoundResult{RequestID: id + 100, ActiveMatchOrdinal: 1, Matches: 9})
			select {
			case <-released:
				t.Error("a result of another request released the find")
			default:
			}
			svc.FoundInPage(w, chrome.FoundResult{RequestID: id, ActiveMatchOrdinal: 2, Matches: 3})
		}()
	}
	go func() {
		ack, err := svc.FindInPage(w, "needle", map[string]any{"forward": false})
		if err != nil {
			t.Error(err)
		}
		released <- ack
	}()
	if ack := <-released; ack != (chrome.FindAck{Count: 0}) {
		t.Fatalf("find ack %+v", ack)
	}
	w.onFind = func(id int) { svc.FoundInPage(w, chrome.FoundResult{RequestID: id, Matches: 0}) }
	svc.FindInPage(w, 7.0, nil)
	svc.FindInPage(w, "x", "not an object")
	svc.FindInPage(w, "y", map[string]any{"findNext": 1.0, "forward": nil})
	if got := strings.Join(w.calls, ","); got != "find needle back new,find 7 fwd new,find x fwd new,find y fwd next" {
		t.Fatalf("native finds %q", got)
	}
	if _, err := svc.FindInPage(w, nil, nil); err == nil || err.Error() != "Must provide a non-empty search content" {
		t.Fatalf("empty find answered %v", err)
	}
	if _, err := svc.FindInPage(w, "", map[string]any{}); err == nil {
		t.Fatal("an empty string find was accepted")
	}
	if _, err := svc.FindInPage(w, "unsearchable", nil); err == nil || err.Error() != "the webview has no find" {
		t.Fatalf("a webview that cannot search answered %v", err)
	}
	w.onFind = func(int) { go w.destroyed[0]() }
	if ack, err := svc.FindInPage(w, "never answered", nil); ack.Count != 0 || err != nil {
		t.Fatalf("a destroyed window kept the find waiting %+v %v", ack, err)
	}
	gone := &fakeWindow{svc: svc, name: "gone", gone: true}
	if ack, err := svc.FindInPage(gone, "needle", nil); ack.Count != 0 || err != nil || len(gone.calls) != 0 {
		t.Fatalf("a destroyed sender was searched %+v %v %q", ack, err, gone.calls)
	}
	if ack, err := svc.FindInPage(nil, "needle", nil); ack.Count != 0 || err != nil {
		t.Fatalf("a missing sender answered %+v %v", ack, err)
	}
}

func TestStopFindInPageClearsTheFindOfTheCallingWindow(t *testing.T) {
	svc, w, _, _ := newChrome(t, "linux")
	svc.StopFindInPage(w)
	gone := &fakeWindow{svc: svc, name: "gone", gone: true}
	svc.StopFindInPage(gone)
	svc.StopFindInPage(nil)
	if strings.Join(w.calls, ",") != "stop" || len(gone.calls) != 0 {
		t.Fatalf("stop calls %q %q", w.calls, gone.calls)
	}
}

func TestOnFoundInPageForwardsEveryResultToTheSearchingWindowOnly(t *testing.T) {
	svc, w, _, _ := newChrome(t, "linux")
	other := &fakeWindow{svc: svc, name: "second"}
	svc.FoundInPage(w, chrome.FoundResult{RequestID: 1, ActiveMatchOrdinal: 1, Matches: 1})
	if len(w.names()) != 0 {
		t.Fatalf("a result before any find was forwarded %q", w.names())
	}
	w.onFind = func(id int) { svc.FoundInPage(w, chrome.FoundResult{RequestID: id, ActiveMatchOrdinal: 2, Matches: 5}) }
	svc.FindInPage(w, "needle", nil)
	svc.FoundInPage(w, chrome.FoundResult{RequestID: 1, ActiveMatchOrdinal: 3, Matches: 5})
	svc.FoundInPage(other, chrome.FoundResult{RequestID: 1, ActiveMatchOrdinal: 4, Matches: 4})
	want := []chrome.FoundInPagePayload{{ActiveMatchOrdinal: 2, Count: 5}, {ActiveMatchOrdinal: 3, Count: 5}}
	if len(w.sent) != len(want) || len(other.sent) != 0 {
		t.Fatalf("forwarded %+v to the searcher and %+v to the other window", w.sent, other.sent)
	}
	for i, s := range w.sent {
		if s.name != "found-in-page" || s.payload != want[i] {
			t.Fatalf("event %d %+v", i, s)
		}
	}
	w.gone = true
	svc.FoundInPage(w, chrome.FoundResult{RequestID: 2, ActiveMatchOrdinal: 1, Matches: 1})
	w.gone = false
	for _, f := range w.destroyed {
		f()
	}
	svc.FoundInPage(w, chrome.FoundResult{RequestID: 3, ActiveMatchOrdinal: 1, Matches: 1})
	if len(w.sent) != 2 {
		t.Fatalf("a destroyed window got results %+v", w.sent)
	}
}

func TestFindIndexStepsTheActiveMatchLikeWebKit(t *testing.T) {
	var f chrome.FindIndex
	steps := []struct {
		query     string
		backwards bool
		count     int
		want      int
	}{
		{"a", false, 3, 1}, {"a", false, 3, 2}, {"a", false, 3, 3}, {"a", false, 3, 1},
		{"a", true, 3, 3}, {"a", true, 3, 2}, {"ab", true, 2, 1}, {"ab", false, 2, 2},
		{"ab", false, 1, 1}, {"b", true, 4, 1}, {"b", true, 4, 4},
	}
	for i, s := range steps {
		if got := f.Found(s.query, s.backwards, s.count); got != s.want {
			t.Fatalf("step %d %+v answered %d", i, s, got)
		}
	}
	f.Reset()
	if got := f.Found("b", false, 4); got != 1 {
		t.Fatalf("a find after reset answered %d", got)
	}
}
