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

	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	Replay      bool
	ReplayValue any
}

// MenuNavigationHandler receives begin, resolve, cancel and back events, an error aborting the menu
type MenuNavigationHandler func(event MenuNavigationEvent, value ...any) (*MenuNavigationStart, error)

type navHandlerKey struct{}
type numberedBackKey struct{}

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

func numberedBackEnabled(ctx context.Context) bool {
	v, _ := ctx.Value(numberedBackKey{}).(bool)
	return v
}

func notifyScoped(ctx context.Context, event MenuNavigationEvent) error {
	if h := navHandler(ctx); h != nil {
		_, err := h(event)
		return err
	}
	return nil
}

// Normalized menu actions
const (
	NavUp        = "up"
	NavDown      = "down"
	NavBack      = "back"
	NavSelect    = "select"
	NavToggle    = "toggle"
	NavCancel    = "cancel"
	NavInterrupt = "interrupt"
	NavNone      = "none"
)

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

// Attribute for a segment style, the cursor row forced bold green
func styleAttr(scr Screen, style string, isCursor bool) Attr {
	has := scr.HasColors()
	if isCursor {
		a := Attr{Bold: true}
		if has {
			a.Pair = 1
		}
		return a
	}
	switch {
	case style == "yellow" && has:
		return Attr{Pair: 2}
	case style == "green" && has:
		return Attr{Pair: 1}
	case style == "red" && has:
		return Attr{Pair: 4}
	case style != "dim":
		return Attr{}
	}
	a := Attr{Dim: true}
	if has {
		a.Pair = 3
	}
	return a
}

// Draws text swallowing writes past the screen edge
func addnstr(scr Screen, y, x int, text string, n int, attr Attr) {
	scr.AddNStr(y, x, text, n, attr)
}

func drawTitleAndHint(scr Screen, title, hint string, maxX, hintRow int) {
	h := Attr{Bold: true}
	if scr.HasColors() {
		h.Pair = 2
	}
	addnstr(scr, 0, 0, title, maxX-1, h)
	addnstr(scr, hintRow, 0, hint, maxX-1, Attr{Dim: true})
}

type attrSegment struct {
	text string
	attr Attr
}

// Draws segments left to right from column x clipped at the edge
func drawSegments(scr Screen, y, x int, segs []attrSegment, maxX int) {
	col := x
	for _, s := range segs {
		remaining := maxX - 1 - col
		if remaining <= 0 {
			break
		}
		rs := []rune(s.text)
		if len(rs) > remaining {
			rs = rs[:remaining]
		}
		addnstr(scr, y, col, string(rs), remaining, s.attr)
		col += len(rs)
	}
}

// Draws a description line with stars in yellow and gauges by how much is used
func drawDescriptionLine(scr Screen, y int, text string, maxX int) {
	star := Attr{}
	if scr.HasColors() {
		star = Attr{Pair: 2}
	}
	var segs []attrSegment
	for i, part := range strings.Split(text, "★") {
		if i > 0 {
			segs = append(segs, attrSegment{"★", star})
		}
		if part != "" {
			for _, g := range GaugeBarSegments(part) {
				segs = append(segs, attrSegment{g.Text, styleAttr(scr, g.Style, false)})
			}
		}
	}
	drawSegments(scr, y, 0, segs, maxX)
}

func drawRadioItem(scr Screen, y, x int, item RadioItem, maxX int, isCursor bool) {
	segs := make([]attrSegment, len(item))
	for i, s := range item {
		segs[i] = attrSegment{s.Text, styleAttr(scr, s.Style, isCursor)}
	}
	drawSegments(scr, y, x, segs, maxX)
}

func drawPlainRow(scr Screen, y int, line string, maxX int, isCursor bool) {
	addnstr(scr, y, 0, line, maxX-1, styleAttr(scr, "", isCursor))
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

func moveFilteredCursor(filtered []int, cursor, pos, delta int) int {
	if len(filtered) == 0 {
		return cursor
	}
	n := len(filtered)
	return filtered[((pos+delta)%n+n)%n]
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

// HandleActiveSearchKey edits the active query, returning handled, confirm and changed
func HandleActiveSearchKey(key int, search *SearchState) (bool, bool, bool) {
	if !search.Active {
		return false, false, false
	}
	switch {
	case key == 27:
		had := search.Query != ""
		search.Active, search.Query = false, ""
		return true, false, had
	case key == KeyEnter || key == 10 || key == 13:
		return true, true, false
	case key == KeyBackspace || key == 127 || key == 8:
		if rs := []rune(search.Query); len(rs) > 0 {
			search.Query = string(rs[:len(rs)-1])
		}
	case key == 21:
		search.Query = ""
	case key >= 32 && key < 127:
		search.Query += string(rune(key))
	default:
		return false, false, false
	}
	return true, false, true
}

// ReadMenuKey reads one key and normalizes it to a menu action
func ReadMenuKey(scr Screen) (string, error) {
	k, err := scr.Getch()
	if err != nil {
		return "", err
	}
	return decodeMenuKey(scr, k)
}

func parseIntDefault(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

// Parses a kitty key report into codepoint, modifier and event type
func parseCSIUKey(raw string) (int, int, int, bool) {
	parts := strings.Split(raw, ";")
	cp := parseIntDefault(strings.SplitN(parts[0], ":", 2)[0], 0)
	if cp == 0 {
		return 0, 0, 0, false
	}
	var mods []string
	if len(parts) > 1 {
		mods = strings.Split(parts[1], ":")
	}
	mod, ev := 1, 1
	if len(mods) > 0 {
		mod = parseIntDefault(mods[0], 1)
	}
	if len(mods) > 1 {
		ev = parseIntDefault(mods[1], 1)
	}
	return cp, mod, ev, true
}

func parseCSINumbers(raw string) []int {
	var out []int
	for _, p := range strings.Split(raw, ";") {
		out = append(out, parseIntDefault(strings.SplitN(p, ":", 2)[0], 0))
	}
	return out
}

// Maps kitty and modifyOtherKeys codepoints to menu actions
func enhancedKeyAction(cp, mod int) string {
	switch cp {
	case 10, 13:
		return NavSelect
	case 27:
		return NavCancel
	case 32:
		return NavToggle
	}
	ctrl := (max(1, mod)-1)&4 != 0
	if cp == 3 || ((cp == 99 || cp == 67) && ctrl) {
		return NavInterrupt
	}
	if cp == 4 || ((cp == 100 || cp == 68) && ctrl) {
		return NavCancel
	}
	return NavNone
}

// Reads CSI parameter bytes through the final byte, zero final when none arrived
func readCSITail(scr Screen) (string, int, error) {
	var raw []byte
	for i := 0; i < 32; i++ {
		v, err := scr.Getch()
		if err != nil {
			return "", 0, err
		}
		if v >= 0x40 && v <= 0x7e {
			return string(raw), v, nil
		}
		if !(v >= 0x20 && v <= 0x3f) {
			break
		}
		raw = append(raw, byte(v))
	}
	return string(raw), 0, nil
}

var csiFinalNav = map[int]string{'A': NavUp, 'k': NavUp, 'B': NavDown, 'j': NavDown, 'D': NavBack}

// Decodes the bytes after an ESC, a lone ESC cancelling
func decodeEscapeSequence(scr Screen) (string, error) {
	nxt, err := scr.Getch()
	if err != nil {
		return "", err
	}
	if nxt == -1 {
		return NavCancel, nil
	}
	if nxt != '[' && nxt != 'O' {
		return NavNone, nil
	}
	raw, final, err := readCSITail(scr)
	if err != nil {
		return "", err
	}
	if a, ok := csiFinalNav[final]; ok {
		return a, nil
	}
	if final == 'u' {
		if cp, mod, ev, ok := parseCSIUKey(raw); ok {
			if ev == 3 {
				return NavNone, nil
			}
			return enhancedKeyAction(cp, mod), nil
		}
	}
	if final == '~' {
		if p := parseCSINumbers(raw); len(p) >= 3 && p[0] == 27 {
			return enhancedKeyAction(p[2], p[1]), nil
		}
	}
	return NavNone, nil
}

var plainMenuKeys = map[int]string{KeyUp: NavUp, 'k': NavUp, KeyDown: NavDown, 'j': NavDown, KeyLeft: NavBack, 3: NavInterrupt,
	4: NavCancel, KeyEnter: NavSelect, 10: NavSelect, 13: NavSelect, ' ': NavToggle, 'q': NavCancel}

// Normalizes an already read key, waiting briefly after ESC for a continuation
func decodeMenuKey(scr Screen, key int) (string, error) {
	if a, ok := plainMenuKeys[key]; ok {
		return a, nil
	}
	if key != 27 {
		return NavNone, nil
	}
	scr.Timeout(60)
	defer scr.Timeout(-1)
	return decodeEscapeSequence(scr)
}

// Marker results of the menu loop
type menuMarker int

const (
	keepMarker menuMarker = iota
	consumedMarker
)

var resolving = map[string]bool{NavSelect: true, NavToggle: true, NavCancel: true, NavInterrupt: true, NavBack: true}

// Turns a raw key into an action honoring an active search prompt, consumed meaning redraw only
func routeKey(scr Screen, key int, search *SearchState, useSearch bool) (string, bool, bool, error) {
	if !useSearch {
		a, err := decodeMenuKey(scr, key)
		return a, false, false, err
	}
	if search.Active && key == 27 {
		a, err := decodeMenuKey(scr, key)
		if err != nil {
			return "", false, false, err
		}
		if a == NavCancel {
			search.Active, search.Query = false, ""
			return "", true, true, nil
		}
		return a, a == NavNone, false, nil
	}
	if search.Active {
		handled, confirm, changed := HandleActiveSearchKey(key, search)
		if confirm {
			return NavSelect, false, changed, nil
		}
		if handled {
			return "", true, changed, nil
		}
		a, err := decodeMenuKey(scr, key)
		return a, false, changed, err
	}
	if key == '/' {
		search.Active = true
		return "", true, false, nil
	}
	a, err := decodeMenuKey(scr, key)
	return a, false, false, err
}

// Callbacks and options of one full screen menu run
type menuSpec struct {
	initialCursor   int
	itemCount       int
	drawHeader      func(scr Screen, maxY, maxX int, search *SearchState, backEnabled bool) int
	drawRow         func(scr Screen, y, i int, isCursor bool, maxX int)
	onAction        func(action string, cursor int) (any, bool)
	reserveBottom   int
	drawFooter      func(scr Screen, maxY, maxX int)
	extraColorPairs bool
	fallback        func(ctx context.Context) (any, error)
	cancelValue     any
	searchable      bool
	searchLabels    []string
	selectable      func(i int) bool
}

// Shared event loop with TTY guard, screen setup, scroll math, key dispatch and numbered fallback
func (c *Console) runMenu(ctx context.Context, m menuSpec) (any, error) {
	handler := navHandler(ctx)
	notify := func(event MenuNavigationEvent, value ...any) error {
		if handler == nil {
			return nil
		}
		_, err := handler(event, value...)
		return err
	}
	var start *MenuNavigationStart
	if handler != nil {
		s, err := handler(NavEventBegin)
		if err != nil {
			return nil, err
		}
		start = s
	}
	allowBack := start != nil && start.AllowBack
	if start != nil && start.Replay {
		if err := notify(NavEventResolve, start.ReplayValue); err != nil {
			return nil, err
		}
		return start.ReplayValue, nil
	}
	if !c.stdinTTY() {
		return m.cancelValue, nil
	}
	useSearch := m.searchable && m.searchLabels != nil && len(m.searchLabels) == m.itemCount
	runFallback := func() (any, error) {
		result, err := m.fallback(context.WithValue(ctx, numberedBackKey{}, allowBack))
		if err != nil {
			return nil, err
		}
		if err := notify(NavEventResolve, result); err != nil {
			return nil, err
		}
		return result, nil
	}
	open := c.OpenScreen
	if open == nil {
		open = c.openANSIScreen
	}
	scr, closeScreen, err := open()
	if err != nil {
		return runFallback()
	}
	result, resolved, err := c.menuLoop(scr, m, useSearch, allowBack, notify)
	closeScreen()
	if errors.Is(err, errCursesInit) {
		return runFallback()
	}
	if errors.Is(err, ErrInterrupt) {
		if nerr := notify(NavEventCancel); nerr != nil {
			return nil, nerr
		}
		return m.cancelValue, nil
	}
	if err != nil {
		return nil, err
	}
	c.FlushStdin()
	if !resolved {
		return m.cancelValue, nil
	}
	return result, nil
}

// Draw and key loop of one menu, resolved false when it ended without an outcome
func (c *Console) menuLoop(scr Screen, m menuSpec, useSearch, allowBack bool, notify func(MenuNavigationEvent, ...any) error) (any, bool, error) {
	if err := scr.InitColors(m.extraColorPairs); err != nil {
		return nil, false, fmt.Errorf("%w: %v", errCursesInit, err)
	}
	cursor, scroll, search := m.initialCursor, 0, &SearchState{}
	nav := func(filtered []int) []int {
		if m.selectable == nil {
			return filtered
		}
		var out []int
		for _, i := range filtered {
			if m.selectable(i) {
				out = append(out, i)
			}
		}
		return out
	}
	all := make([]int, m.itemCount)
	for i := range all {
		all[i] = i
	}
	for {
		scr.Clear()
		maxY, maxX := scr.GetMaxYX()
		filtered := all
		if useSearch {
			filtered = FilterIndices(m.searchLabels, search.Query)
		}
		navRows := nav(filtered)
		var navPos int
		cursor, navPos = ReconcileCursor(navRows, cursor)
		cursorPos := max(indexOf(filtered, cursor), 0)
		itemsStart := m.drawHeader(scr, maxY, maxX, search, allowBack)
		visible := max(1, maxY-itemsStart-m.reserveBottom)
		scroll = ScrollForCursor(scroll, cursorPos, visible, len(filtered))
		if useSearch && search.Query != "" && len(navRows) == 0 {
			addnstr(scr, itemsStart, 0, "  No matches", maxX-1, Attr{Dim: true})
		}
		end := min(scroll+visible, len(filtered))
		for drawI, i := range filtered[min(scroll, end):end] {
			y := drawI + itemsStart
			if y >= maxY-m.reserveBottom {
				break
			}
			m.drawRow(scr, y, i, i == cursor, maxX)
		}
		if m.drawFooter != nil {
			m.drawFooter(scr, maxY, maxX)
		}
		if err := scr.Refresh(); err != nil {
			return nil, false, err
		}
		key, err := scr.Getch()
		if err != nil {
			return nil, false, err
		}
		action, consumed, changed, err := routeKey(scr, key, search, useSearch)
		if err != nil {
			return nil, false, err
		}
		if changed {
			scroll = 0
			if search.Active {
				cursor, _ = ReconcileCursor(nav(FilterIndices(m.searchLabels, search.Query)), cursor)
			}
		}
		if !consumed && (action == NavUp || action == NavDown) {
			delta := 1
			if action == NavUp {
				delta = -1
			}
			cursor = moveFilteredCursor(navRows, cursor, navPos, delta)
			continue
		}
		if consumed || !resolving[action] || (action == NavBack && !allowBack) || (action == NavSelect && useSearch && len(navRows) == 0) {
			continue
		}
		switch action {
		case NavCancel, NavInterrupt:
			if err := notify(NavEventCancel); err != nil {
				return nil, false, err
			}
		case NavBack:
			if err := notify(NavEventBack); err != nil {
				return nil, false, err
			}
		}
		outcome, keep := m.onAction(action, cursor)
		if !keep {
			if err := notify(NavEventResolve, outcome); err != nil {
				return nil, false, err
			}
			return outcome, true, nil
		}
	}
}

// errCursesInit is a terminal that cannot run the full screen menu, routed to the numbered fallback
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

// CursesChecklist is a multi select checklist, cancel returning cancelReturns or the original selection
func (c *Console) CursesChecklist(ctx context.Context, title string, items []string, selected, cancelReturns map[int]bool, statusFn func(map[int]bool) string) (map[int]bool, error) {
	if cancelReturns == nil {
		cancelReturns = copySet(selected)
	}
	chosen := copySet(selected)
	m := menuSpec{
		initialCursor: 0, itemCount: len(items),
		drawHeader: simpleHeader(title, "SPACE toggle  ENTER confirm", "ESC cancel", false),
		drawRow: func(scr Screen, y, i int, isCursor bool, maxX int) {
			arrow, mark := " ", " "
			if isCursor {
				arrow = "→"
			}
			if chosen[i] {
				mark = "✓"
			}
			drawPlainRow(scr, y, " "+arrow+" ["+mark+"] "+items[i], maxX, isCursor)
		},
		onAction: func(action string, cursor int) (any, bool) {
			switch action {
			case NavToggle:
				if chosen[cursor] {
					delete(chosen, cursor)
				} else {
					chosen[cursor] = true
				}
				return nil, true
			case NavSelect:
				return copySet(chosen), false
			}
			return cancelReturns, false
		},
		reserveBottom: 1, extraColorPairs: statusFn != nil, cancelValue: cancelReturns,
		fallback: func(ctx context.Context) (any, error) {
			return c.numberedFallback(ctx, title, items, selected, cancelReturns, statusFn)
		},
	}
	if statusFn != nil {
		m.reserveBottom = 2
		m.drawFooter = func(scr Screen, maxY, maxX int) {
			status := statusFn(chosen)
			if status != "" {
				sx := max(0, maxX-len([]rune(status))-1)
				a := Attr{Dim: true}
				if scr.HasColors() {
					a.Pair = 3
				}
				addnstr(scr, maxY-1, sx, status, maxX-sx-1, a)
			}
		}
	}
	res, err := c.runMenu(ctx, m)
	if err != nil {
		return nil, err
	}
	set, ok := res.(map[int]bool)
	if !ok {
		return nil, fmt.Errorf("menu navigation replay value %T is not a checklist selection", res)
	}
	return set, nil
}

// Key hint row, swapped for the search prompt while slash search is active
func searchHint(search *SearchState, searchable bool, confirm, cancel string, backEnabled bool) string {
	var hint string
	if searchable && search != nil && search.Active {
		hint = "  Search: " + search.Query + "▎  BACKSPACE edit  Ctrl+U clear  ESC stop"
	} else {
		s := ""
		if searchable {
			s = "/ search  "
		}
		hint = "  ↑↓ navigate  " + confirm + "  " + s + cancel
	}
	if backEnabled {
		hint += "  ← previous"
	}
	return hint
}

func simpleHeader(title, confirm, cancel string, searchable bool) func(Screen, int, int, *SearchState, bool) int {
	return func(scr Screen, maxY, maxX int, search *SearchState, back bool) int {
		drawTitleAndHint(scr, title, searchHint(search, searchable, confirm, cancel, back), maxX, 1)
		return 3
	}
}

// RadiolistOptions are the optional arguments of a radiolist, nil CancelReturns meaning the selection
type RadiolistOptions struct {
	CancelReturns *int
	Description   string
	Searchable    bool
	SearchLabels  []string
	Headings      map[int]string
}

// Header with description lines kept short enough that five choices and the hint stay visible
func radioHeader(title string, descLines []string, rowCount int, searchable bool) func(Screen, int, int, *SearchState, bool) int {
	return func(scr Screen, maxY, maxX int, search *SearchState, back bool) int {
		row := 1
		choiceRows := min(max(1, rowCount), 5)
		descRows := max(0, maxY-4-choiceRows)
		visible := append([]string{}, descLines[:min(descRows, len(descLines))]...)
		if len(descLines) > descRows && len(visible) > 0 {
			hidden := len(descLines) - descRows + 1
			visible[len(visible)-1] = fmt.Sprintf("  ... %d more description lines (enlarge terminal)", hidden)
		}
		for _, d := range visible {
			drawDescriptionLine(scr, row, d, maxX)
			row++
		}
		drawTitleAndHint(scr, title, searchHint(search, searchable, "ENTER/SPACE select", "ESC cancel", back), maxX, row)
		return row + 2
	}
}

func radioPrefix(idx, selected int, isCursor bool) string {
	radio, arrow := "○", " "
	if idx == selected {
		radio = "●"
	}
	if isCursor {
		arrow = "→"
	}
	return " " + arrow + " (" + radio + ") "
}

// CursesRadiolist is a single select radio list returning the original index of the chosen row
func (c *Console) CursesRadiolist(ctx context.Context, title string, items []RadioItem, selected int, opts RadiolistOptions) (int, error) {
	if len(opts.Headings) > 0 {
		return c.radiolistWithHeadings(ctx, title, items, selected, opts)
	}
	cancel := selected
	if opts.CancelReturns != nil {
		cancel = *opts.CancelReturns
	}
	desc := pystr.Splitlines(opts.Description, false)
	var labels []string
	if opts.Searchable {
		labels = opts.SearchLabels
		if labels == nil {
			for _, it := range items {
				labels = append(labels, RadioItemPlain(it))
			}
		}
	}
	m := menuSpec{
		initialCursor: selected, itemCount: len(items),
		drawHeader: radioHeader(title, desc, len(items), opts.Searchable),
		drawRow: func(scr Screen, y, i int, isCursor bool, maxX int) {
			prefix := radioPrefix(i, selected, isCursor)
			drawPlainRow(scr, y, prefix, maxX, isCursor)
			drawRadioItem(scr, y, len([]rune(prefix)), items[i], maxX, isCursor)
		},
		onAction: func(action string, cursor int) (any, bool) {
			if action == NavSelect || action == NavToggle {
				return cursor, false
			}
			return cancel, false
		},
		reserveBottom: 1, extraColorPairs: true, cancelValue: cancel,
		fallback: func(ctx context.Context) (any, error) {
			return c.radioNumberedFallback(ctx, title, items, selected, cancel, nil)
		},
		searchable: opts.Searchable, searchLabels: labels,
	}
	return intResult(c.runMenu(ctx, m))
}

func intResult(res any, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	n, ok := res.(int)
	if !ok {
		return 0, fmt.Errorf("menu navigation replay value %T is not a row index", res)
	}
	return n, nil
}

// Radiolist with dim unselectable section titles above the given items
func (c *Console) radiolistWithHeadings(ctx context.Context, title string, items []RadioItem, selected int, opts RadiolistOptions) (int, error) {
	cancel := selected
	if opts.CancelReturns != nil {
		cancel = *opts.CancelReturns
	}
	type row struct {
		heading bool
		idx     int
	}
	var rows []row
	rowOfItem := map[int]int{}
	for i := range items {
		if _, ok := opts.Headings[i]; ok {
			rows = append(rows, row{true, i})
		}
		rowOfItem[i] = len(rows)
		rows = append(rows, row{false, i})
	}
	labels := opts.SearchLabels
	if labels == nil {
		for _, it := range items {
			labels = append(labels, RadioItemPlain(it))
		}
	}
	rowLabels := make([]string, len(rows))
	for r, rw := range rows {
		if !rw.heading {
			rowLabels[r] = labels[rw.idx]
		}
	}
	initial := 0
	if r, ok := rowOfItem[selected]; ok {
		initial = r
	}
	m := menuSpec{
		initialCursor: initial, itemCount: len(rows),
		drawHeader: radioHeader(title, pystr.Splitlines(opts.Description, false), len(rows), opts.Searchable),
		drawRow: func(scr Screen, y, r int, isCursor bool, maxX int) {
			rw := rows[r]
			if rw.heading {
				addnstr(scr, y, 0, "   "+opts.Headings[rw.idx], maxX-1, Attr{Dim: true, Bold: true})
				return
			}
			prefix := radioPrefix(rw.idx, selected, isCursor)
			drawPlainRow(scr, y, prefix, maxX, isCursor)
			drawRadioItem(scr, y, len([]rune(prefix)), items[rw.idx], maxX, isCursor)
		},
		onAction: func(action string, cursor int) (any, bool) {
			if action == NavSelect || action == NavToggle {
				return rows[cursor].idx, false
			}
			return cancel, false
		},
		reserveBottom: 1, extraColorPairs: true, cancelValue: cancel,
		fallback: func(ctx context.Context) (any, error) {
			return c.radioNumberedFallback(ctx, title, items, selected, cancel, opts.Headings)
		},
		searchable: opts.Searchable,
		selectable: func(r int) bool { return !rows[r].heading },
	}
	if opts.Searchable {
		m.searchLabels = rowLabels
	}
	return intResult(c.runMenu(ctx, m))
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

// Numbered choice as a zero based index, ok false on empty input, abort on cancel, back, interrupt or junk
func (c *Console) readNumberedChoice(ctx context.Context, prompt string) (idx int, empty, abort bool, err error) {
	val, nav, rerr := c.readNumberedInput(ctx, prompt)
	if errors.Is(rerr, ErrInterrupt) || errors.Is(rerr, ErrEOF) {
		return 0, false, true, notifyScoped(ctx, NavEventCancel)
	}
	if rerr != nil {
		return 0, false, false, rerr
	}
	switch nav {
	case navCancelKey:
		return 0, false, true, notifyScoped(ctx, NavEventCancel)
	case navBackKey:
		return 0, false, true, notifyScoped(ctx, NavEventBack)
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

// Plain input outside setup flows, an escapable line editor inside a scoped flow
func (c *Console) readNumberedInput(ctx context.Context, prompt string) (string, numberedNav, error) {
	if navHandler(ctx) == nil {
		s, err := c.input(prompt)
		return s, navNone, err
	}
	text, nav, err := c.editLine(prompt, &editorBindings{cancel: true, back: numberedBackEnabled(ctx)})
	if errors.Is(err, errEditorUnavailable) {
		return "", navNone, fmt.Errorf("scoped numbered prompt needs a terminal: %w", err)
	}
	return text, nav, err
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

// CursesSingleSelect picks one item or the Cancel row, ok false on cancel
func (c *Console) CursesSingleSelect(ctx context.Context, title string, items []string, defaultIndex int, cancelLabel string, searchable bool) (int, bool, error) {
	all := append(append([]string{}, items...), cancelLabel)
	cancelIdx := len(items)
	m := menuSpec{
		initialCursor: min(defaultIndex, len(all)-1), itemCount: len(all),
		drawHeader: simpleHeader(title, "ENTER confirm", "ESC/q cancel", searchable),
		drawRow: func(scr Screen, y, i int, isCursor bool, maxX int) {
			arrow := " "
			if isCursor {
				arrow = "→"
			}
			drawPlainRow(scr, y, " "+arrow+" "+all[i], maxX, isCursor)
		},
		onAction: func(action string, cursor int) (any, bool) {
			switch action {
			case NavSelect:
				if cursor >= cancelIdx {
					return nil, false
				}
				return cursor, false
			case NavCancel, NavInterrupt:
				return nil, false
			}
			return nil, true
		},
		reserveBottom: 1, cancelValue: nil,
		fallback: func(ctx context.Context) (any, error) {
			return c.numberedSingleFallback(ctx, title, all, cancelIdx)
		},
		searchable: searchable,
	}
	if searchable {
		m.searchLabels = all
	}
	res, err := c.runMenu(ctx, m)
	if err != nil {
		return 0, false, err
	}
	if res == nil {
		return 0, false, nil
	}
	n, ok := res.(int)
	if !ok {
		return 0, false, fmt.Errorf("menu navigation replay value %T is not a row index", res)
	}
	return n, true, nil
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

// Text toggle fallback for terminals without the full screen menu
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
