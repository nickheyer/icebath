package cliui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Segment is one styled run of a radiolist row, Style empty, yellow, dim, green or red
type Segment struct {
	Text  string
	Style string
}

// RadioItem is a radiolist row as styled segments
type RadioItem []Segment

// PlainItem is a radiolist row with no styling
func PlainItem(text string) RadioItem { return RadioItem{{Text: text}} }

// MenuNavigationEvent names a scoped navigation notification
type MenuNavigationEvent string

const (
	NavEventBegin   MenuNavigationEvent = "begin"
	NavEventResolve MenuNavigationEvent = "resolve"
	NavEventCancel  MenuNavigationEvent = "cancel"
	NavEventBack    MenuNavigationEvent = "back"
)

// MenuNavigationStart is what a scoped handler answers when a menu begins
type MenuNavigationStart struct {
	AllowBack   bool
	GuardCancel bool
	Replay      bool
	ReplayValue any
}

// MenuNavigationHandler receives begin, resolve, cancel and back events, an error aborting the menu
type MenuNavigationHandler func(event MenuNavigationEvent, value ...any) (*MenuNavigationStart, error)

type navHandlerKey struct{}

// WithMenuNavigationHandler scopes setup style cancel and back behavior to menus run under ctx
func WithMenuNavigationHandler(ctx context.Context, h MenuNavigationHandler) context.Context {
	return context.WithValue(ctx, navHandlerKey{}, h)
}

// Handler scoped on ctx, else the process handler set for the running CLI invocation
func navHandler(ctx context.Context) MenuNavigationHandler {
	if ctx != nil {
		if h, ok := ctx.Value(navHandlerKey{}).(MenuNavigationHandler); ok {
			return h
		}
	}
	processNavMu.Lock()
	defer processNavMu.Unlock()
	return processNav
}

var (
	processNavMu sync.Mutex
	processNav   MenuNavigationHandler
)

// NavigationToken restores the process handler active before a set
type NavigationToken struct{ prev MenuNavigationHandler }

// SetMenuNavigationHandler scopes cancel and back behavior to the current CLI invocation for callers without a ctx
func SetMenuNavigationHandler(h MenuNavigationHandler) NavigationToken {
	processNavMu.Lock()
	defer processNavMu.Unlock()
	tok := NavigationToken{prev: processNav}
	processNav = h
	return tok
}

// ResetMenuNavigationHandler restores the handler active before the token was issued
func ResetMenuNavigationHandler(tok NavigationToken) {
	processNavMu.Lock()
	processNav = tok.prev
	processNavMu.Unlock()
}

func notifyScoped(ctx context.Context, event MenuNavigationEvent) error {
	if h := navHandler(ctx); h != nil {
		_, err := h(event)
		return err
	}
	return nil
}

// Curses key codes the keypad translation produces
const (
	KeyDown      = 258
	KeyUp        = 259
	KeyLeft      = 260
	KeyRight     = 261
	KeyHome      = 262
	KeyBackspace = 263
	KeyF0        = 264
	KeyDC        = 330
	KeyIC        = 331
	KeyNPage     = 338
	KeyPPage     = 339
	KeyEnter     = 343
	KeyEnd       = 360
)

// RadioItemPlain flattens a radiolist row to its searchable text
func RadioItemPlain(item RadioItem) string {
	var b strings.Builder
	for _, s := range item {
		b.WriteString(s.Text)
	}
	return b.String()
}

func wordBoundary(r rune) bool { return strings.ContainsRune("-_/. ", r) }

// Start, after a separator, or a lower to upper case step
func isBoundary(target []rune, index int) bool {
	if index == 0 {
		return true
	}
	prev, cur := target[index-1], target[index]
	return wordBoundary(prev) || (string(prev) == strings.ToLower(string(prev)) && string(cur) != strings.ToLower(string(cur)) && string(cur) == strings.ToUpper(string(cur)))
}

// Scores one token as a subsequence of the lowered target, false when it is not one
func tokenScore(orig, lower []rune, token []rune) (float64, bool) {
	score, prev := 0.0, -1
	var positions []int
	for _, ch := range token {
		idx := -1
		for k := prev + 1; k < len(lower); k++ {
			if lower[k] == ch {
				idx = k
				break
			}
		}
		if idx < 0 {
			return 0, false
		}
		positions = append(positions, idx)
		score++
		if prev >= 0 && idx == prev+1 {
			score += 5
		} else if prev >= 0 {
			score -= float64(min(idx-prev-1, 3))
		}
		if idx < len(orig) && isBoundary(orig, idx) {
			score += 3
		}
		if idx == 0 {
			score += 5
		}
		prev = idx
	}
	if len(positions) > 0 && positions[0] == 0 && positions[len(positions)-1] == len(positions)-1 {
		score += 8
	}
	if string(lower) == string(token) {
		score += 20
	}
	return score - float64(len(lower))*0.01, true
}

// FuzzyScore sums per token scores of a multi token query, false when any token misses
func FuzzyScore(label, query string) (float64, bool) {
	lower := []rune(strings.ToLower(label))
	orig := []rune(label)
	total := 0.0
	for _, tok := range strings.Fields(strings.ToLower(query)) {
		s, ok := tokenScore(orig, lower, []rune(tok))
		if !ok {
			return 0, false
		}
		total += s
	}
	return total, true
}

// FilterIndices returns matching item indices best first with ties in catalog order
func FilterIndices(items []string, query string) []int {
	q := strings.TrimSpace(query)
	if q == "" {
		out := make([]int, len(items))
		for i := range out {
			out[i] = i
		}
		return out
	}
	type scored struct {
		i int
		s float64
	}
	var hits []scored
	for i, label := range items {
		if s, ok := FuzzyScore(label, q); ok {
			hits = append(hits, scored{i, s})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].s != hits[b].s {
			return hits[a].s > hits[b].s
		}
		return hits[a].i < hits[b].i
	})
	out := make([]int, len(hits))
	for k, h := range hits {
		out[k] = h.i
	}
	return out
}

// SearchState is the slash search prompt state of a menu
type SearchState struct {
	Active bool
	Query  string
}

func indexOf(list []int, v int) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// ReconcileCursor keeps the cursor on a visible row, returning it and its position
func ReconcileCursor(filtered []int, cursor int) (int, int) {
	if len(filtered) == 0 {
		return cursor, 0
	}
	if p := indexOf(filtered, cursor); p >= 0 {
		return cursor, p
	}
	return filtered[0], 0
}

// ScrollForCursor clamps the scroll offset so the cursor row stays visible
func ScrollForCursor(offset, cursorPos, visible, total int) int {
	visible = max(1, visible)
	if cursorPos < offset {
		offset = cursorPos
	} else if cursorPos >= offset+visible {
		offset = cursorPos - visible + 1
	}
	return max(0, min(offset, max(0, total-visible)))
}

// errCursesInit is a terminal that cannot open a full screen surface
var errCursesInit = errors.New("curses error")

func copySet(s map[int]bool) map[int]bool {
	out := make(map[int]bool, len(s))
	for k, v := range s {
		if v {
			out[k] = true
		}
	}
	return out
}

var ansiStyle = map[string]string{"yellow": Yellow, "dim": Dim, "green": Green, "red": Red}

var gaugeBarRe = regexp.MustCompile(`\[[| ]{10}\]`)
var gaugeShapeRe = regexp.MustCompile(`^\[\|* *\]$`)

// Green, yellow or red by how much of the gauge is used
func gaugeBarStyle(bar string) string {
	inner := []rune(bar)
	inner = inner[1 : len(inner)-1]
	used := 0.0
	if len(inner) > 0 {
		used = float64(strings.Count(string(inner), "|")) / float64(len(inner))
	}
	switch {
	case used > 0.8:
		return "red"
	case used > 0.5:
		return "yellow"
	}
	return "green"
}

// GaugeBarSegments splits text so each ten cell usage gauge is its own styled segment
func GaugeBarSegments(text string) []Segment {
	var segs []Segment
	last := 0
	pos := 0
	for pos <= len(text) {
		loc := gaugeBarRe.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		s, e := pos+loc[0], pos+loc[1]
		if !gaugeShapeRe.MatchString(text[s:e]) {
			pos = s + 1
			continue
		}
		if s > last {
			segs = append(segs, Segment{Text: text[last:s]})
		}
		segs = append(segs, Segment{Text: text[s:e], Style: gaugeBarStyle(text[s:e])})
		last, pos = e, e
	}
	if last < len(text) {
		segs = append(segs, Segment{Text: text[last:]})
	}
	if len(segs) == 0 {
		return []Segment{{Text: text}}
	}
	return segs
}

// ColorizeGaugeBarsANSI colors usage gauges in a header or legend line
func (c *Console) ColorizeGaugeBarsANSI(text string) string {
	var b strings.Builder
	for _, s := range GaugeBarSegments(text) {
		if code, ok := ansiStyle[s.Style]; ok {
			b.WriteString(c.Color(s.Text, code))
		} else {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

// FormatRadioItemANSI applies ANSI colors to a styled radiolist row
func (c *Console) FormatRadioItemANSI(item RadioItem) string {
	var b strings.Builder
	for _, s := range item {
		if code, ok := ansiStyle[s.Style]; ok {
			b.WriteString(c.Color(s.Text, code))
		} else {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

// Numbered choice as a zero based index, empty on a blank line, abort on interrupt, end of input or junk
func (c *Console) readNumberedChoice(ctx context.Context, prompt string) (idx int, empty, abort bool, err error) {
	val, rerr := c.input(prompt)
	if errors.Is(rerr, ErrInterrupt) || errors.Is(rerr, ErrEOF) {
		return 0, false, true, notifyScoped(ctx, NavEventCancel)
	}
	if rerr != nil {
		return 0, false, false, rerr
	}
	if strings.TrimSpace(val) == "" {
		return 0, true, false, nil
	}
	n, perr := strconv.Atoi(pyIntText(val))
	if perr != nil {
		return 0, false, true, nil
	}
	return n - 1, false, false, nil
}

// Strips whitespace and digit separators the way Python int accepts them
func pyIntText(s string) string {
	s = strings.TrimFunc(s, unicode.IsSpace)
	if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
		return "x"
	}
	var b strings.Builder
	for _, r := range strings.ReplaceAll(s, "_", "") {
		if r > 0x7f && unicode.IsDigit(r) {
			start := r
			for unicode.IsDigit(start - 1) {
				start--
			}
			r = '0' + (r-start)%10
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Text numbered fallback for radio selection, headings printed and never numbered
func (c *Console) radioNumberedFallback(ctx context.Context, title string, items []RadioItem, selected, cancel int, headings map[int]string) (any, error) {
	c.println(c.Color("\n  "+title, Yellow))
	c.println(c.Color("  Select by number, Enter to confirm.\n", Dim))
	for i, label := range items {
		if h, ok := headings[i]; ok {
			c.println(c.Color("  "+h, Dim))
		}
		marker := "(○)"
		if i == selected {
			marker = c.Color("(●)", Green)
		}
		c.println(fmt.Sprintf("  %s %2d. %s", marker, i+1, c.FormatRadioItemANSI(label)))
	}
	c.println("")
	idx, empty, abort, err := c.readNumberedChoice(ctx, c.Color(fmt.Sprintf("  Choice [default %d]: ", selected+1), Dim))
	if err != nil {
		return nil, err
	}
	if abort {
		return cancel, nil
	}
	if !empty && idx >= 0 && idx < len(items) {
		return idx, nil
	}
	return selected, nil
}

func (c *Console) numberedSingleFallback(ctx context.Context, title string, items []string, cancelIdx int) (any, error) {
	c.println("\n  " + title + "\n")
	for i, label := range items {
		c.println(fmt.Sprintf("  %d. %s", i+1, label))
	}
	c.println("")
	idx, empty, abort, err := c.readNumberedChoice(ctx, fmt.Sprintf("  Choice [1-%d]: ", len(items)))
	if err != nil {
		return nil, err
	}
	if abort || empty || idx < 0 || idx >= min(len(items), cancelIdx) {
		return nil, nil
	}
	return idx, nil
}

// Text toggle fallback for terminals that cannot draw the live list
func (c *Console) numberedFallback(ctx context.Context, title string, items []string, selected, cancelReturns map[int]bool, statusFn func(map[int]bool) string) (any, error) {
	chosen := copySet(selected)
	c.println(c.Color("\n  "+title, Yellow))
	c.println(c.Color("  Toggle by number, Enter to confirm.\n", Dim))
	for {
		for i, label := range items {
			marker := "[ ]"
			if chosen[i] {
				marker = c.Color("[✓]", Green)
			}
			c.println(fmt.Sprintf("  %s %2d. %s", marker, i+1, label))
		}
		if statusFn != nil {
			if status := statusFn(chosen); status != "" {
				c.println(c.Color("\n  "+status, Dim))
			}
		}
		c.println("")
		idx, empty, abort, err := c.readNumberedChoice(ctx, c.Color("  Toggle # (or Enter to confirm): ", Dim))
		if err != nil {
			return nil, err
		}
		if abort {
			return cancelReturns, nil
		}
		if empty {
			return chosen, nil
		}
		if idx >= 0 && idx < len(items) {
			if chosen[idx] {
				delete(chosen, idx)
			} else {
				chosen[idx] = true
			}
		}
		c.println("")
	}
}

// ESC delay ncurses waits for a keypad sequence, from ESCDELAY or one second
func (c *Console) escDelay() time.Duration {
	if v, ok := c.getenv("ESCDELAY"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return time.Second
}
