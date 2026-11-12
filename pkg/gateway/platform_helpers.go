package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	DefaultDedupMaxSize    = 2000
	DefaultDedupTTLSeconds = 300.0
)

var dedupNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// TTL bounded cache of seen message ids that also caps its size under load
type MessageDeduplicator struct {
	mu      sync.Mutex
	seen    *pydict.Dict[string, float64]
	maxSize int
	ttl     float64
}

// Empty cache keeping at most maxSize ids for ttlSeconds each
func NewMessageDeduplicator(maxSize int, ttlSeconds float64) *MessageDeduplicator {
	return &MessageDeduplicator{seen: pydict.New[string, float64](), maxSize: maxSize, ttl: ttlSeconds}
}

// True when the id was already seen inside the window, otherwise records it and trims the cache
func (d *MessageDeduplicator) IsDuplicate(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := dedupNow()
	if seenAt, ok := d.seen.Get(msgID); ok {
		if now-seenAt < d.ttl {
			return true
		}
		d.seen.Delete(msgID)
	}
	d.seen.Set(msgID, now)
	if d.seen.Len() <= d.maxSize {
		return false
	}
	cutoff := now - d.ttl
	fresh := pydict.New[string, float64]()
	for k, v := range d.seen.All() {
		if v > cutoff {
			fresh.Set(k, v)
		}
	}
	d.seen = fresh
	if fresh.Len() > d.maxSize {
		keys := fresh.Keys()
		sort.SliceStable(keys, func(i, j int) bool {
			a, _ := fresh.Get(keys[i])
			b, _ := fresh.Get(keys[j])
			return a < b
		})
		newest := pydict.New[string, float64]()
		for _, k := range keys[max(len(keys)-d.maxSize, 0):] {
			v, _ := fresh.Get(k)
			newest.Set(k, v)
		}
		d.seen = newest
	}
	return false
}

// Whether the id is live in the cache without inserting it, an expired id being dropped
func (d *MessageDeduplicator) Contains(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	seenAt, ok := d.seen.Get(msgID)
	if !ok {
		return false
	}
	if dedupNow()-seenAt < d.ttl {
		return true
	}
	d.seen.Delete(msgID)
	return false
}

// Releases a claimed id after a cancelled or failed handoff
func (d *MessageDeduplicator) Discard(msgID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen.Delete(msgID)
}

// Forgets every id
func (d *MessageDeduplicator) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen.Clear()
}

// Adopts the other cache's still live ids at their original seen times
func (d *MessageDeduplicator) Absorb(other *MessageDeduplicator) {
	if other == d {
		return
	}
	cutoff := dedupNow() - d.ttl
	other.mu.Lock()
	type entry struct {
		k string
		v float64
	}
	var live []entry
	for k, v := range other.seen.All() {
		if v > cutoff {
			live = append(live, entry{k, v})
		}
	}
	other.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range live {
		if !d.seen.Has(e.k) {
			d.seen.Set(e.k, e.v)
		}
	}
}

var dedupType = reflect.TypeOf((*MessageDeduplicator)(nil))

// Deduplicator fields of an adapter by attribute name, embedded structs flattened like an instance dict
func dedupFields(adapter any) map[string]*MessageDeduplicator {
	out := map[string]*MessageDeduplicator{}
	v := reflect.ValueOf(adapter)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return out
		}
		v = v.Elem()
	}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		for i := 0; i < v.NumField(); i++ {
			f, sf := v.Field(i), v.Type().Field(i)
			if f.Type() == dedupType {
				if _, seen := out[sf.Name]; !seen && !f.IsNil() {
					out[sf.Name] = (*MessageDeduplicator)(f.UnsafePointer())
				}
				continue
			}
			if !sf.Anonymous {
				continue
			}
			for f.Kind() == reflect.Pointer && !f.IsNil() {
				f = f.Elem()
			}
			if f.Kind() == reflect.Struct {
				walk(f)
			}
		}
	}
	if v.Kind() == reflect.Struct {
		walk(v)
	}
	return out
}

// Seeds a rebuilt adapter's dedup caches from the instance it replaces so replayed ids stay suppressed
func CarryInboundDedup(predecessor, adapter any) {
	if predecessor == nil {
		return
	}
	current := dedupFields(adapter)
	for name, previous := range dedupFields(predecessor) {
		if cur := current[name]; cur != nil && cur != previous {
			cur.Absorb(previous)
		}
	}
}

// Adapter that takes over inbound another instance is holding
type HeldInboundAdopter interface {
	AdoptHeldInbound(ctx context.Context, source any)
}

// Moves the inbound the source holds to the target when the target can adopt it
func HandOverHeldInbound(ctx context.Context, source, target any) {
	adopter, ok := target.(HeldInboundAdopter)
	if source != nil && ok {
		adopter.AdoptHeldInbound(ctx, source)
	}
}

// Worker handoff for the off loop persist paths, replaced by tests at this seam
var helpersToThread = func(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancels the task and waits for it to unwind, a self cancel only requesting cancellation
func CancelTask(ctx context.Context, task *AdapterTask) {
	if task == nil || task.Done() {
		return
	}
	task.cancel()
	if task == currentAdapterTask(ctx) {
		return
	}
	select {
	case <-task.done:
	case <-ctx.Done():
	}
}

// Inserts into an insertion ordered map with a hard bound, evicting the oldest keys and refreshing a re put key
func BoundedPut[V any](store *pydict.Dict[string, V], key string, value V, capacity int) {
	store.Delete(key)
	store.Set(key, value)
	for store.Len() > capacity {
		if store.Len() == 0 {
			panic("StopIteration")
		}
		store.Delete(store.Keys()[0])
	}
}

// Typing metadata of a turn for the platform adapters, the adapter hook taking the turn anchor
func TypingMetadataForTurn(adapter any, chatID string, anchor *string, metadata map[string]any) map[string]any {
	return typingMetadataForTurn(adapter, chatID, anchor, metadata)
}

type stripRule struct {
	re   *regexp2.Regexp
	repl string
}

var (
	stripMarkdownRules = []stripRule{
		{mustPyre(`(?s)\*\*(.+?)\*\*`), "$1"},
		{mustPyre(`(?s)\*(.+?)\*`), "$1"},
		{mustPyre(`(?s)\b__(?![\s_])(.+?)(?<![\s_])__\b`), "$1"},
		{mustPyre(`(?s)\b_(?![\s_])(.+?)(?<![\s_])_\b`), "$1"},
		{mustPyre("```[a-zA-Z0-9_+-]*\\n?"), ""},
		{mustPyre("`(.+?)`"), "$1"},
		{mustPyre(`(?m)^#{1,6}\s+`), ""},
	}
	mdLinkRE          = mustPyre(`\[([^\]]+)\]\(([^\)]+)\)`)
	httpTargetRE      = mustPyre(`(?i)https?://`)
	newlineSqueezeRE  = mustPyre(`\n{3,}`)
	TableSeparatorRE  = mustPyre(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)
	sentenceEndNLRE   = mustPyre(`[。！？.!?]\n`)
	firstGroupReplace = func(m regexp2.Match) string { return m.GroupByNumber(1).String() }
)

// Python re.sub with a group one or empty replacement
func pyreSub(re *regexp2.Regexp, text, repl string) string {
	out, err := re.ReplaceFunc(text, func(m regexp2.Match) string {
		if repl == "$1" {
			return firstGroupReplace(m)
		}
		return repl
	}, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Label then the bare http target on its own line, the label alone for other targets
func keepLinkTarget(m regexp2.Match) string {
	label, target := pystr.Strip(m.GroupByNumber(1).String()), pystr.Strip(m.GroupByNumber(2).String())
	if hit, _ := httpTargetRE.FindStringMatch(target); hit == nil || hit.RuneIndex != 0 {
		return m.GroupByNumber(1).String()
	}
	if label == target {
		return target
	}
	return label + "\n" + target
}

// Markdown stripped for plain text platforms, link targets kept as bare urls when asked
func StripMarkdown(text string, keepLinkTargets bool) string {
	for _, rule := range stripMarkdownRules {
		text = pyreSub(rule.re, text, rule.repl)
	}
	repl := firstGroupReplace
	if keepLinkTargets {
		repl = keepLinkTarget
	}
	text, err := mdLinkRE.ReplaceFunc(text, repl, -1, -1)
	if err != nil {
		panic(err)
	}
	return pystr.Strip(pyreSub(newlineSqueezeRE, text, "\n\n"))
}

// Default cap on tracked threads of a platform tracker
const DefaultMaxTrackedThreads = 500

// Persisted set of threads the bot took part in, one json file per platform
type ThreadParticipationTracker struct {
	platform   string
	maxTracked int
	mu         sync.Mutex
	ioMu       sync.Mutex
	threads    *pydict.Dict[string, struct{}]
}

// State file of a platform's tracker under the active home, replaced by tests at this seam
var threadTrackerStatePath = func(ctx context.Context, platform string) string {
	return pypath.PathJoin(home.Dir(ctx), platform+"_threads.json")
}

var helpersAtomicJSONWrite = func(ctx context.Context, path string, data any) error {
	return config.AtomicJSONWriteWith(ctx, path, data)
}

// Tracker seeded in file order from the platform state file, keeping at most maxTracked ids on persist
func NewThreadParticipationTracker(ctx context.Context, platform string, maxTracked int) *ThreadParticipationTracker {
	t := &ThreadParticipationTracker{platform: platform, maxTracked: maxTracked, threads: pydict.New[string, struct{}]()}
	for _, id := range t.load(ctx) {
		t.threads.Set(id, struct{}{})
	}
	return t
}

// Thread ids of the state file, empty when it is missing, unreadable or not a list
func (t *ThreadParticipationTracker) load(ctx context.Context) []string {
	raw, err := os.ReadFile(threadTrackerStatePath(ctx, t.platform))
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return nil
	}
	data, err := pyjson.Loads(text)
	list, ok := data.([]any)
	if err != nil || !ok {
		return nil
	}
	out := make([]string, len(list))
	for i, id := range list {
		out[i] = pyrepr.Str(id)
	}
	return out
}

// Writes the newest ids under the io lock, the memory lock held only for the snapshot
func (t *ThreadParticipationTracker) save(ctx context.Context) error {
	t.ioMu.Lock()
	defer t.ioMu.Unlock()
	t.mu.Lock()
	list := t.threads.Keys()
	if len(list) > t.maxTracked {
		list = list[pyNegativeSliceStart(len(list), t.maxTracked):]
		t.threads = pydict.New[string, struct{}]()
		for _, id := range list {
			t.threads.Set(id, struct{}{})
		}
	}
	t.mu.Unlock()
	payload := make([]any, len(list))
	for i, id := range list {
		payload[i] = id
	}
	return helpersAtomicJSONWrite(ctx, threadTrackerStatePath(ctx, t.platform), payload)
}

// Start index Python takes for seq[-n:] over a sequence of the given size, any sign of n
func pyNegativeSliceStart(size, n int) int {
	switch {
	case n > 0:
		return max(size-n, 0)
	case n < 0:
		return min(-n, size)
	}
	return 0
}

// Records the id in memory, true when a persist is still owed
func (t *ThreadParticipationTracker) remember(threadID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.threads.Has(threadID) {
		return false
	}
	t.threads.Set(threadID, struct{}{})
	return true
}

// Marks the thread as participated and persists inline
func (t *ThreadParticipationTracker) Mark(ctx context.Context, threadID string) error {
	if t.remember(threadID) {
		return t.save(ctx)
	}
	return nil
}

// Marks the thread in memory at once and persists on a worker
func (t *ThreadParticipationTracker) MarkAsync(ctx context.Context, threadID string) error {
	if t.remember(threadID) {
		return helpersToThread(ctx, func() error { return t.save(ctx) })
	}
	return nil
}

// Whether the bot took part in the thread
func (t *ThreadParticipationTracker) Contains(threadID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.threads.Has(threadID)
}

// Forgets every thread in memory
func (t *ThreadParticipationTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.threads.Clear()
}

// Sends the chunks in order and stops at the first failure, marking a partial overflow after delivered chunks
func SendChunks(ctx context.Context, chunks []string, sendOne func(ctx context.Context, chunk string) (*SendResult, error)) (*SendResult, error) {
	result := &SendResult{Success: false, Error: "nothing to send"}
	for delivered, chunk := range chunks {
		var err error
		if result, err = sendOne(ctx, chunk); err != nil {
			return nil, err
		}
		if !result.Success {
			if delivered > 0 {
				raw := map[string]any{}
				if prior, ok := result.RawResponse.(map[string]any); ok {
					for k, v := range prior {
						raw[k] = v
					}
				}
				raw["partial_overflow"], raw["delivered_chunks"], raw["total_chunks"] = true, delivered, len(chunks)
				result.RawResponse = raw
			}
			break
		}
	}
	return result, nil
}

// Phone number for logs keeping the country code and last four digits
func RedactPhone(phone string) string {
	if phone == "" {
		return "<none>"
	}
	rs := []rune(phone)
	if len(rs) <= 8 {
		if len(rs) > 4 {
			return string(rs[:2]) + "****" + string(rs[len(rs)-2:])
		}
		return "****"
	}
	return string(rs[:4]) + "****" + string(rs[len(rs)-4:])
}

// Whether the line could be a table data row
func IsTableRow(line string) bool { return strings.Contains(pystr.Strip(line), "|") }

// Stripped cells of a markdown table row
func SplitMarkdownTableRow(line string) []string { return splitTableRow(line) }

// Cells between the optional outer pipes of a table row, each stripped
func splitTableRow(row string) []string {
	s := pystr.Strip(row)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	cells := strings.Split(s, "|")
	for i, c := range cells {
		cells[i] = pystr.Strip(c)
	}
	return cells
}

func pyreMatch(re *regexp2.Regexp, s string) bool {
	m, err := re.FindStringMatch(s)
	return err == nil && m != nil && m.RuneIndex == 0
}

// Table rendered as bold row headings with one bullet per column
func renderTableBlock(block []string) string {
	var headers []string
	if len(block) >= 3 {
		headers = SplitMarkdownTableRow(block[0])
	}
	if len(headers) < 2 {
		return strings.Join(block, "\n")
	}
	hasRowLabel := len(SplitMarkdownTableRow(block[2])) == len(headers)+1
	var groups []string
	for index, row := range block[2:] {
		cells := SplitMarkdownTableRow(row)
		label := fmt.Sprintf("Row %d", index+1)
		heading, data := label, cells
		if hasRowLabel {
			if len(cells) > 0 && cells[0] != "" {
				heading = cells[0]
			}
			data = cells[1:]
		} else {
			for _, c := range cells {
				if c != "" {
					heading = c
					break
				}
			}
		}
		padded := append(append([]string{}, data...), make([]string, len(headers))...)[:len(headers)]
		lines := []string{"**" + heading + "**"}
		for i, h := range headers {
			if hasRowLabel || padded[i] != heading {
				lines = append(lines, "• "+h+": "+padded[i])
			}
		}
		groups = append(groups, strings.Join(lines, "\n"))
	}
	return strings.Join(groups, "\n\n")
}

// Pipe tables rewritten as heading and bullet groups, fenced code left alone
func ConvertTableToBullets(text string) string {
	if !strings.Contains(text, "|") || !strings.Contains(text, "-") {
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	inFence := false
	for i := 0; i < len(lines); {
		line := lines[i]
		isFenceLine := strings.HasPrefix(pystr.Lstrip(line), "```")
		inFence = inFence != isFenceLine
		if !(inFence || isFenceLine) && strings.Contains(line, "|") && i+1 < len(lines) && pyreMatch(TableSeparatorRE, lines[i+1]) {
			j := i + 2
			for j < len(lines) && IsTableRow(lines[j]) {
				j++
			}
			out = append(out, renderTableBlock(lines[i:j]))
			i = j
			continue
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

// How a mention pattern list is read, config style naming the platform and wakeword style giving defaults
type MentionPatternOptions struct {
	LogPrefix     string
	PlatformLabel *string
	DisplayLabel  string
	Defaults      []string
	Logger        *slog.Logger
}

// Case insensitive patterns from a config or env value, invalid ones logged and skipped
func CompileMentionPatterns(raw any, opts MentionPatternOptions) []*regexp2.Regexp {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	compile := func(patterns []string, warn func(pattern string, err error)) []*regexp2.Regexp {
		var compiled []*regexp2.Regexp
		for _, p := range patterns {
			re, err := pyre.CompileIgnoreCase(p)
			if err != nil {
				warn(p, err)
				continue
			}
			compiled = append(compiled, re)
		}
		return compiled
	}
	if opts.PlatformLabel != nil {
		display := opts.DisplayLabel
		if display == "" {
			display = *opts.PlatformLabel
		}
		if raw == nil {
			return nil
		}
		patterns := raw
		if s, ok := raw.(string); ok {
			patterns = []any{s}
		}
		list, ok := patterns.([]any)
		if !ok {
			log.Warn(fmt.Sprintf("[%s] %s mention_patterns must be a list or string; got %s", opts.LogPrefix, *opts.PlatformLabel, pyrepr.TypeName(patterns)))
			return nil
		}
		var texts []string
		for _, p := range list {
			if s, ok := p.(string); ok && pystr.Strip(s) != "" {
				texts = append(texts, s)
			}
		}
		compiled := compile(texts, func(p string, err error) {
			log.Warn(fmt.Sprintf("[%s] Invalid %s mention pattern %s: %s", opts.LogPrefix, display, pyrepr.Repr(p), err))
		})
		if len(compiled) > 0 {
			log.Info(fmt.Sprintf("[%s] Loaded %d %s mention pattern(s)", opts.LogPrefix, len(compiled), display))
		}
		return compiled
	}
	var patterns []any
	switch r := raw.(type) {
	case nil:
		for _, d := range opts.Defaults {
			patterns = append(patterns, d)
		}
	case string:
		text := pystr.Strip(r)
		var loaded any
		if text == "" {
			loaded = []any{}
		} else if v, err := pyjson.Loads(text); err == nil {
			loaded = v
		}
		if list, ok := loaded.([]any); ok {
			patterns = list
		} else {
			for _, line := range pystr.Splitlines(text, false) {
				for _, part := range strings.Split(line, ",") {
					patterns = append(patterns, pystr.Strip(part))
				}
			}
		}
	case []any:
		patterns = r
	default:
		patterns = []any{r}
	}
	var texts []string
	for _, p := range patterns {
		if t := pystr.Strip(pyrepr.Str(p)); t != "" {
			texts = append(texts, t)
		}
	}
	return compile(texts, func(p string, err error) {
		log.Warn(fmt.Sprintf("[%s] Invalid mention pattern %s: %s", opts.LogPrefix, pyrepr.Repr(p), err))
	})
}

// True when the text ends inside an unclosed code fence
func TextHasUnclosedFence(text string) bool {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "```") {
			n++
		}
	}
	return n%2 == 1
}

// True when the last non empty line starts and ends with a pipe
func TextEndsWithTableRow(text string) bool {
	lines := strings.Split(pystr.Rstrip(text), "\n")
	return isPipeRow(lines[len(lines)-1])
}

// True when an atomic block is a code block
func IsFenceAtom(text string) bool { return strings.HasPrefix(pystr.Lstrip(text), "```") }

func isPipeRow(line string) bool {
	s := pystr.Strip(line)
	return strings.HasPrefix(s, "|") && strings.HasSuffix(s, "|")
}

// True when an atomic block is a table
func IsTableAtom(text string) bool { return isPipeRow(strings.Split(text, "\n")[0]) }

// Code points of the longest prefix within budget units, plain code point length needing no search
func cpBudget(text []rune, budget int, lenFn func(string) int) int {
	if lenFn == nil {
		return budget
	}
	return customUnitToCP(text, budget, lenFn)
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func measureOrRunes(lenFn func(string) int) func(string) int {
	if lenFn == nil {
		return runeLen
	}
	return lenFn
}

// Last code point index of sub inside rs, minus one when absent
func runeRFind(rs []rune, sub string) int {
	i := strings.LastIndex(string(rs), sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(string(rs)[:i])
}

// Head within maxChars cut at a blank line, a sentence end, a newline or the window edge, plus the tail
func SplitAtParagraphBoundary(text string, maxChars int, lenFn func(string) int) (string, string) {
	if measureOrRunes(lenFn)(text) <= maxChars {
		return text, ""
	}
	rs := []rune(text)
	window := rs[:min(cpBudget(rs, maxChars, lenFn), len(rs))]
	pos := runeRFind(window, "\n\n")
	cut := 0
	if pos > 0 {
		cut = pos + 2
	} else {
		ws := string(window)
		var last int
		m, _ := sentenceEndNLRE.FindStringMatch(ws)
		for m != nil {
			last = m.RuneIndex + m.RuneLength
			m, _ = sentenceEndNLRE.FindNextMatch(m)
		}
		cut = last
	}
	if cut == 0 {
		if p := runeRFind(window, "\n"); p > 0 {
			cut = p + 1
		} else {
			cut = len(window)
		}
	}
	return string(rs[:cut]), string(rs[cut:])
}

// Fenced code blocks, tables and paragraphs as indivisible atoms, blank lines belonging to none
func SplitMarkdownAtoms(text string) []string {
	var atoms, current []string
	inFence := false
	flush := func() {
		if len(current) > 0 {
			atom := strings.Join(current, "\n")
			if pystr.Strip(atom) != "" {
				atoms = append(atoms, atom)
			}
			current = current[:0]
		}
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case inFence:
			current = append(current, line)
			if strings.HasPrefix(line, "```") {
				inFence = false
				flush()
			}
		case strings.HasPrefix(line, "```"):
			flush()
			inFence = true
			current = append(current, line)
		case pystr.Strip(line) == "":
			flush()
		default:
			if len(current) > 0 && isPipeRow(current[len(current)-1]) != isPipeRow(line) {
				flush()
			}
			current = append(current, line)
		}
	}
	flush()
	return atoms
}

// One newline at a fence or a continued table boundary, a blank line otherwise
func InferBlockSeparator(prevChunk, nextChunk string) string {
	prevTrimmed, nextTrimmed := pystr.Rstrip(prevChunk), pystr.Lstrip(nextChunk)
	if strings.HasSuffix(prevTrimmed, "```") || strings.HasPrefix(nextTrimmed, "```") {
		return "\n"
	}
	if TextEndsWithTableRow(prevChunk) && nextTrimmed != "" && isPipeRow(strings.Split(nextTrimmed, "\n")[0]) {
		return "\n"
	}
	return "\n\n"
}

// Chunks cut inside a fence rejoined with their successors
func MergeStreamingFences(chunks []string) []string {
	var result []string
	for i := 0; i < len(chunks); i++ {
		current := chunks[i]
		for TextHasUnclosedFence(current) && i+1 < len(chunks) {
			current = current + InferBlockSeparator(current, chunks[i+1]) + chunks[i+1]
			i++
		}
		result = append(result, current)
	}
	return result
}

// Every chunk fence balanced alone, an orphaned fence closed and reopened with its language
func BalanceFencesAcrossChunks(chunks []string) []string {
	if len(chunks) <= 1 {
		return chunks
	}
	out := make([]string, 0, len(chunks))
	var carry *string
	for _, chunk := range chunks {
		body := chunk
		lang := ""
		if carry != nil {
			body = "```" + *carry + "\n" + chunk
			lang = *carry
		}
		inCode, after := FenceStateAfter(chunk, carry != nil, lang)
		carry = nil
		if inCode {
			carry = &after
			body += "\n```"
		}
		out = append(out, body)
	}
	return out
}

// Blocks packed greedily into chunks within maxLength, an oversized block going through overflow when given
func GreedyPackBlocks(blocks []string, maxLength int, lenFn func(string) int, sep string, overflow func(block string) []string) []string {
	measureFn := measureOrRunes(lenFn)
	var packed []string
	current := ""
	for _, block := range blocks {
		candidate := block
		if current != "" {
			candidate = current + sep + block
		}
		if measureFn(candidate) <= maxLength {
			current = candidate
			continue
		}
		if current != "" {
			packed = append(packed, current)
			current = ""
		}
		switch {
		case measureFn(block) <= maxLength:
			current = block
		case overflow != nil:
			packed = append(packed, overflow(block)...)
		default:
			packed = append(packed, block)
		}
	}
	if current != "" {
		packed = append(packed, current)
	}
	return packed
}

// Markdown split into chunks within limit respecting fences, by paragraphs or by newlines
func SplitTextFenceAware(text string, limit int, lenFn func(string) int, preferParagraphs, balanceFences bool) []string {
	if text == "" {
		return nil
	}
	var chunks []string
	if preferParagraphs {
		chunks = chunkMarkdownParagraphs(text, limit, lenFn)
	} else {
		chunks = chunkNewlinePreferred(text, limit, lenFn)
	}
	if balanceFences {
		return BalanceFencesAcrossChunks(chunks)
	}
	return chunks
}

// Atoms merged greedily, oversized divisible chunks split at paragraphs and small neighbours re merged
func chunkMarkdownParagraphs(text string, maxChars int, lenFn func(string) int) []string {
	measureFn := measureOrRunes(lenFn)
	if measureFn(text) <= maxChars {
		return []string{text}
	}
	var chunks, parts []string
	indivisible := map[int]bool{}
	currentLen := 0
	for _, atom := range SplitMarkdownAtoms(text) {
		atomLen := measureFn(atom)
		sepLen := 0
		if len(parts) > 0 {
			sepLen = 2
		}
		if currentLen+sepLen+atomLen > maxChars && len(parts) > 0 {
			chunks = append(chunks, strings.Join(parts, "\n\n"))
			parts, currentLen, sepLen = nil, 0, 0
		}
		if len(parts) == 0 && atomLen > maxChars && (IsFenceAtom(atom) || IsTableAtom(atom)) {
			indivisible[len(chunks)] = true
			chunks = append(chunks, atom)
			continue
		}
		parts = append(parts, atom)
		currentLen += sepLen + atomLen
	}
	if len(parts) > 0 {
		chunks = append(chunks, strings.Join(parts, "\n\n"))
	}
	var result []string
	for idx, chunk := range chunks {
		if measureFn(chunk) <= maxChars || indivisible[idx] || TextHasUnclosedFence(chunk) {
			result = append(result, chunk)
			continue
		}
		remaining := chunk
		for measureFn(remaining) > maxChars {
			var head string
			head, remaining = SplitAtParagraphBoundary(remaining, maxChars, lenFn)
			if head == "" {
				rs := []rune(remaining)
				cut := min(maxChars, len(rs))
				head, remaining = string(rs[:cut]), string(rs[cut:])
			}
			if head != "" {
				result = append(result, head)
			}
		}
		if remaining != "" {
			result = append(result, remaining)
		}
	}
	var merged []string
	if len(result) > 0 {
		merged = append(merged, result[0])
	}
	for _, chunk := range result[min(1, len(result)):] {
		if combined := merged[len(merged)-1] + "\n\n" + chunk; measureFn(combined) <= maxChars {
			merged[len(merged)-1] = combined
		} else {
			merged = append(merged, chunk)
		}
	}
	out := merged[:0]
	for _, c := range merged {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Newline preferred hard splitting with headroom kept for the fence markers balancing may add
func chunkNewlinePreferred(text string, limit int, lenFn func(string) int) []string {
	measureFn := measureOrRunes(lenFn)
	if measureFn(text) <= limit {
		return []string{text}
	}
	splitLimit := limit
	if strings.Contains(text, "```") {
		splitLimit = max(limit-16, limit/2, 1)
	}
	var chunks []string
	remaining := text
	for measureFn(remaining) > splitLimit {
		rs := []rune(remaining)
		budget := cpBudget(rs, splitLimit, lenFn)
		splitAt := runeRFind(rs[:min(max(budget, 0), len(rs))], "\n")
		if splitAt < budget/2 {
			splitAt = budget
		}
		splitAt = min(max(splitAt, 0), len(rs))
		chunks = append(chunks, string(rs[:splitAt]))
		remaining = strings.TrimLeft(string(rs[splitAt:]), "\n")
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

const (
	DiscordChannelObfuscatedFlag = 1 << 17
	DiscordObfuscatedChannelName = "___hidden___"
)

// Channel whose flags value can be read, ok false when the channel carries no flags
type DiscordChannelFlagsReader interface {
	DiscordChannelFlags() (value any, ok bool)
}

// Channel whose name can be read, ok false when the channel carries no name
type DiscordChannelNameReader interface {
	DiscordChannelName() (name any, ok bool)
}

// True for a channel Discord hid from the bot, by its flag first and the sentinel name second
func IsDiscordChannelObfuscated(channel any) bool {
	if r, ok := channel.(DiscordChannelFlagsReader); ok {
		if value, has := r.DiscordChannelFlags(); has {
			if n, isInt := pyint.Instance(value); isInt && new(big.Int).And(n, big.NewInt(DiscordChannelObfuscatedFlag)).Sign() != 0 {
				return true
			}
		}
	}
	if r, ok := channel.(DiscordChannelNameReader); ok {
		if name, has := r.DiscordChannelName(); has {
			s, isStr := name.(string)
			return isStr && s == DiscordObfuscatedChannelName
		}
	}
	return false
}

// How a platform HTTP client is built, nil fields taking the platform defaults
type PlatformHTTPClientOptions struct {
	Timeout         *time.Duration
	FollowRedirects *bool
	Limits          *HTTPXLimits
	DefaultLimits   bool
	ResponseHooks   []func(resp *http.Response) error
}

// Transport running the redirect guard then the caller hooks on every response hop
type platformHookTransport struct {
	base  http.RoundTripper
	hooks []func(resp *http.Response) error
}

func (t *platformHookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	for _, hook := range t.hooks {
		if herr := hook(resp); herr != nil {
			resp.Body.Close()
			return nil, herr
		}
	}
	return resp, nil
}

// Redirect target of a response, empty when it is not a redirect
func redirectTargetFromResponse(resp *http.Response) string {
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
	default:
		return ""
	}
	loc := resp.Header.Get("Location")
	if loc == "" || resp.Request == nil {
		return ""
	}
	u, err := resp.Request.URL.Parse(loc)
	if err != nil {
		return ""
	}
	return u.String()
}

// SSRF guarded client for a platform adapter with redirect hop checks and the shared keepalive limits
func PlatformHTTPClient(ctx context.Context, opts PlatformHTTPClientOptions) *http.Client {
	timeout := 30 * time.Second
	if opts.Timeout != nil {
		timeout = *opts.Timeout
	}
	follow := true
	if opts.FollowRedirects != nil {
		follow = *opts.FollowRedirects
	}
	env := urlSafetyEnv(ctx)
	client := tools.NewSSRFSafeClient(env, timeout, follow)
	limits := PlatformHTTPXLimits()
	switch {
	case opts.DefaultLimits:
		limits = DefaultHTTPXLimits()
	case opts.Limits != nil:
		limits = *opts.Limits
	}
	if tr, ok := client.Transport.(*http.Transport); ok {
		applyHTTPXLimits(tr, limits)
	}
	guard := func(resp *http.Response) error { return SSRFRedirectGuard(env, redirectTargetFromResponse(resp)) }
	client.Transport = &platformHookTransport{base: client.Transport, hooks: append([]func(*http.Response) error{guard}, opts.ResponseHooks...)}
	return client
}
