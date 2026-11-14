package cliui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/nickheyer/icebath/pkg/term"

	"github.com/xo/terminfo"
)

// Replays queued key codes, minus one once the queue is empty unless exhausting
type fakeScreen struct {
	keys       []int
	exhausting bool
	timeouts   []int
	heights    []int
	height     int
	rows       map[int]string
	frames     []map[int]string
	initErr    error
}

func (f *fakeScreen) Getch() (int, error) {
	if len(f.keys) == 0 {
		if f.exhausting {
			return 0, errors.New("menu requested another key after the scripted keys")
		}
		return -1, nil
	}
	k := f.keys[0]
	f.keys = f.keys[1:]
	return k, nil
}
func (f *fakeScreen) Timeout(ms int) { f.timeouts = append(f.timeouts, ms) }
func (f *fakeScreen) Clear()         { f.rows = map[int]string{} }
func (f *fakeScreen) GetMaxYX() (int, int) {
	if f.height == 0 {
		f.height = 24
	}
	if len(f.heights) > 0 {
		f.height, f.heights = f.heights[0], f.heights[1:]
	}
	return f.height, 100
}
func (f *fakeScreen) AddNStr(y, x int, text string, n int, _ Attr) error {
	if y < 0 || y >= f.height {
		panic(fmt.Sprintf("off-screen write at row %d", y))
	}
	rs := []rune(text)
	if len(rs) > n {
		rs = rs[:max(n, 0)]
	}
	f.rows[y] += string(rs)
	return nil
}
func (f *fakeScreen) Refresh() error {
	frame := map[int]string{}
	for k, v := range f.rows {
		frame[k] = v
	}
	f.frames = append(f.frames, frame)
	return nil
}
func (f *fakeScreen) HasColors() bool             { return false }
func (f *fakeScreen) InitColors(bool) error       { return f.initErr }
func (f *fakeScreen) InitPairs(map[int]int) error { return f.initErr }
func (f *fakeScreen) Colors() int                 { return 0 }

func keys(s string) []int {
	out := make([]int, 0, len(s))
	for _, b := range []byte(s) {
		out = append(out, int(b))
	}
	return out
}

// Console over scripted input that claims a terminal on both streams
func testConsole(input string, scr Screen) (*Console, *bytes.Buffer) {
	out := &bytes.Buffer{}
	c := &Console{In: strings.NewReader(input), Out: out,
		Getenv:   func(string) (string, bool) { return "", false },
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true },
	}
	if scr != nil {
		c.OpenScreen = func() (Screen, func(), error) { return scr, func() {}, nil }
	}
	return c, out
}

func readKey(t *testing.T, ks []int) string {
	t.Helper()
	a, err := ReadMenuKey(&fakeScreen{keys: ks})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRawSS3ArrowKeysDecode(t *testing.T) {
	if readKey(t, keys("\x1bOA")) != NavUp || readKey(t, keys("\x1bOB")) != NavDown {
		t.Fatal("ss3 arrows")
	}
}

func TestLeftArrowDecodesToBack(t *testing.T) {
	if readKey(t, []int{KeyLeft}) != NavBack || readKey(t, keys("\x1b[D")) != NavBack || readKey(t, keys("\x1bOD")) != NavBack {
		t.Fatal("left arrow")
	}
}

func TestEnhancedKeyboardSequencesDecode(t *testing.T) {
	cases := map[string]string{
		"\x1b[13u": NavSelect, "\x1b[13;1u": NavSelect, "\x1b[27u": NavCancel, "\x1b[32u": NavToggle,
		"\x1b[99;5u": NavInterrupt, "\x1b[1;1D": NavBack, "\x1b[27;1;13~": NavSelect, "\x1b[27;5;99~": NavInterrupt,
		"\x1b[13;1:3u": NavNone,
	}
	for seq, want := range cases {
		if got := readKey(t, keys(seq)); got != want {
			t.Errorf("%q got %s want %s", seq, got, want)
		}
	}
}

func TestRawCtrlCCancels(t *testing.T) {
	if readKey(t, []int{3}) != NavInterrupt {
		t.Fatal("ctrl c")
	}
}

func TestCtrlDLeavesAMenuLikeEsc(t *testing.T) {
	for _, ks := range [][]int{{4}, keys("\x1b[100;5u"), keys("\x1b[27;5;100~")} {
		if got := readKey(t, ks); got != NavCancel {
			t.Errorf("%v got %s", ks, got)
		}
	}
}

func TestLoneEscapeIsCancel(t *testing.T) {
	f := &fakeScreen{keys: []int{27}}
	if a, _ := ReadMenuKey(f); a != NavCancel || !reflect.DeepEqual(f.timeouts, []int{60, -1}) {
		t.Fatalf("got %s timeouts %v", a, f.timeouts)
	}
}

func TestEnterVariantsSelect(t *testing.T) {
	for _, k := range []int{10, 13, KeyEnter} {
		if readKey(t, []int{k}) != NavSelect {
			t.Fatalf("%d", k)
		}
	}
}

func items(labels ...string) []RadioItem {
	out := make([]RadioItem, len(labels))
	for i, l := range labels {
		out[i] = PlainItem(l)
	}
	return out
}

func TestEnhancedEnterSelectsFilteredModelWhileSearchIsActive(t *testing.T) {
	f := &fakeScreen{keys: keys("/g\x1b[13u"), exhausting: true}
	c, _ := testConsole("", f)
	got, err := c.CursesRadiolist(context.Background(), "Pick", items("alpha", "gpt"), 0, RadiolistOptions{Searchable: true, SearchLabels: []string{"alpha", "gpt"}})
	if err != nil || got != 1 {
		t.Fatalf("got %d err %v", got, err)
	}
}

var errDispatched = errors.New("navigation dispatched")

func TestEnhancedControlKeysDispatchWhileSearchIsActive(t *testing.T) {
	for seq, expected := range map[string]MenuNavigationEvent{"\x1b[99;5u": NavEventCancel, "\x1b[1;1D": NavEventBack} {
		var events []MenuNavigationEvent
		h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
			events = append(events, ev)
			if ev == expected {
				return nil, errDispatched
			}
			if ev == NavEventBegin {
				return &MenuNavigationStart{AllowBack: true}, nil
			}
			return nil, nil
		}
		c, _ := testConsole("", &fakeScreen{keys: keys("/g" + seq)})
		ctx := WithMenuNavigationHandler(context.Background(), h)
		_, err := c.CursesRadiolist(ctx, "Pick", items("alpha", "gpt"), 0, RadiolistOptions{Searchable: true, SearchLabels: []string{"alpha", "gpt"}})
		if !errors.Is(err, errDispatched) || !reflect.DeepEqual(events, []MenuNavigationEvent{NavEventBegin, expected}) {
			t.Fatalf("%q err %v events %v", seq, err, events)
		}
	}
}

func TestNumberedFallbackCtrlCDispatchesScopedCancel(t *testing.T) {
	cancelled := errors.New("cancelled")
	c, _ := testConsole("\x1b[99;5u", nil)
	c.OpenScreen = func() (Screen, func(), error) { return nil, nil, errors.New("terminal unavailable") }
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		if ev == NavEventCancel {
			return nil, cancelled
		}
		if ev == NavEventBegin {
			return &MenuNavigationStart{}, nil
		}
		return nil, nil
	}
	cancel := -1
	_, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 0, RadiolistOptions{CancelReturns: &cancel})
	if !errors.Is(err, cancelled) {
		t.Fatalf("err %v", err)
	}
}

func TestNavigationHandlerProgrammingErrorIsNotHiddenByFallback(t *testing.T) {
	mismatch := errors.New("navigation contract mismatch")
	c, _ := testConsole("", &fakeScreen{keys: []int{13}})
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		if ev == NavEventResolve {
			return nil, mismatch
		}
		return &MenuNavigationStart{}, nil
	}
	if _, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 0, RadiolistOptions{}); !errors.Is(err, mismatch) {
		t.Fatalf("err %v", err)
	}
}

func TestRadiolistDispatchesContextualBackAndEscape(t *testing.T) {
	for _, tc := range []struct {
		ks   []int
		want MenuNavigationEvent
	}{{[]int{KeyLeft}, NavEventBack}, {keys("\x1b"), NavEventCancel}} {
		var events []MenuNavigationEvent
		var resolved any
		h := func(ev MenuNavigationEvent, v ...any) (*MenuNavigationStart, error) {
			events = append(events, ev)
			if ev == NavEventResolve {
				resolved = v[0]
			}
			return &MenuNavigationStart{AllowBack: true}, nil
		}
		c, _ := testConsole("", &fakeScreen{keys: tc.ks})
		got, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 1, RadiolistOptions{})
		if err != nil || got != 1 || resolved != 1 || !reflect.DeepEqual(events, []MenuNavigationEvent{NavEventBegin, tc.want, NavEventResolve}) {
			t.Fatalf("%v got %d err %v events %v", tc.ks, got, err, events)
		}
	}
}

func TestReplayValueResolvesWithoutDrawing(t *testing.T) {
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		return &MenuNavigationStart{Replay: true, ReplayValue: 1}, nil
	}
	c, _ := testConsole("", nil)
	c.OpenScreen = func() (Screen, func(), error) { t.Fatal("screen opened"); return nil, nil, nil }
	if got, err := c.CursesRadiolist(WithMenuNavigationHandler(context.Background(), h), "Pick", items("a", "b"), 0, RadiolistOptions{}); err != nil || got != 1 {
		t.Fatalf("got %d err %v", got, err)
	}
}

func TestNonTTYStdinReturnsCancelValue(t *testing.T) {
	c, _ := testConsole("", nil)
	c.StdinTTY = func() bool { return false }
	got, ok, err := c.CursesSingleSelect(context.Background(), "Pick", []string{"a"}, 0, "Cancel", false)
	if err != nil || ok || got != 0 {
		t.Fatalf("got %d %v %v", got, ok, err)
	}
}

const longDescription = "Unavailable model 0"

func viewportRun(t *testing.T, ks []int, heights []int, description string, selected int) (int, []map[int]string) {
	t.Helper()
	labels := make([]string, 20)
	for i := range labels {
		labels[i] = fmt.Sprintf("model-%02d", i)
	}
	f := &fakeScreen{keys: ks, heights: heights}
	c, _ := testConsole("", f)
	got, err := c.CursesRadiolist(context.Background(), "Select default model:", items(labels...), selected, RadiolistOptions{Description: description, Searchable: true})
	if err != nil {
		t.Fatal(err)
	}
	return got, f.frames
}

func longDesc() string {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("Unavailable model %d", i)
	}
	return strings.Join(lines, "\n")
}

func anyRow(frame map[int]string, pred func(string) bool) bool {
	for _, r := range frame {
		if pred(r) {
			return true
		}
	}
	return false
}

func TestLongDescriptionKeepsSelectedChoiceAndHintVisible(t *testing.T) {
	for _, height := range []int{5, 8, 12, 24, 50} {
		got, frames := viewportRun(t, []int{10}, []int{height}, longDesc(), 19)
		f := frames[0]
		if got != 19 || !anyRow(f, func(r string) bool { return strings.Contains(r, "model-19") && strings.Contains(r, "→") }) || !anyRow(f, func(r string) bool { return strings.Contains(r, "/ search") }) {
			t.Fatalf("height %d frame %v", height, f)
		}
		choices := 0
		for _, r := range f {
			if strings.Contains(r, "model-") {
				choices++
			}
		}
		if choices < min(5, height-4) {
			t.Fatalf("height %d choices %d", height, choices)
		}
		desc := anyRow(f, func(r string) bool { return strings.Contains(r, "Unavailable model") })
		more := anyRow(f, func(r string) bool { return strings.Contains(r, "more description lines") })
		if (height < 12 && (desc || more)) || (height >= 12 && height < 50 && !more) {
			t.Fatalf("height %d desc %v more %v", height, desc, more)
		}
		if height == 50 {
			for i := 0; i < 40; i++ {
				if !anyRow(f, func(r string) bool { return r == fmt.Sprintf("Unavailable model %d", i) }) {
					t.Fatalf("missing description %d", i)
				}
			}
		}
	}
}

func TestChoiceScrollsAfterTerminalShrinks(t *testing.T) {
	ks := append(repeat(KeyDown, 19), 10)
	got, frames := viewportRun(t, ks, []int{50, 12}, longDesc(), 0)
	if got != 19 {
		t.Fatalf("got %d", got)
	}
	for i, f := range frames {
		want := fmt.Sprintf("model-%02d", i)
		if !anyRow(f, func(r string) bool { return strings.Contains(r, want) && strings.Contains(r, "→") }) {
			t.Fatalf("frame %d lost the cursor %v", i, f)
		}
	}
}

func repeat(k, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = k
	}
	return out
}

func TestSearchResultStaysVisibleWithLongDescription(t *testing.T) {
	got, frames := viewportRun(t, keys("/19\n"), []int{12}, longDesc(), 0)
	last := frames[len(frames)-1]
	if got != 19 || !anyRow(last, func(r string) bool { return strings.Contains(r, "Search: 19") }) || !anyRow(last, func(r string) bool { return strings.Contains(r, "model-19") && strings.Contains(r, "→") }) {
		t.Fatalf("got %d frame %v", got, last)
	}
}

func TestShortDescriptionLayoutIsUnchanged(t *testing.T) {
	for _, desc := range []string{"", "First line\nSecond line"} {
		got, frames := viewportRun(t, []int{10}, []int{24}, desc, 0)
		start := 3
		if desc != "" {
			start += 2
		}
		if got != 0 || !strings.Contains(frames[0][start], "model-00") || anyRow(frames[0], func(r string) bool { return strings.Contains(r, "more description lines") }) {
			t.Fatalf("desc %q frame %v", desc, frames[0])
		}
	}
}

func TestReconcileCursorMovesToFirstVisibleMatch(t *testing.T) {
	if c, p := ReconcileCursor([]int{2, 4}, 0); c != 2 || p != 0 {
		t.Fatal("first match")
	}
	if c, p := ReconcileCursor([]int{2, 4}, 4); c != 4 || p != 1 {
		t.Fatal("kept cursor")
	}
}

func TestActiveSearchConsumesQueryEditingAndConfirmKeys(t *testing.T) {
	s := &SearchState{Active: true, Query: "op"}
	if h, c, ch := HandleActiveSearchKey('u', s); !h || c || !ch || s.Query != "opu" {
		t.Fatal("typing")
	}
	if h, c, ch := HandleActiveSearchKey(KeyEnter, s); !h || !c || ch {
		t.Fatal("confirm")
	}
}

func TestScorerRanksPrefixAndBoundaryMatchesHigher(t *testing.T) {
	a, _ := FuzzyScore("gpt-4o", "gpt")
	b, _ := FuzzyScore("gpt-4o", "g4o")
	c, _ := FuzzyScore("claude-sonnet-4", "sonnet")
	d, _ := FuzzyScore("claude-sonnet-4", "clad snnt")
	if !(a > b) || !(c > d) {
		t.Fatalf("scores %v %v %v %v", a, b, c, d)
	}
	if _, ok := FuzzyScore("gpt-4o", "zzz"); ok {
		t.Fatal("non subsequence matched")
	}
}

func TestEscClearsQueryAndSignalsChanged(t *testing.T) {
	s := &SearchState{Active: true, Query: "gpt"}
	if h, c, ch := HandleActiveSearchKey(27, s); !h || c || !ch || s.Active || s.Query != "" {
		t.Fatal("esc with query")
	}
	s2 := &SearchState{Active: true}
	if h, c, ch := HandleActiveSearchKey(27, s2); !h || c || ch {
		t.Fatal("esc without query")
	}
}

func bar(fraction float64) string {
	n := int(fraction*10 + 0.5)
	return "[" + strings.Repeat("|", n) + strings.Repeat(" ", 10-n) + "]"
}

func TestRealGaugesAreStyled(t *testing.T) {
	for _, tc := range []struct {
		f     float64
		style string
	}{{0, "green"}, {0.6, "yellow"}, {0.9, "red"}} {
		b := bar(tc.f)
		want := []Segment{{Text: "Today  "}, {Text: b, Style: tc.style}, {Text: "  x"}}
		if got := GaugeBarSegments("Today  " + b + "  x"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v got %v", tc.f, got)
		}
	}
}

func TestUnrelatedBracketsStayPlain(t *testing.T) {
	for _, text := range []string{"toggle [] here", "checkbox [ ] off", "a [||] b", "flag [x]", "[" + strings.Repeat(" ", 11) + "]", "[| |       ]"} {
		if got := GaugeBarSegments(text); !reflect.DeepEqual(got, []Segment{{Text: text}}) {
			t.Fatalf("%q got %v", text, got)
		}
	}
}

func TestColorizeAndFormatANSI(t *testing.T) {
	c, _ := testConsole("", nil)
	if got := c.ColorizeGaugeBarsANSI("x " + bar(0.9)); got != "x "+Red+bar(0.9)+Reset {
		t.Fatalf("got %q", got)
	}
	item := RadioItem{{Text: "a"}, {Text: "b", Style: "dim"}, {Text: "c", Style: "bogus"}}
	if got := c.FormatRadioItemANSI(item); got != "a"+Dim+"b"+Reset+"c" {
		t.Fatalf("got %q", got)
	}
	c.Getenv = func(k string) (string, bool) { return "", k == "NO_COLOR" }
	if c.Color("x", Red) != "x" {
		t.Fatal("NO_COLOR ignored")
	}
}

func TestLineInputSupportsCursorAndEmacsNavigation(t *testing.T) {
	c, out := testConsole("ac\x1b[Db\x1b[Cd\x01X\x05Y\r", nil)
	got, err := c.LineInput("Enter model name: ")
	if err != nil || got != "XabcdY" || !strings.Contains(out.String(), "Enter model name: ") {
		t.Fatalf("got %q err %v", got, err)
	}
	c2, _ := testConsole("abc def\x17\x7fX\x0bZ\x15Q\r", nil)
	if got, _ := c2.LineInput("> "); got != "Q" {
		t.Fatalf("kill keys got %q", got)
	}
	c3, _ := testConsole("\x03", nil)
	if _, err := c3.LineInput("> "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("ctrl c err %v", err)
	}
	c4, _ := testConsole("\x04", nil)
	if _, err := c4.LineInput("> "); !errors.Is(err, ErrEOF) {
		t.Fatalf("ctrl d err %v", err)
	}
}

func TestLineInputPreservesBuiltinInputForRedirectedStdin(t *testing.T) {
	c, out := testConsole("typed model\n", nil)
	c.StdinTTY = func() bool { return false }
	got, err := c.LineInput("Enter model name: ")
	if err != nil || got != "typed model" || out.String() != "Enter model name: " {
		t.Fatalf("got %q err %v out %q", got, err, out.String())
	}
}

func TestLineInputFallsBackToInputOnEditorFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	io.WriteString(w, "fallback text\n")
	w.Close()
	c, _ := testConsole("", nil)
	c.In = r
	if got, err := c.LineInput("Enter model name: "); err != nil || got != "fallback text" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestPasswordPromptUsesMaskedSecretPrompt(t *testing.T) {
	c, _ := testConsole("", nil)
	var seen string
	c.MaskedSecretPrompt = func(display string) (string, error) { seen = display; return " secret ", nil }
	if got := c.Prompt("API key", "old", true); got != "secret" || !strings.Contains(seen, "API key [old]") {
		t.Fatalf("got %q seen %q", got, seen)
	}
	c.MaskedSecretPrompt = func(string) (string, error) { return "", nil }
	if got := c.Prompt("API key", "old", true); got != "old" {
		t.Fatalf("empty got %q", got)
	}
	c.MaskedSecretPrompt = func(string) (string, error) { return "", ErrInterrupt }
	if got := c.Prompt("API key", "old", true); got != "" {
		t.Fatalf("interrupt got %q", got)
	}
}

func TestPromptYesNo(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	for input, want := range map[string]bool{"\n": true, "n\n": false, "yes\n": true, "nope\n": false} {
		c, _ := testConsole(input, nil)
		c.StdinTTY = func() bool { return false }
		if got := c.PromptYesNo(ctx, "Continue?", true); got != want {
			t.Fatalf("%q got %v", input, got)
		}
	}
}

func TestScopedNumberedInputHandlesNavigationKeys(t *testing.T) {
	h := func(ev MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		return &MenuNavigationStart{AllowBack: true}, nil
	}
	ctx := context.WithValue(WithMenuNavigationHandler(context.Background(), h), numberedBackKey{}, true)
	for seq, want := range map[string]numberedNav{"\x1b[27u": navCancelKey, "\x1b[D": navBackKey, "\x1b[99;5u": navCancelKey} {
		c, _ := testConsole(seq, nil)
		if _, nav, err := c.readNumberedInput(ctx, "Choice: "); err != nil || nav != want {
			t.Fatalf("%q nav %v err %v", seq, nav, err)
		}
	}
}

func TestNumberedFallbacks(t *testing.T) {
	ctx := context.Background()
	c, out := testConsole("2\n\n", nil)
	c.StdinTTY = func() bool { return false }
	res, err := c.numberedFallback(ctx, "Tools", []string{"a", "b"}, map[int]bool{0: true}, map[int]bool{}, func(s map[int]bool) string { return fmt.Sprintf("%d on", len(s)) })
	if err != nil || !reflect.DeepEqual(res, map[int]bool{0: true, 1: true}) || !strings.Contains(out.String(), "2 on") {
		t.Fatalf("checklist %v %v", res, err)
	}
	c, _ = testConsole("9\n", nil)
	if res, _ := c.radioNumberedFallback(ctx, "Pick", items("a", "b"), 1, -1, map[int]string{0: "Head"}); res != 1 {
		t.Fatalf("radio out of range %v", res)
	}
	c, _ = testConsole("x\n", nil)
	if res, _ := c.radioNumberedFallback(ctx, "Pick", items("a", "b"), 1, -1, nil); res != -1 {
		t.Fatalf("radio junk %v", res)
	}
	c, _ = testConsole("2\n", nil)
	if res, _ := c.numberedSingleFallback(ctx, "Pick", []string{"a", "Cancel"}, 1); res != nil {
		t.Fatalf("single cancel row %v", res)
	}
	c, _ = testConsole("١\n", nil)
	if res, _ := c.numberedSingleFallback(ctx, "Pick", []string{"a", "Cancel"}, 1); res != 0 {
		t.Fatalf("arabic digit %v", res)
	}
}

func TestChecklistAndSingleSelectThroughScreen(t *testing.T) {
	ctx := context.Background()
	c, _ := testConsole("", &fakeScreen{keys: []int{' ', KeyDown, ' ', 10}})
	got, err := c.CursesChecklist(ctx, "Tools", []string{"a", "b", "c"}, map[int]bool{0: true}, nil, func(s map[int]bool) string { return "status" })
	if err != nil || !reflect.DeepEqual(got, map[int]bool{1: true}) {
		t.Fatalf("checklist %v %v", got, err)
	}
	c, _ = testConsole("", &fakeScreen{keys: []int{'q'}})
	got, _ = c.CursesChecklist(ctx, "Tools", []string{"a"}, map[int]bool{0: true}, nil, nil)
	if !reflect.DeepEqual(got, map[int]bool{0: true}) {
		t.Fatalf("cancel %v", got)
	}
	c, _ = testConsole("", &fakeScreen{keys: []int{KeyDown, KeyDown, 10}})
	if n, ok, err := c.CursesSingleSelect(ctx, "Pick", []string{"a", "b"}, 0, "Cancel", false); ok || err != nil || n != 0 {
		t.Fatalf("cancel row %d %v %v", n, ok, err)
	}
	c, _ = testConsole("", &fakeScreen{keys: []int{KeyUp, KeyUp, 10}})
	if n, ok, _ := c.CursesSingleSelect(ctx, "Pick", []string{"a", "b"}, 0, "Cancel", false); !ok || n != 1 {
		t.Fatalf("wrap %d %v", n, ok)
	}
	c, _ = testConsole("2\n", &fakeScreen{initErr: errors.New("curs_set() returned ERR")})
	if n, ok, _ := c.CursesSingleSelect(ctx, "Pick", []string{"a", "b"}, 0, "Cancel", false); !ok || n != 1 {
		t.Fatalf("init failure fallback %d %v", n, ok)
	}
}

func TestHeadingsAreDrawnButNeverSelected(t *testing.T) {
	f := &fakeScreen{keys: []int{KeyDown, 10}}
	c, _ := testConsole("", f)
	got, err := c.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, RadiolistOptions{Headings: map[int]string{0: "First", 1: "Second"}})
	if err != nil || got != 1 || !anyRow(f.frames[0], func(r string) bool { return r == "   Second" }) {
		t.Fatalf("got %d err %v frame %v", got, err, f.frames[0])
	}
}

func TestFlushStdinIsTTYGated(t *testing.T) {
	c, _ := testConsole("leftover", nil)
	c.StdinTTY = func() bool { return false }
	c.FlushStdin()
	if got, _ := c.input(""); got != "leftover" {
		t.Fatalf("non tty flush consumed %q", got)
	}
}

// Writer feeding a terminal emulator that snapshots the visible rows after every synchronized frame
type emulatorOut struct {
	emu    *term.Emulator
	frames [][]string
}

func (e *emulatorOut) Write(p []byte) (int, error) {
	e.emu.Write(p)
	if bytes.Contains(p, []byte("\x1b[?2026l")) {
		_, h := e.emu.Size()
		rows := make([]string, h)
		for y := 0; y < h; y++ {
			line, _ := e.emu.Row(e.emu.Base() + uint64(y))
			var b strings.Builder
			for _, cell := range line.Cells {
				b.WriteString(cell.Content)
			}
			rows[y] = strings.TrimRight(b.String(), " ")
		}
		e.frames = append(e.frames, rows)
	}
	return len(p), nil
}

func TestFullScreenRendererDrawsAndReadsKeypadKeys(t *testing.T) {
	emu := term.New(60, 12, term.Options{})
	out := &emulatorOut{emu: emu}
	down := string(emu.Key(term.Key{Code: uv.KeyDown}))
	c := &Console{In: strings.NewReader(""), Out: out,
		Getenv: func(k string) (string, bool) {
			return map[string]string{"TERM": "xterm-256color", "ESCDELAY": "25"}[k], k == "TERM" || k == "ESCDELAY"
		},
		StdinTTY: func() bool { return true }, StdoutTTY: func() bool { return true },
		TermSize: func() (int, int, error) { return 12, 60, nil },
	}
	c.In = strings.NewReader(down + "\x1bOB" + "\r")
	got, err := c.CursesRadiolist(context.Background(), "Pick a model", items("alpha", "beta", "gamma"), 0, RadiolistOptions{Searchable: true})
	if err != nil || got != 2 {
		t.Fatalf("got %d err %v", got, err)
	}
	first := out.frames[0]
	if first[0] != "Pick a model" || !strings.Contains(first[1], "/ search") || first[3] != " → (●) alpha" || first[4] != "   (○) beta" {
		t.Fatalf("first frame %q", first)
	}
	if last := out.frames[len(out.frames)-1]; last[5] != " → (○) gamma" {
		t.Fatalf("last frame %q", last)
	}
	if emu.AltScreen() {
		t.Fatal("alternate screen left on")
	}
}

func TestMissingTerminfoFallsBackToNumberedList(t *testing.T) {
	c, out := testConsole("2\n", nil)
	c.OpenScreen = nil
	got, err := c.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, RadiolistOptions{})
	if err != nil || got != 1 || !strings.Contains(out.String(), "Select by number") {
		t.Fatalf("got %d err %v out %q", got, err, out.String())
	}
}

func TestInitPairsHidesTheCursorAndMapsCursesColors(t *testing.T) {
	var out bytes.Buffer
	c := &Console{Out: &out}
	ti := &terminfo.Terminfo{Strings: map[int][]byte{terminfo.CursorInvisible: []byte("\x1b[?25l"), terminfo.SetAForeground: []byte("\x1b[3%p1%dm")}, Nums: map[int]int{}}
	s := &ansiScreen{c: c, ti: ti, colors: 256, pairs: map[int]string{}}
	if err := s.InitPairs(map[int]int{1: ColorGreen, 3: ColorCyan, 4: 8, 5: 200}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\x1b[?25l" || s.pairs[1] != "32" || s.pairs[3] != "36" || s.pairs[4] != "90" || s.pairs[5] != "38;5;200" || s.Colors() != 256 {
		t.Fatalf("out %q pairs %v", out.String(), s.pairs)
	}
	bare := &ansiScreen{c: c, ti: &terminfo.Terminfo{Strings: map[int][]byte{}, Nums: map[int]int{}}, pairs: map[int]string{}}
	if err := bare.InitPairs(map[int]int{1: ColorGreen}); err == nil || err.Error() != "curs_set() returned ERR" {
		t.Fatalf("limited terminal %v", err)
	}
	opened := 0
	c.OpenScreen = func() (Screen, func(), error) { opened++; return &fakeScreen{}, func() {}, nil }
	if scr, _, err := c.OpenFullScreen(); err != nil || scr == nil || opened != 1 {
		t.Fatalf("injected opener %v %d", err, opened)
	}
}
