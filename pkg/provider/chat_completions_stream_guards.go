package provider

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ---- message content flattening ----

var nonTextPartTypes = map[string]bool{"image": true, "image_url": true, "input_image": true, "audio": true, "input_audio": true}
var textPartKeys = []string{"text", "content", "input_text", "output_text", "summary_text"}

// Visible text of one content part, empty for media parts
func textFromPart(part any) string {
	switch p := part.(type) {
	case nil:
		return ""
	case string:
		return p
	case map[string]any:
		if nonTextPartTypes[strings.ToLower(strings.TrimSpace(stringOf(p["type"])))] {
			return ""
		}
		for _, k := range textPartKeys {
			if s, ok := p[k].(string); ok {
				return s
			}
		}
	}
	return ""
}

// Visible text of chat or Responses message content shapes joined with sep
func flattenMessageText(content any, sep string) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if t := textFromPart(p); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, sep)
	case []map[string]any:
		var parts []string
		for _, p := range c {
			if t := textFromPart(p); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, sep)
	}
	if t := textFromPart(content); t != "" {
		return t
	}
	b, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprint(content)
	}
	return string(b)
}

// FlattenMessageText exports the visible text join of message content shapes
func FlattenMessageText(content any, sep string) string { return flattenMessageText(content, sep) }

// ---- streamed tool call accumulator ----

// One assembled tool call slot of a streamed chat completion
type ccToolCallEntry struct {
	ID               string
	Type             string
	Name             string
	Arguments        string
	ExtraContent     any
	ThoughtSignature string
}

type ccArgScan struct {
	done     int
	depth    int
	inString bool
	escaped  bool
	started  bool
}

// Assembles streamed tool call deltas into complete entries keyed by slot
type ccToolCallAccumulator struct {
	acc             map[int]*ccToolCallEntry
	notified        map[int]bool
	lastIDAtIdx     map[int]string
	activeSlotByIdx map[int]int
	argParts        map[int][]string
	argScan         map[int]ccArgScan
}

func newCCToolCallAccumulator() *ccToolCallAccumulator {
	return &ccToolCallAccumulator{acc: map[int]*ccToolCallEntry{}, notified: map[int]bool{}, lastIDAtIdx: map[int]string{},
		activeSlotByIdx: map[int]int{}, argParts: map[int][]string{}, argScan: map[int]ccArgScan{}}
}

// Len reports how many slots opened so far
func (a *ccToolCallAccumulator) Len() int { return len(a.acc) }

// Slots lists the slot indexes in ascending order
func (a *ccToolCallAccumulator) Slots() []int {
	out := make([]int, 0, len(a.acc))
	for idx := range a.acc {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

// Materialize joins the buffered argument fragments into each entry and returns the slots
func (a *ccToolCallAccumulator) Materialize() map[int]*ccToolCallEntry {
	for idx, parts := range a.argParts {
		a.acc[idx].Arguments = strings.Join(parts, "")
	}
	return a.acc
}

// Reports whether a named delta at a reused index opens a new call rather than continuing the slot
func (a *ccToolCallAccumulator) startsNewCall(name string, slot int) bool {
	if name == "" {
		return false
	}
	if cur, ok := a.acc[slot]; !ok || name != cur.Name {
		return true
	}
	return a.argumentsClosed(slot)
}

// Reports structural completion of a slot's arguments, brackets balanced outside strings
func (a *ccToolCallAccumulator) argumentsClosed(slot int) bool {
	parts := a.argParts[slot]
	sc := a.argScan[slot]
	for _, ch := range []byte(strings.Join(parts[sc.done:], "")) {
		if sc.inString {
			if sc.escaped {
				sc.escaped = false
			} else if ch == '\\' {
				sc.escaped = true
			} else if ch == '"' {
				sc.inString = false
			}
		} else if ch == '"' {
			sc.inString = true
		} else if ch == '{' || ch == '[' {
			sc.depth++
			sc.started = true
		} else if ch == '}' || ch == ']' {
			sc.depth--
		}
	}
	sc.done = len(parts)
	a.argScan[slot] = sc
	return sc.started && sc.depth == 0 && !sc.inString
}

// Tool call id of a delta, integer ids rendered as text
func ccDeltaID(v any) string {
	switch id := v.(type) {
	case string:
		return id
	case float64:
		if id == math.Trunc(id) {
			return strconv.FormatInt(int64(id), 10)
		}
		return strconv.FormatFloat(id, 'f', -1, 64)
	case json.Number:
		return id.String()
	case int:
		return strconv.Itoa(id)
	case int64:
		return strconv.FormatInt(id, 10)
	}
	return ""
}

// Feed merges one delta and returns the tool name the first time a slot's name is known
func (a *ccToolCallAccumulator) Feed(delta map[string]any) (string, int) {
	rawIdx := 0
	if n, ok := anyInt(delta["index"]); ok {
		rawIdx = n
	}
	tcID := ccDeltaID(delta["id"])
	fn, _ := delta["function"].(map[string]any)
	fnName := stringOf(fn["name"])
	if _, ok := a.activeSlotByIdx[rawIdx]; !ok {
		a.activeSlotByIdx[rawIdx] = rawIdx
	}
	if last, seen := a.lastIDAtIdx[rawIdx]; tcID != "" && seen && tcID != last && a.startsNewCall(fnName, a.activeSlotByIdx[rawIdx]) {
		next := -1
		for idx := range a.acc {
			if idx > next {
				next = idx
			}
		}
		a.activeSlotByIdx[rawIdx] = next + 1
	}
	if tcID != "" {
		a.lastIDAtIdx[rawIdx] = tcID
	}
	idx := a.activeSlotByIdx[rawIdx]
	entry, ok := a.acc[idx]
	if !ok {
		entry = &ccToolCallEntry{ID: tcID, Type: "function"}
		a.acc[idx] = entry
	}
	if _, ok := a.argParts[idx]; !ok {
		a.argParts[idx] = nil
	}
	if tcID != "" {
		entry.ID = tcID
	}
	if fn != nil {
		if fnName != "" {
			entry.Name = fnName
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			a.argParts[idx] = append(a.argParts[idx], args)
		}
		if sig, ok := fn["thought_signature"].(string); ok && sig != "" {
			entry.ThoughtSignature = sig
		}
	}
	if extra, ok := delta["extra_content"]; ok && extra != nil {
		entry.ExtraContent = extra
	}
	if entry.Name != "" && !a.notified[idx] {
		a.notified[idx] = true
		return entry.Name, idx
	}
	return "", idx
}

// ---- streamed reasoning details ----

var mergeableDetailTextKeys = map[string]string{"reasoning.text": "text", "reasoning.summary": "summary"}
var backfillDetailKeys = []string{"signature", "id", "format", "index"}

// Readable text of a reasoning detail delta, never opaque replay material
func streamedReasoningDetailText(detail any) string {
	dm, ok := detail.(map[string]any)
	if !ok {
		return ""
	}
	key, ok := mergeableDetailTextKeys[stringOf(dm["type"])]
	if !ok {
		return ""
	}
	text, _ := dm[key].(string)
	return text
}

func detailValueEmpty(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// Accumulates one streamed reasoning_details entry, merging consecutive text and summary fragments
func appendStreamedReasoningDetail(acc []map[string]any, detail any) []map[string]any {
	dm, ok := detail.(map[string]any)
	if !ok {
		return acc
	}
	dtype := stringOf(dm["type"])
	mergeKey, mergeable := mergeableDetailTextKeys[dtype]
	if len(acc) > 0 && mergeable {
		last := acc[len(acc)-1]
		if frag, isStr := dm[mergeKey].(string); isStr && stringOf(last["type"]) == dtype {
			prev, _ := last[mergeKey].(string)
			last[mergeKey] = prev + frag
			for _, k := range backfillDetailKeys {
				if detailValueEmpty(last[k]) && !detailValueEmpty(dm[k]) {
					last[k] = dm[k]
				}
			}
			return acc
		}
	}
	cp := make(map[string]any, len(dm))
	for k, v := range dm {
		cp[k] = v
	}
	return append(acc, cp)
}

// Delta prefixed with a paragraph break when it opens a closed bold heading onto mid line text
func separateGluedReasoningBlocks(previous string, delta any) string {
	text := flattenMessageText(delta, "")
	if previous == "" || text == "" {
		return text
	}
	runes := []rune(previous)
	if unicode.IsSpace(runes[len(runes)-1]) {
		return text
	}
	if strings.HasPrefix(text, "**") && strings.Contains(text[2:], "**") {
		return "\n\n" + text
	}
	return text
}

// ---- streaming think scrubber ----

var thinkOpenTags, thinkCloseTags, thinkAllTags [][]rune
var thinkMaxTagLen int
var orphanThinkCloseRe *regexp.Regexp

func init() {
	var closeAlts []string
	for _, name := range ThinkTagNames {
		lower := strings.ToLower(name)
		thinkOpenTags = append(thinkOpenTags, []rune("<"+lower+">"))
		thinkCloseTags = append(thinkCloseTags, []rune("</"+lower+">"))
		closeAlts = append(closeAlts, regexp.QuoteMeta("</"+lower+">"))
	}
	thinkAllTags = append(append([][]rune{}, thinkOpenTags...), thinkCloseTags...)
	for _, t := range thinkAllTags {
		if len(t) > thinkMaxTagLen {
			thinkMaxTagLen = len(t)
		}
	}
	orphanThinkCloseRe = regexp.MustCompile("(?i)(?:" + strings.Join(closeAlts, "|") + ")[ \t\n\r]*")
}

// StreamingThinkScrubber hides reasoning tag blocks from streamed assistant text across delta boundaries
type StreamingThinkScrubber struct {
	inBlock                 bool
	buf                     []rune
	lastEmittedEndedNewline bool
	lastHidden              string
}

func NewStreamingThinkScrubber() *StreamingThinkScrubber {
	s := &StreamingThinkScrubber{}
	s.Reset()
	return s
}

// Reset clears every state for a new turn
func (s *StreamingThinkScrubber) Reset() {
	s.inBlock = false
	s.buf = nil
	s.lastEmittedEndedNewline = true
	s.lastHidden = ""
}

// LastHidden is the reasoning text the most recent Feed stripped from inside blocks
func (s *StreamingThinkScrubber) LastHidden() string { return s.lastHidden }

func lowerRunes(r []rune) []rune {
	out := make([]rune, len(r))
	for i, c := range r {
		out[i] = unicode.ToLower(c)
	}
	return out
}

func runeIndex(hay, needle []rune, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func runeLastIndexRune(hay []rune, c rune, from, to int) int {
	for i := to - 1; i >= from; i-- {
		if hay[i] == c {
			return i
		}
	}
	return -1
}

func (s *StreamingThinkScrubber) emit(out *[]string, text []rune) {
	t := stripOrphanThinkCloseTags(string(text))
	if t != "" {
		*out = append(*out, t)
		s.lastEmittedEndedNewline = strings.HasSuffix(t, "\n")
	}
}

// Feed scrubs one delta and returns the visible portion, empty when it is all reasoning or held back
func (s *StreamingThinkScrubber) Feed(text string) string {
	s.lastHidden = ""
	if text == "" {
		return ""
	}
	buf := append(append([]rune{}, s.buf...), []rune(text)...)
	s.buf = nil
	var out []string
	var hidden []string
	for len(buf) > 0 {
		if s.inBlock {
			closeIdx, closeLen := findFirstThinkTag(buf, thinkCloseTags)
			if closeIdx == -1 {
				hidden = append(hidden, string(s.holdPartial(buf, thinkCloseTags)))
				break
			}
			hidden = append(hidden, string(buf[:closeIdx]))
			buf = buf[closeIdx+closeLen:]
			s.inBlock = false
			continue
		}
		pairStart, pairEnd, hasPair := findEarliestClosedThinkPair(buf)
		openIdx, openLen := s.findOpenAtBoundary(buf, out)
		if hasPair && (openIdx == -1 || pairStart <= openIdx) {
			s.emit(&out, buf[:pairStart])
			innerStart := runeIndex(buf, []rune{'>'}, pairStart) + 1
			innerEnd := runeLastIndexRune(buf, '<', pairStart, pairEnd)
			hidden = append(hidden, string(buf[innerStart:innerEnd]))
			buf = buf[pairEnd:]
			continue
		}
		if openIdx != -1 {
			s.emit(&out, buf[:openIdx])
			s.inBlock = true
			buf = buf[openIdx+openLen:]
			continue
		}
		s.emit(&out, s.holdPartial(buf, thinkAllTags))
		break
	}
	s.lastHidden = strings.Join(hidden, "")
	return strings.Join(out, "")
}

// Moves a trailing partial tag prefix into the hold buffer and returns the remainder
func (s *StreamingThinkScrubber) holdPartial(buf []rune, tags [][]rune) []rune {
	held := maxPartialThinkSuffix(buf, tags)
	if held == 0 {
		s.buf = nil
		return buf
	}
	s.buf = append([]rune{}, buf[len(buf)-held:]...)
	return buf[:len(buf)-held]
}

// Flush releases held prose at stream end, discarding an unterminated block, and re arms the boundary
func (s *StreamingThinkScrubber) Flush() string {
	tail := ""
	if !s.inBlock {
		tail = string(s.buf)
	}
	s.buf = nil
	s.inBlock = false
	s.lastEmittedEndedNewline = true
	if tail == "" {
		return ""
	}
	return stripOrphanThinkCloseTags(tail)
}

// Earliest index and length over the tags, case insensitive, or -1
func findFirstThinkTag(buf []rune, tags [][]rune) (int, int) {
	lower := lowerRunes(buf)
	bestIdx, bestLen := -1, 0
	for _, tag := range tags {
		idx := runeIndex(lower, tag, 0)
		if idx == -1 {
			continue
		}
		if bestIdx == -1 || idx < bestIdx || (idx == bestIdx && len(tag) < bestLen) {
			bestIdx, bestLen = idx, len(tag)
		}
	}
	return bestIdx, bestLen
}

// Start and end of the earliest closed tag pair in the buffer
func findEarliestClosedThinkPair(buf []rune) (int, int, bool) {
	lower := lowerRunes(buf)
	bestStart, bestEnd, found := 0, 0, false
	for i := range thinkOpenTags {
		openIdx := runeIndex(lower, thinkOpenTags[i], 0)
		if openIdx == -1 {
			continue
		}
		closeIdx := runeIndex(lower, thinkCloseTags[i], openIdx+len(thinkOpenTags[i]))
		if closeIdx == -1 {
			continue
		}
		end := closeIdx + len(thinkCloseTags[i])
		if !found || openIdx < bestStart || (openIdx == bestStart && end < bestEnd) {
			bestStart, bestEnd, found = openIdx, end, true
		}
	}
	return bestStart, bestEnd, found
}

// Earliest open tag sitting at a block boundary, or -1
func (s *StreamingThinkScrubber) findOpenAtBoundary(buf []rune, emitted []string) (int, int) {
	lower := lowerRunes(buf)
	bestIdx, bestLen := -1, 0
	for _, tag := range thinkOpenTags {
		idx := runeIndex(lower, tag, 0)
		for idx != -1 && !s.isBlockBoundary(buf, idx, emitted) {
			idx = runeIndex(lower, tag, idx+1)
		}
		if idx == -1 {
			continue
		}
		if bestIdx == -1 || idx < bestIdx || (idx == bestIdx && len(tag) < bestLen) {
			bestIdx, bestLen = idx, len(tag)
		}
	}
	return bestIdx, bestLen
}

// Reports whether the index starts a line whose preceding text is whitespace only
func (s *StreamingThinkScrubber) isBlockBoundary(buf []rune, idx int, emitted []string) bool {
	priorNewline := s.lastEmittedEndedNewline
	if len(emitted) > 0 {
		priorNewline = strings.HasSuffix(emitted[len(emitted)-1], "\n")
	}
	if idx == 0 {
		return priorNewline
	}
	preceding := buf[:idx]
	lastNL := runeLastIndexRune(preceding, '\n', 0, len(preceding))
	ok := priorNewline
	if lastNL != -1 {
		ok = true
	}
	return ok && strings.TrimSpace(string(preceding[lastNL+1:])) == ""
}

// Longest buffer suffix that is a strict prefix of any tag
func maxPartialThinkSuffix(buf []rune, tags [][]rune) int {
	lower := lowerRunes(buf)
	limit := thinkMaxTagLen - 1
	if len(lower) < limit {
		limit = len(lower)
	}
	for i := limit; i > 0; i-- {
		suffix := lower[len(lower)-i:]
		for _, tag := range tags {
			if len(tag) > i && runeIndex(tag[:i], suffix, 0) == 0 {
				return i
			}
		}
	}
	return 0
}

// Removes close tags with no matching open plus trailing whitespace
func stripOrphanThinkCloseTags(text string) string {
	if !strings.Contains(text, "</") {
		return text
	}
	return orphanThinkCloseRe.ReplaceAllString(text, "")
}

// ---- repetition guard ----

// Fragments shorter than this are never judged for repetition
const MinFragmentLength = 400

const repeatWindow = 60
const minRepeatCount = 5
const dominanceRatio = 0.5

// Interrupt checkpoint text used instead of a repetition dominated partial
const RepetitionLoopInterrupted = "[the reply degenerated into a repetition loop and was interrupted]"

const maxAnchorSamples = 32
const maxAnchorMatches = 8
const runawayDistinctLineRatio = 0.5

// Stop path scale floor below which asked for repeats are delivered
const StopPathMinChars = 16000

const streamTailChars = 4 * StopPathMinChars

var pythonLineBreaks = map[rune]bool{'\n': true, '\r': true, '\v': true, '\f': true, 0x1c: true, 0x1d: true, 0x1e: true, 0x85: true, 0x2028: true, 0x2029: true}

// Splits text on every line boundary Python splitlines recognizes
func splitLinesPython(r []rune) [][]rune {
	var lines [][]rune
	start := 0
	for i := 0; i < len(r); i++ {
		if !pythonLineBreaks[r[i]] {
			continue
		}
		lines = append(lines, r[start:i])
		if r[i] == '\r' && i+1 < len(r) && r[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(r) {
		lines = append(lines, r[start:])
	}
	return lines
}

// IsRepetitionDominated reports a contiguous run of exact repeats covering at least half the text
func IsRepetitionDominated(text string) bool {
	r := []rune(text)
	n := len(r)
	if n < MinFragmentLength {
		return false
	}
	if lineRepetitionDominated(r, n) {
		return true
	}
	return windowCountDominated(r, n) || periodicRunDominated(r, n)
}

// Reports one 60 rune window recurring often enough to cover half the text
func windowCountDominated(r []rune, n int) bool {
	needed := int(math.Ceil(float64(n) * dominanceRatio / repeatWindow))
	if needed < minRepeatCount {
		needed = minRepeatCount
	}
	counts := map[string]int{}
	for i := 0; i+repeatWindow <= n; i++ {
		key := string(r[i : i+repeatWindow])
		c := counts[key] + 1
		if c >= needed {
			return true
		}
		counts[key] = c
	}
	return false
}

// Detects a dominant exact periodic run from evenly spaced anchors
func periodicRunDominated(r []rune, n int) bool {
	maxStart := n - repeatWindow
	if maxStart < 1 {
		return false
	}
	step := (maxStart + maxAnchorSamples - 2) / (maxAnchorSamples - 1)
	if step < 1 {
		step = 1
	}
	var starts []int
	for s := 0; s <= maxStart; s += step {
		starts = append(starts, s)
	}
	if starts[len(starts)-1] != maxStart {
		starts = append(starts, maxStart)
	}
	type run struct{ left, right, period int }
	var rejected []run
	for _, start := range starts {
		anchor := r[start : start+repeatWindow]
		searchFrom := start + 1
		for k := 0; k < maxAnchorMatches; k++ {
			match := runeIndex(r, anchor, searchFrom)
			if match < 0 {
				break
			}
			period := match - start
			searchFrom = match + 1
			skip := false
			for _, rj := range rejected {
				if rj.left <= start && start < rj.right && period%rj.period == 0 {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			left, right := expandPeriodicRun(r, n, start, period, repeatWindow)
			if right-left >= minRepeatCount*period && float64(right-left) >= float64(n)*dominanceRatio {
				return true
			}
			rejected = append(rejected, run{left, right, period})
		}
	}
	return false
}

// Expands one equal window to the bounds of its exact periodic run
func expandPeriodicRun(r []rune, n, start, period, matched int) (int, int) {
	left := start
	for left > 0 && r[left-1] == r[left-1+period] {
		left--
	}
	right := start + matched
	for right+period < n && r[right] == r[right+period] {
		right++
	}
	return left, right + period
}

// Reports one normalized line covering half the text through repeats
func lineRepetitionDominated(r []rune, n int) bool {
	counts := map[string]int{}
	for _, line := range splitLinesPython(r) {
		norm := strings.TrimSpace(string(line))
		if norm != "" {
			counts[norm]++
		}
	}
	for line, c := range counts {
		if c >= minRepeatCount && float64(c*len([]rune(line))) >= float64(n)*dominanceRatio {
			return true
		}
	}
	return false
}

// IsRunawayRepetition requires domination plus mostly duplicate lines when the text has line structure
func IsRunawayRepetition(text string) bool {
	if !IsRepetitionDominated(text) {
		return false
	}
	var lines []string
	for _, line := range splitLinesPython([]rune(text)) {
		if norm := strings.TrimSpace(string(line)); norm != "" {
			lines = append(lines, norm)
		}
	}
	if len(lines) < minRepeatCount {
		return true
	}
	distinct := map[string]bool{}
	for _, l := range lines {
		distinct[l] = true
	}
	return float64(len(distinct)) <= float64(len(lines))*runawayDistinctLineRatio
}

// RunawayStreamWatch applies the stop path repetition criterion while one channel streams
type RunawayStreamWatch struct {
	parts     []string
	chars     int
	nextCheck int
}

func NewRunawayStreamWatch() *RunawayStreamWatch {
	return &RunawayStreamWatch{nextCheck: StopPathMinChars}
}

// Feed adds one delta and reports once the channel has become a runaway loop
func (w *RunawayStreamWatch) Feed(text string) bool {
	if text == "" {
		return false
	}
	if w.nextCheck == 0 {
		w.nextCheck = StopPathMinChars
	}
	w.parts = append(w.parts, text)
	w.chars += len([]rune(text))
	if w.chars < w.nextCheck {
		return false
	}
	gap := w.chars
	if gap > streamTailChars {
		gap = streamTailChars
	}
	w.nextCheck = w.chars + gap
	joined := []rune(strings.Join(w.parts, ""))
	if len(joined) > streamTailChars {
		joined = joined[len(joined)-streamTailChars:]
	}
	tail := string(joined)
	w.parts = []string{tail}
	return IsRunawayRepetition(tail)
}

// ---- tool call argument repair ----

const fullArgsLogBound = 100000

var trailingCommaRe = regexp.MustCompile(`,\s*([}\]])`)

func jsonLoadsOK(text string) bool {
	return json.Valid([]byte(text))
}

// Escapes literal control characters inside JSON string values as unicode escapes
func escapeInvalidCharsInJSONStrings(raw string) string {
	var out strings.Builder
	inString := false
	r := []rune(raw)
	for i := 0; i < len(r); i++ {
		ch := r[i]
		if inString && ch == '\\' && i+1 < len(r) {
			out.WriteRune(ch)
			out.WriteRune(r[i+1])
			i++
			continue
		}
		if ch == '"' {
			inString = !inString
		}
		if inString && ch < 0x20 {
			fmt.Fprintf(&out, "\\u%04x", ch)
		} else {
			out.WriteRune(ch)
		}
	}
	return out.String()
}

var jsonClosers = map[byte]byte{'{': '}', '[': ']'}

// Closes a JSON prefix's open structures in stack order, false when it ends inside a string
func rebalanceJSONClosers(raw string) (string, bool) {
	var out strings.Builder
	var stack []byte
	inString := false
	b := []byte(raw)
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if inString {
			if ch == '\\' {
				out.WriteByte(ch)
				if i+1 < len(b) {
					out.WriteByte(b[i+1])
				}
				i++
				continue
			}
			if ch == '"' {
				inString = false
			}
		} else if ch == '"' {
			inString = true
		} else if _, open := jsonClosers[ch]; open {
			stack = append(stack, ch)
		} else if ch == '}' || ch == ']' {
			matches := false
			for _, o := range stack {
				if jsonClosers[o] == ch {
					matches = true
					break
				}
			}
			if matches {
				for jsonClosers[stack[len(stack)-1]] != ch {
					out.WriteByte(jsonClosers[stack[len(stack)-1]])
					stack = stack[:len(stack)-1]
				}
				stack = stack[:len(stack)-1]
			}
		}
		out.WriteByte(ch)
	}
	if inString {
		return "", false
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out.WriteByte(jsonClosers[stack[i]])
	}
	return out.String(), true
}

// RepairToolCallArguments fixes truncated, trailing comma, or control character argument JSON, else returns {}
func RepairToolCallArguments(rawArgs, toolName string) string {
	if toolName == "" {
		toolName = "?"
	}
	stripped := pystr.Strip(rawArgs)
	if stripped == "" {
		catalogLog.Warn(fmt.Sprintf("Sanitized empty tool_call arguments for %s", toolName))
		return "{}"
	}
	if stripped == "None" {
		catalogLog.Warn(fmt.Sprintf("Sanitized Python-None tool_call arguments for %s", toolName))
		return "{}"
	}
	if parsed, err := pyjson.LoadsLenient(stripped); err == nil {
		if reserialised, err := pyjson.Dumps(parsed, pyjson.Separators(",", ":")); err == nil {
			if reserialised != stripped {
				catalogLog.Warn(fmt.Sprintf("Repaired unescaped control chars in tool_call arguments for %s", toolName))
			}
			return reserialised
		}
	}
	fixed := trailingCommaRe.ReplaceAllString(stripped, "$1")
	if balanced, ok := rebalanceJSONClosers(fixed); ok && balanced != "" {
		fixed = balanced
	}
	for i := 0; i < 50; i++ {
		excessBrace := strings.HasSuffix(fixed, "}") && strings.Count(fixed, "}") > strings.Count(fixed, "{")
		excessBracket := strings.HasSuffix(fixed, "]") && strings.Count(fixed, "]") > strings.Count(fixed, "[")
		if jsonLoadsOK(fixed) || !(excessBrace || excessBracket) {
			break
		}
		fixed = fixed[:len(fixed)-1]
	}
	if jsonLoadsOK(fixed) {
		catalogLog.Warn(fmt.Sprintf("Repaired malformed tool_call arguments for %s: %s → %s", toolName, truncate(stripped, 80), truncate(fixed, 80)))
		return fixed
	}
	escaped := escapeInvalidCharsInJSONStrings(fixed)
	if escaped != fixed && jsonLoadsOK(escaped) {
		catalogLog.Warn(fmt.Sprintf("Repaired control-char-laced tool_call arguments for %s: %s → %s", toolName, truncate(stripped, 80), truncate(escaped, 80)))
		return escaped
	}
	catalogLog.Warn(fmt.Sprintf("Unrepairable tool_call arguments for %s — replaced with empty object (was: %s)", toolName, truncate(stripped, fullArgsLogBound)))
	return "{}"
}

// ---- stream lifetime cap ----

// StreamLifetimeExceededError is a stream attempt cut by the agent.stream_max_lifetime cap
type StreamLifetimeExceededError struct {
	Elapsed float64
	Cap     float64
	Cause   error
}

// Transport error of the cut attempt the cap error replaced
func (e *StreamLifetimeExceededError) Unwrap() error { return e.Cause }

func (e *StreamLifetimeExceededError) Error() string {
	return fmt.Sprintf("Response still streaming after %.0fs, cut by agent.stream_max_lifetime (%gs). Raise agent.stream_max_lifetime, or set it to 0 to turn the cap off, if this model legitimately streams this long.", e.Elapsed, e.Cap)
}

// Timeout marks the cap kill as a timeout for transport error classification
func (e *StreamLifetimeExceededError) Timeout() bool { return true }

// Lifetime cap in seconds, zero when off, floored at a finite stale timeout
func resolveStreamMaxLifetime(configured, stale float64) float64 {
	if configured <= 0 || math.IsInf(configured, 0) || math.IsNaN(configured) {
		return 0
	}
	if !math.IsInf(stale, 0) && !math.IsNaN(stale) && stale > configured {
		return stale
	}
	return configured
}

// ---- llama server prompt progress ----

var promptProgressContentFields = []string{"content", "tool_calls", "reasoning_content", "reasoning", "refusal"}

// Copy of a request body asking llama server for prompt progress chunks
func withPromptProgress(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	if _, ok := out["return_progress"]; !ok {
		out["return_progress"] = true
	}
	return out
}

// Progress payload of a progress only chunk, nil for a chunk carrying content or a finish
func promptProgress(chunk map[string]any) map[string]any {
	progress, ok := chunk["prompt_progress"].(map[string]any)
	if !ok {
		return nil
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		if fr := choice["finish_reason"]; fr != nil && fr != "" {
			return nil
		}
		delta, _ := choice["delta"].(map[string]any)
		for _, f := range promptProgressContentFields {
			switch v := delta[f].(type) {
			case nil:
			case string:
				if v != "" {
					return nil
				}
			case []any:
				if len(v) > 0 {
					return nil
				}
			default:
				return nil
			}
		}
	}
	return progress
}

// Share of the prompt read so far as a percentage
func promptProgressPercent(progress map[string]any) (int, bool) {
	total, okT := anyInt(progress["total"])
	processed, okP := anyInt(progress["processed"])
	if !okT || !okP || total <= 0 {
		return 0, false
	}
	pct := int(math.Round(float64(processed) / float64(total) * 100))
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}
