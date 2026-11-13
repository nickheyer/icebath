package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Block header titles the prompt renderer and compression share to spot a leftover block
var BlockHeaders = map[string]string{
	"memory": "MEMORY (your personal notes)",
	"user":   "USER PROFILE (who the user is)",
}

// Separator between entries inside MEMORY.md and USER.md
const EntryDelimiter = "\n§\n"

// Default character budgets of the two stores
const (
	DefaultMemoryCharLimit = 2200
	DefaultUserCharLimit   = 1375
)

// Hint naming how to drop text inside an entry without losing its siblings
const PatchDropHint = "To drop text inside an entry, patch a longer span that contains it; 'remove' deletes the whole entry."

const backupFailedSuffix = " (BACKUP FAILED — file unchanged on disk)"

// Failed consolidation attempts allowed per turn before a terminal skip result
const maxConsolidationFailuresPerTurn = 3

// Result is one store response plus the closed failure class the shared metric reads
type Result struct {
	Data         map[string]any
	FailureClass string
}

// Reports whether the response carries a truthy success flag
func (r Result) Success() bool {
	return pyTruthy(r.Data["success"])
}

// Error response carrying a failure class and extra fields
func failure(message, class string, extra map[string]any) Result {
	data := map[string]any{"success": false, "error": message}
	for k, v := range extra {
		data[k] = v
	}
	return Result{Data: data, FailureClass: class}
}

// NewFailure builds an error response for closures passed to Mutate
func NewFailure(message, class string) Result {
	return failure(message, class, nil)
}

// Memory tool target owning a store file, for hints showing the call to make
func storeTarget(filename string) string {
	if filename == "USER.md" {
		return "user"
	}
	return "memory"
}

// Refusal for a file holding an entry over the whole limit, naming the snapshot
func driftError(path, bakPath string) Result {
	name := filepath.Base(path)
	return failure(fmt.Sprintf("Refusing to write %s: it holds an entry larger than the whole store's char limit, "+
		"i.e. free-form text written without § delimiters (the patch tool, a shell append, a manual "+
		"edit). Rewriting it would lose that text. A snapshot was saved to %s. Resolve the drift first — "+
		"either rewrite the file as a clean §-delimited list of entries, or move the extra "+
		"content out — then retry. This guard exists to prevent silent data loss (issue #26045).", name, bakPath),
		"drift", map[string]any{
			"drift_backup": bakPath,
			"remediation": "Open the .bak file, integrate the missing entries into the memory tool with one add op per entry via " +
				fmt.Sprintf("memory(target='%s', operations=[{action: 'add', content: ...}]), then remove or ", storeTarget(name)) +
				"rewrite the original file to a clean state.",
		})
}

// Refusal for an existing file that could not be read, saving would wipe it
func readFailedError(path string) Result {
	return failure(fmt.Sprintf("Refusing to write %s: the file exists on disk but could not be read right now "+
		"(temporarily locked by another program, a permission change, invalid/corrupt text encoding, "+
		"or a filesystem error). Treating an unreadable file as empty and saving would wipe existing "+
		"memory, so the write is refused. Nothing was changed — retry in a moment.", filepath.Base(path)), "read_failed", nil)
}

// Quote and dash code points a model retypes differently from the stored entry
var matchFold = map[rune]rune{
	'"': '\'', '`': '\'', 0x2018: '\'', 0x2019: '\'', 0x201a: '\'', 0x201b: '\'', 0x201c: '\'', 0x201d: '\'',
	0x201e: '\'', 0x201f: '\'',
	0x2010: '-', 0x2011: '-', 0x2012: '-', 0x2013: '-', 0x2014: '-', 0x2015: '-', 0x2212: '-',
}

// Quote, dash and whitespace folded form for tolerant old_text matching
func normalizeForMatch(text string) string {
	text = strings.ReplaceAll(text, `\n`, " ")
	folded := strings.Map(func(r rune) rune {
		if f, ok := matchFold[r]; ok {
			return f
		}
		return r
	}, text)
	return strings.Join(pyFields(folded), " ")
}

// Indices of entries containing old_text, byte exact first and typography folded otherwise
func substringMatches(entries []string, oldText string) []int {
	var exact []int
	for i, e := range entries {
		if strings.Contains(e, oldText) {
			exact = append(exact, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	needle := normalizeForMatch(oldText)
	if !strings.ContainsFunc(needle, pyIsAlnum) {
		return nil
	}
	var out []int
	for i, e := range entries {
		if strings.Contains(normalizeForMatch(e), needle) {
			out = append(out, i)
		}
	}
	return out
}

// Index matching old_text with a whole entry match winning, -1 when none, ambiguous for distinct matches
func findUniqueMatch(entries []string, oldText string) (int, bool) {
	var matches []int
	for i, e := range entries {
		if e == oldText {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		matches = substringMatches(entries, oldText)
	}
	distinct := map[string]bool{}
	for _, i := range matches {
		distinct[entries[i]] = true
	}
	if len(distinct) > 1 {
		return -1, true
	}
	if len(matches) == 0 {
		return -1, false
	}
	return matches[0], false
}

// Lowercased Unicode word set like re findall of word runs
func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(pyLower(text), func(r rune) bool { return !pyIsAlnum(r) && r != '_' }) {
		out[w] = true
	}
	return out
}

// Zero match error naming the closest entries by shared words and a hint for multi entry spans
func noMatchError(entries []string, oldText, verb string, extra map[string]any) Result {
	words := wordSet(oldText)
	type scored struct {
		score float64
		idx   int
	}
	var ranked []scored
	if len(words) > 0 {
		for i, e := range entries {
			shared := 0
			for w := range wordSet(e) {
				if words[w] {
					shared++
				}
			}
			ranked = append(ranked, scored{float64(shared) / float64(len(words)), i})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score
		}
		return ranked[a].idx < ranked[b].idx
	})
	var closest []string
	for k := 0; k < len(ranked) && k < 3; k++ {
		if ranked[k].score >= 0.3 {
			closest = append(closest, entries[ranked[k].idx])
		}
	}
	hint := ""
	if strings.Contains(oldText, "§") {
		hint = " old_text must come from ONE entry ('§' separates entries); use one op per entry."
	}
	tail := "."
	fields := map[string]any{}
	if len(closest) > 0 {
		tail = " (closest_entries below)."
		fields["closest_entries"] = closest
	}
	for k, v := range extra {
		fields[k] = v
	}
	return failure(fmt.Sprintf("No entry matched '%s'.%s Retry the %s with old_text copied verbatim from the intended entry%s", oldText, hint, verb, tail), "no_match", fields)
}

// Budget phrase for a write that would exceed the limit
func overLimit(newTotal, limit int) string {
	return fmt.Sprintf("would put memory at %s/%s chars: free at least %s chars", thousands(newTotal), thousands(limit), thousands(newTotal-limit))
}

func repeatedSpanMessage(oldText string, count int) string {
	return fmt.Sprintf("'%s' appears %d times in the matched entry, and patch edits one span. "+
		"Include more surrounding text so old_text occurs once.", oldText, count)
}

// Occurrences of span in text with overlapping ones counted
func spanOccurrences(text, span string) int {
	n := 0
	for i := 0; i+len(span) <= len(text); i++ {
		if strings.HasPrefix(text[i:], span) {
			n++
		}
	}
	return n
}

// Patched entry, or an error and failure class for a repeated span, a no op, or a threat hit
func patchEntry(entry, oldText, newText string) (string, string, string, bool) {
	if count := spanOccurrences(entry, oldText); count > 1 {
		return "", repeatedSpanMessage(oldText, count), "ambiguous", false
	}
	patched := strings.Replace(entry, oldText, newText, 1)
	if patched == entry {
		return "", fmt.Sprintf("Patch is a no-op: '%s' -> '%s' leaves the entry unchanged.", oldText, newText), "invalid_args", false
	}
	if scanErr := FirstThreatMessage(patched, ScopeStrict); scanErr != "" {
		return "", scanErr, "scan_blocked", false
	}
	return patched, "", "none", true
}

// Index of the exact entry a staged write was reviewed against, -1 once it is gone
func pinnedIndex(entries []string, matched any) int {
	s, ok := matched.(string)
	if !ok {
		return -1
	}
	for i, e := range entries {
		if e == s {
			return i
		}
	}
	return -1
}

func staleEntryMessage(entry any) string {
	return fmt.Sprintf("Entry changed since it was staged, so this write was not applied: '%s' is no longer "+
		"in memory as reviewed. Recreate the change against the current entry or reject it; the "+
		"pending record has been preserved.", pyStr(entry))
}

// Store is the bounded file backed curated memory, MEMORY.md and USER.md, one per session
type Store struct {
	Dir                string
	MemoryCharLimit    int
	UserCharLimit      int
	MemoryEnabled      bool
	UserProfileEnabled bool
	Log                *slog.Logger

	mu                    sync.Mutex
	memoryEntries         []string
	userEntries           []string
	snapshot              map[string]string
	consolidationFailures int
}

// NewStore returns an unloaded store rooted at the memories directory
func NewStore(dir string, memoryCharLimit, userCharLimit int, memoryEnabled, userProfileEnabled bool) *Store {
	return &Store{
		Dir:                dir,
		MemoryCharLimit:    memoryCharLimit,
		UserCharLimit:      userCharLimit,
		MemoryEnabled:      memoryEnabled,
		UserProfileEnabled: userProfileEnabled,
		snapshot:           map[string]string{"memory": "", "user": ""},
	}
}

func (s *Store) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Whether built in writes to the target are enabled in config
func (s *Store) TargetEnabled(target string) bool {
	if target == "user" {
		return s.UserProfileEnabled
	}
	return s.MemoryEnabled
}

// Clears the per turn consolidation failure count at a turn boundary
func (s *Store) ResetConsolidationFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consolidationFailures = 0
}

// Counts a consolidation failure, past the cap returning a terminal result so the turn can answer
func (s *Store) consolidationFailure(response Result) Result {
	s.consolidationFailures++
	if s.consolidationFailures <= maxConsolidationFailuresPerTurn {
		return response
	}
	return Result{FailureClass: "retry_cap", Data: map[string]any{"success": false, "done": true, "error": fmt.Sprintf(
		"Memory consolidation failed %d times this turn. Stop retrying "+
			"memory calls — leave memory unchanged for now and continue with your reply to the user. "+
			"The fact can be saved in a later turn.", s.consolidationFailures)}}
}

// Loads both files deduplicated and freezes the prompt snapshot with threat hits replaced by a placeholder
func (s *Store) LoadFromDisk(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot == nil {
		s.snapshot = map[string]string{"memory": "", "user": ""}
	}
	for _, target := range []string{"memory", "user"} {
		path := s.PathFor(target)
		if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
			return err
		}
		entries := dedupe(ReadEntriesFile(path))
		s.setEntries(target, entries)
		if count, limit := s.charCount(target), s.charLimit(target); count > limit {
			s.log().Warn(fmt.Sprintf("%s exceeds its char limit on load: %d/%d chars. Entries stay loaded; "+
				"further additions are blocked until it is back under the limit.", filepath.Base(path), count, limit))
		}
		sanitized := make([]string, len(entries))
		for i, e := range entries {
			sanitized[i] = s.sanitizeForSnapshot(e, filepath.Base(path))
		}
		s.snapshot[target] = s.renderBlock(target, sanitized)
	}
	return nil
}

// Snapshot form of an entry, a BLOCKED placeholder when the strict scan finds a threat
func (s *Store) sanitizeForSnapshot(entry, filename string) string {
	if entry == "" || strings.HasPrefix(entry, "[BLOCKED:") {
		return entry
	}
	findings, _ := ScanForThreats(entry, ScopeStrict)
	if len(findings) == 0 {
		return entry
	}
	joined := strings.Join(findings, ", ")
	s.log().Warn(fmt.Sprintf("Memory entry from %s blocked at load time: %s", filename, joined))
	return fmt.Sprintf("[BLOCKED: %s entry contained threat pattern(s): %s. "+
		"Removed from system prompt; use memory(action=remove, target='%s') "+
		"to delete the original.]", filename, joined, storeTarget(filename))
}

// File path of the target store
func (s *Store) PathFor(target string) string {
	if target == "user" {
		return filepath.Join(s.Dir, "USER.md")
	}
	return filepath.Join(s.Dir, "MEMORY.md")
}

func (s *Store) entriesFor(target string) []string {
	if target == "user" {
		return s.userEntries
	}
	return s.memoryEntries
}

func (s *Store) setEntries(target string, entries []string) {
	if target == "user" {
		s.userEntries = entries
	} else {
		s.memoryEntries = entries
	}
}

func (s *Store) charCount(target string) int {
	return runeLen(strings.Join(s.entriesFor(target), EntryDelimiter))
}

func (s *Store) charLimit(target string) int {
	if target == "user" {
		return s.UserCharLimit
	}
	return s.MemoryCharLimit
}

func (s *Store) usage(target string) string {
	return thousands(s.charCount(target)) + "/" + thousands(s.charLimit(target))
}

func (s *Store) usagePct(target string, current int) string {
	limit := s.charLimit(target)
	pct := 0
	if limit > 0 {
		pct = min(100, int(math.Trunc(float64(current)/float64(limit)*100)))
	}
	return fmt.Sprintf("%d%% — %s/%s chars", pct, thousands(current), thousands(limit))
}

// Live entries of the target
func (s *Store) Entries(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entriesFor(target)...)
}

// Current and limit characters of the target formatted with separators
func (s *Store) Usage(target string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage(target)
}

// Character limit of the target
func (s *Store) CharLimit(target string) int {
	return s.charLimit(target)
}

// Over budget consolidation failure carrying the live entries so the model can consolidate
func (s *Store) failureWithEntries(target, message string) Result {
	return s.consolidationFailure(failure(message, "over_budget", map[string]any{
		"current_entries": append([]string(nil), s.entriesFor(target)...), "usage": s.usage(target)}))
}

// A lone add over budget returning the live entries and the exact overflow
func (s *Store) addOverflow(target, content string, limit int) Result {
	over := runeLen(strings.Join(append(append([]string(nil), s.entriesFor(target)...), content), EntryDelimiter)) - limit
	return s.failureWithEntries(target, fmt.Sprintf(
		"Memory at %s/%s chars; adding this entry (%d chars) "+
			"would exceed the limit by %s chars. Retry as ONE 'operations' batch that "+
			"removes or shortens (patch) stale entries from current_entries below to free at least "+
			"%s chars AND adds this entry — the limit is checked only on the batch result.",
		thousands(s.charCount(target)), thousands(limit), runeLen(content), thousands(over), thousands(over)))
}

// Batch abort without current entries, a zero match op carrying only its closest entries
func (s *Store) batchFailure(target, message, class string, closest []string) Result {
	extra := map[string]any{"usage": s.usage(target)}
	if len(closest) > 0 {
		extra["closest_entries"] = closest
	}
	return s.consolidationFailure(failure(message+" No operations were applied (batch is all-or-nothing).", class, extra))
}

// Mutation is a closure outcome, a passthrough result or new entries to persist with a message
type Mutation struct {
	Result  *Result
	Entries []string
	Message string
	Extra   map[string]any
}

// MutateFunc computes a mutation from the entries reread under the lock and the target limit
type MutateFunc func(entries []string, limit int) Mutation

func passthrough(r Result) Mutation { return Mutation{Result: &r} }

// Mutate locks, rereads, runs fn, applies the drift guard, persists and returns the success response
func (s *Store) Mutate(ctx context.Context, target string, fn MutateFunc, skipDrift bool) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutate(ctx, target, fn, skipDrift)
}

func (s *Store) mutate(ctx context.Context, target string, fn MutateFunc, skipDrift bool) (Result, error) {
	path := s.PathFor(target)
	unlock, err := fileLock(ctx, path)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	raw, readOK := readRawChecked(path)
	if !readOK {
		return readFailedError(path), nil
	}
	oversized := !skipDrift && s.hasOversizedEntry(target, raw)
	before := dedupe(ParseEntries(raw))
	s.setEntries(target, before)
	result := fn(append([]string(nil), before...), s.charLimit(target))
	if result.Result != nil {
		res := *result.Result
		if res.FailureClass == "" {
			res.FailureClass = "other"
		}
		if oversized && isFalse(res.Data["success"]) && res.FailureClass == "over_budget" {
			return driftError(path, s.driftBackup(target, raw)), nil
		}
		return res, nil
	}
	extra := map[string]any{}
	for k, v := range result.Extra {
		extra[k] = v
	}
	if oversized {
		beforeSet := map[string]bool{}
		for _, e := range before {
			beforeSet[e] = true
		}
		afterSet := map[string]bool{}
		for _, e := range result.Entries {
			afterSet[e] = true
			if !beforeSet[e] {
				return driftError(path, s.driftBackup(target, raw)), nil
			}
		}
		limit := s.charLimit(target)
		for _, e := range before {
			if !afterSet[e] && runeLen(e) > limit {
				bak := s.driftBackup(target, raw)
				if strings.HasSuffix(bak, backupFailedSuffix) {
					return driftError(path, bak), nil
				}
				extra["drift_backup"] = bak
				break
			}
		}
	}
	s.setEntries(target, result.Entries)
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return Result{}, err
	}
	if err := writeEntriesFile(ctx, path, result.Entries); err != nil {
		return Result{}, err
	}
	return s.successResponse(target, result.Message, extra), nil
}

// Appends a new entry, refusing empty, poisoned or over budget content
func (s *Store) Add(ctx context.Context, target, content string) (Result, error) {
	content = pyStrip(content)
	if content == "" {
		return failure("Content cannot be empty.", "missing_content", nil), nil
	}
	if scanErr := FirstThreatMessage(content, ScopeStrict); scanErr != "" {
		return failure(scanErr, "scan_blocked", nil), nil
	}
	return s.Mutate(ctx, target, func(entries []string, limit int) Mutation {
		for _, e := range entries {
			if e == content {
				return passthrough(s.successResponse(target, "Entry already exists (no duplicate added).", nil))
			}
		}
		if runeLen(strings.Join(append(entries, content), EntryDelimiter)) > limit {
			return passthrough(s.addOverflow(target, content, limit))
		}
		return Mutation{Entries: append(entries, content), Message: "Entry added."}
	}, true)
}

// Overwrites the whole entry old_text selects with new content
func (s *Store) Replace(ctx context.Context, target, oldText, newContent string, matchedEntry any) (Result, error) {
	newContent = pyStrip(newContent)
	if pyStrip(oldText) == "" {
		return failure("old_text cannot be empty.", "missing_old_text", nil), nil
	}
	if newContent == "" {
		return failure("new_content cannot be empty. Use 'remove' to delete entries.", "missing_content", nil), nil
	}
	if scanErr := FirstThreatMessage(newContent, ScopeStrict); scanErr != "" {
		return failure(scanErr, "scan_blocked", nil), nil
	}
	return s.edit(ctx, target, pyStrip(oldText), &newContent, matchedEntry)
}

// Replaces one span inside the matched entry and keeps the rest of the entry verbatim
func (s *Store) Patch(ctx context.Context, target, oldText, newText string, matchedEntry any) (Result, error) {
	if pyStrip(oldText) == "" {
		return failure("old_text cannot be empty.", "missing_old_text", nil), nil
	}
	if pyStrip(newText) == "" {
		return failure("new_text cannot be empty. "+PatchDropHint, "missing_content", nil), nil
	}
	return s.Mutate(ctx, target, func(entries []string, limit int) Mutation {
		idx, errRes := s.locate(entries, oldText, "patch", matchedEntry)
		if errRes != nil {
			return passthrough(*errRes)
		}
		before := entries[idx]
		after, message, class, ok := patchEntry(before, oldText, newText)
		if !ok {
			return passthrough(failure(message, class, nil))
		}
		patched := append(append(append([]string(nil), entries[:idx]...), after), entries[idx+1:]...)
		if newTotal := runeLen(strings.Join(patched, EntryDelimiter)); newTotal > limit {
			return passthrough(s.failureWithEntries(target, fmt.Sprintf(
				"Patch would put memory at %s/%s chars. Shorten the new "+
					"text, or 'remove' other stale or less important entries to make room "+
					"(see current_entries below), then retry — all in this turn.", thousands(newTotal), thousands(limit))))
		}
		return Mutation{Entries: patched, Message: "Entry patched.", Extra: map[string]any{
			"patched_entry_before": before, "patched_entry_after": after}}
	}, false)
}

// Removes the entry old_text selects
func (s *Store) Remove(ctx context.Context, target, oldText string, matchedEntry any) (Result, error) {
	if pyStrip(oldText) == "" {
		return failure("old_text cannot be empty.", "missing_old_text", nil), nil
	}
	return s.edit(ctx, target, pyStrip(oldText), nil, matchedEntry)
}

// Index old_text selects, or the error the edit returns, a pinned entry selecting only itself
func (s *Store) locate(entries []string, oldText, verb string, matchedEntry any) (int, *Result) {
	if matchedEntry != nil {
		if idx := pinnedIndex(entries, matchedEntry); idx >= 0 {
			return idx, nil
		}
		r := failure(staleEntryMessage(matchedEntry), "stale_entry", nil)
		return -1, &r
	}
	idx, ambiguous := findUniqueMatch(entries, oldText)
	if ambiguous {
		var previews []string
		for _, i := range substringMatches(entries, oldText) {
			p := runePrefix(entries[i], 80)
			if runeLen(entries[i]) > 80 {
				p += "..."
			}
			previews = append(previews, p)
		}
		r := failure(fmt.Sprintf("Multiple entries matched '%s'. Be more specific.", oldText), "ambiguous", map[string]any{"matches": previews})
		return -1, &r
	}
	if idx < 0 {
		r := s.consolidationFailure(noMatchError(entries, oldText, verb, map[string]any{"current_entries": append([]string(nil), entries...)}))
		return -1, &r
	}
	return idx, nil
}

// Full entry old_text selects now, read under the lock, or the error the direct edit would return
func (s *Store) ResolveEntry(ctx context.Context, target, oldText, verb string) (Result, error) {
	return s.Mutate(ctx, target, func(entries []string, limit int) Mutation {
		idx, errRes := s.locate(entries, pyStrip(oldText), verb, nil)
		if errRes != nil {
			return passthrough(*errRes)
		}
		return passthrough(Result{Data: map[string]any{"success": true, "matched_entry": entries[idx]}, FailureClass: "other"})
	}, true)
}

// Locked whole entry replace when newContent is set, remove otherwise
func (s *Store) edit(ctx context.Context, target, oldText string, newContent *string, matchedEntry any) (Result, error) {
	verb := "remove"
	if newContent != nil && *newContent != "" {
		verb = "replace"
	}
	return s.Mutate(ctx, target, func(entries []string, limit int) Mutation {
		idx, errRes := s.locate(entries, oldText, verb, matchedEntry)
		if errRes != nil {
			return passthrough(*errRes)
		}
		replaced := append([]string(nil), entries[:idx]...)
		if newContent != nil {
			replaced = append(replaced, *newContent)
		}
		replaced = append(replaced, entries[idx+1:]...)
		if newContent == nil {
			return Mutation{Entries: replaced, Message: "Entry removed.", Extra: map[string]any{"removed_entry": entries[idx]}}
		}
		if newTotal := runeLen(strings.Join(replaced, EntryDelimiter)); newTotal > limit {
			return passthrough(s.failureWithEntries(target, fmt.Sprintf(
				"Replacement %s. Shorten the new content, or retry as ONE "+
					"'operations' batch with this replace plus removes/shortenings of other stale entries "+
					"(see current_entries below) — the limit is checked only on the batch result.", overLimit(newTotal, limit))))
		}
		return Mutation{Entries: replaced, Message: "Entry replaced.", Extra: map[string]any{"replaced_entry": entries[idx]}}
	}, false)
}

// One batch op outcome, an error and class, the previous entry or closest entries, and a patched entry
type batchOpOutcome struct {
	errMsg   string
	previous any
	class    string
	patched  *string
	crash    error
}

// Applies one batch op to working, capturing the selected entry before each edit
func applyBatchOp(working *[]string, act any, content, oldText, pos string, matchedEntry any, contentErr error) batchOpOutcome {
	if act == "add" {
		if content == "" {
			return batchOpOutcome{errMsg: pos + ": content is required.", class: "missing_content"}
		}
		if !containsString(*working, content) {
			*working = append(*working, content)
		}
		return batchOpOutcome{class: "none"}
	}
	if act != "replace" && act != "remove" && act != "patch" {
		return batchOpOutcome{errMsg: pos + ": unknown action. Use add, patch, or remove.", class: "invalid_args"}
	}
	if pyStrip(oldText) == "" {
		return batchOpOutcome{errMsg: pos + ": old_text is required.", class: "missing_old_text"}
	}
	if contentErr != nil {
		return batchOpOutcome{crash: contentErr}
	}
	if act == "replace" && content == "" {
		return batchOpOutcome{errMsg: pos + ": content is required — set this operation object's own 'content' to the COMPLETE " +
			"new entry (or use action='remove' to delete).", class: "missing_content"}
	}
	if act == "patch" && pyStrip(content) == "" {
		return batchOpOutcome{errMsg: pos + ": content is required — set this operation object's own 'content' to the new " +
			"text for the old_text span. " + PatchDropHint, class: "missing_content"}
	}
	var idx int
	if matchedEntry != nil {
		idx = pinnedIndex(*working, matchedEntry)
		if idx < 0 {
			return batchOpOutcome{errMsg: pos + ": " + staleEntryMessage(matchedEntry), class: "stale_entry"}
		}
	} else {
		var ambiguous bool
		idx, ambiguous = findUniqueMatch(*working, oldText)
		if ambiguous {
			return batchOpOutcome{errMsg: fmt.Sprintf("%s: '%s' matched multiple distinct entries -- be more specific.", pos, oldText), class: "ambiguous"}
		}
		if idx < 0 {
			miss := noMatchError(*working, oldText, pyStr(act), nil)
			out := batchOpOutcome{errMsg: pos + ": " + miss.Data["error"].(string), class: "no_match"}
			if closest, ok := miss.Data["closest_entries"]; ok {
				out.previous = closest
			}
			return out
		}
	}
	previous := (*working)[idx]
	if act != "patch" {
		rest := append([]string(nil), (*working)[idx+1:]...)
		next := append([]string(nil), (*working)[:idx]...)
		if act == "replace" {
			next = append(next, content)
		}
		*working = append(next, rest...)
		return batchOpOutcome{previous: previous, class: "none"}
	}
	patched, message, class, ok := patchEntry(previous, oldText, content)
	if !ok {
		return batchOpOutcome{errMsg: pos + ": " + message, class: class}
	}
	(*working)[idx] = patched
	return batchOpOutcome{previous: previous, class: "none", patched: &patched}
}

// Applies add, patch and remove ops atomically against the final budget, all or nothing
func (s *Store) ApplyBatch(ctx context.Context, target string, operations []map[string]any) (Result, error) {
	return s.batch(ctx, target, operations, true)
}

// Dry run of ApplyBatch under the lock returning the full entry each op selects now
func (s *Store) ResolveBatchEntries(ctx context.Context, target string, operations []map[string]any) (Result, error) {
	return s.batch(ctx, target, operations, false)
}

func (s *Store) batch(ctx context.Context, target string, operations []map[string]any, commit bool) (Result, error) {
	var named any
	if target != "" {
		named = target
	}
	if targetErr := memoryTargetError(s, named, operations); targetErr != nil {
		return *targetErr, nil
	}
	if len(operations) == 0 {
		return failure("operations list is empty.", "invalid_args", nil), nil
	}
	ops := make([]map[string]any, len(operations))
	for i, op := range operations {
		if op == nil {
			op = map[string]any{}
		}
		ops[i] = op
	}
	for i, op := range ops {
		act := op["action"]
		if err := hashable(act); err != nil {
			return Result{}, err
		}
		if (act == "add" || act == "replace") && pyTruthy(op["content"]) {
			text, err := scanText(op["content"])
			if err != nil {
				return Result{}, err
			}
			if scanErr := FirstThreatMessage(text, ScopeStrict); scanErr != "" {
				return failure(fmt.Sprintf("Operation %d: %s", i+1, scanErr), "scan_blocked", nil), nil
			}
		}
	}
	var opErr error
	res, err := s.Mutate(ctx, target, func(entries []string, limit int) Mutation {
		working := append([]string(nil), entries...)
		matched := make([]any, 0, len(ops))
		produced := map[string]any{}
		for i, op := range ops {
			act := op["action"]
			content, oldText, contentErr, err := batchOpText(op, act != "patch")
			if err != nil {
				opErr = err
				return passthrough(Result{})
			}
			pos := fmt.Sprintf("Operation %d (%s)", i+1, pyStrOr(act, "unknown"))
			out := applyBatchOp(&working, act, content, oldText, pos, op["matched_entry"], contentErr)
			if out.crash != nil {
				opErr = out.crash
				return passthrough(Result{})
			}
			if out.errMsg != "" {
				closest, _ := out.previous.([]string)
				return passthrough(s.batchFailure(target, out.errMsg, out.class, closest))
			}
			matched = append(matched, out.previous)
			if out.patched != nil {
				produced[strconv.Itoa(i+1)] = *out.patched
			}
		}
		if len(entries) > 0 && len(working) == 0 && !(len(ops) == 1 && ops[0]["action"] == "remove") {
			label := filepath.Base(s.PathFor(target))
			return passthrough(s.batchFailure(target, fmt.Sprintf(
				"Refusing to empty %s: this batch would remove every entry from a "+
					"previously non-empty store. Keep at least one entry — merge overlapping "+
					"entries into a shorter one instead of removing the last one. To delete the "+
					"final entry deliberately, remove it alone in a one-item batch.", label), "would_empty", nil))
		}
		newTotal := runeLen(strings.Join(working, EntryDelimiter))
		allRemove := true
		for _, op := range ops {
			if op["action"] != "remove" {
				allRemove = false
				break
			}
		}
		grows := !(allRemove || (len(ops) == 1 && equalStrings(working, entries)))
		if grows && newTotal > limit {
			if len(ops) == 1 && ops[0]["action"] == "add" {
				content, _, _, _ := batchOpText(ops[0], true)
				return passthrough(s.addOverflow(target, content, limit))
			}
			return passthrough(s.batchFailure(target, fmt.Sprintf(
				"Applying all %d operations %s more. Add "+
					"removes or shortening patches of other stale entries to the same batch, then retry.",
				len(operations), overLimit(newTotal, limit)), "over_budget", nil))
		}
		if !commit {
			return passthrough(Result{Data: map[string]any{"success": true, "matched_entries": matched}, FailureClass: "other"})
		}
		replaced, removed, patchedPrev := map[string]any{}, map[string]any{}, map[string]any{}
		for i, op := range ops {
			if matched[i] == nil {
				continue
			}
			key := strconv.Itoa(i + 1)
			switch op["action"] {
			case "replace":
				replaced[key] = matched[i]
			case "patch":
				patchedPrev[key] = matched[i]
			case "remove":
				removed[key] = matched[i]
			}
		}
		extra := map[string]any{}
		if len(replaced) > 0 {
			extra["replaced_entries"] = replaced
		}
		if len(patchedPrev) > 0 {
			extra["patched_entries"] = patchedPrev
			extra["patched_entries_after"] = produced
		}
		if len(removed) > 0 {
			extra["removed_entries"] = removed
		}
		return Mutation{Entries: working, Message: fmt.Sprintf("Applied %d operation(s).", len(operations)), Extra: extra}
	}, !commit)
	if err != nil {
		return Result{}, err
	}
	if opErr != nil {
		return Result{}, opErr
	}
	return res, nil
}

// Content and old_text of a batch op, stripped for whole entry actions, patch content crashes deferred
func batchOpText(op map[string]any, strip bool) (string, string, error, error) {
	content := op["content"]
	if !pyTruthy(content) {
		content = op["new_text"]
	}
	contentText, contentErr := strMethodArg(content)
	if strip && contentErr != nil {
		return "", "", nil, contentErr
	}
	oldText, err := strMethodArg(op["old_text"])
	if err != nil {
		return "", "", nil, err
	}
	if strip {
		contentText, oldText = pyStrip(contentText), pyStrip(oldText)
	}
	return contentText, oldText, contentErr, nil
}

// Frozen load time block for the system prompt, empty when the target has no entries
func (s *Store) FormatForSystemPrompt(target string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot[target]
}

// Terminal success without the entries list, resetting the per turn failure budget
func (s *Store) successResponse(target, message string, extra map[string]any) Result {
	s.consolidationFailures = 0
	data := map[string]any{
		"success":     true,
		"done":        true,
		"target":      target,
		"usage":       s.usagePct(target, s.charCount(target)),
		"entry_count": len(s.entriesFor(target)),
	}
	if message != "" {
		data["message"] = message
	}
	for k, v := range extra {
		data[k] = v
	}
	data["note"] = "Write saved. This update is complete — do not repeat it."
	return Result{Data: data, FailureClass: "other"}
}

// Prompt block with header, usage indicator and entries, empty when there are none
func (s *Store) renderBlock(target string, entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	content := strings.Join(entries, EntryDelimiter)
	sep := strings.Repeat("═", 46)
	title := BlockHeaders["memory"]
	if target == "user" {
		title = BlockHeaders["user"]
	}
	return fmt.Sprintf("%s\n%s [%s]\n%s\n%s", sep, title, s.usagePct(target, runeLen(content)), sep, content)
}

// File reader and clock the store uses, swapped by tests for a failing read or a pinned second
var (
	readMemoryFile = os.ReadFile
	memoryNow      = time.Now
)

// Raw text and whether it was readable, false only when the file exists but cannot be decoded or read
func readRawChecked(path string) (string, bool) {
	if _, err := os.Stat(path); err != nil {
		if missingPathError(err) {
			return "", true
		}
		return "", false
	}
	data, err := readMemoryFile(path)
	if err != nil {
		return "", false
	}
	data = []byte(strings.TrimPrefix(string(data), "\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return "", false
	}
	return universalNewlines(string(data)), true
}

// Stripped non empty entries split on the full delimiter so a bare section sign survives
func ParseEntries(raw string) []string {
	var out []string
	for _, x := range strings.Split(raw, EntryDelimiter) {
		if e := pyStrip(x); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Entries of a memory file, empty on any read failure, for read only callers
func ReadEntriesFile(path string) []string {
	raw, _ := readRawChecked(path)
	return ParseEntries(raw)
}

// Atomic temp file and rename of the joined entries, a write failure wrapped as the runtime error
func writeEntriesFile(ctx context.Context, path string, entries []string) error {
	err := pyfs.AtomicWriteText(ctx, path, strings.Join(entries, EntryDelimiter), ".mem_")
	if err == nil {
		return nil
	}
	var ee *pyfs.EncodeError
	if errors.As(err, &ee) {
		return &pyError{class: "UnicodeEncodeError", msg: ee.Msg}
	}
	return &pyError{class: "RuntimeError", msg: fmt.Sprintf("Failed to write memory file %s: %s", path, osErrorText(err))}
}

// True when one parsed entry is longer than the whole file limit
func (s *Store) hasOversizedEntry(target, raw string) bool {
	longest := 0
	for _, e := range ParseEntries(raw) {
		longest = max(longest, runeLen(e))
	}
	return longest > s.charLimit(target)
}

// Exclusive bak snapshot of raw text, suffixed with the failure marker when no copy could be written
func (s *Store) driftBackup(target, raw string) string {
	path := s.PathFor(target)
	stem := path + ".bak." + strconv.FormatInt(memoryNow().Unix(), 10)
	for n := 0; ; n++ {
		bak := stem
		if n > 0 {
			bak += "." + strconv.Itoa(n)
		}
		f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return bak + backupFailedSuffix
		}
		_, werr := f.WriteString(platformNewlines(raw))
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return bak + backupFailedSuffix
		}
		return bak
	}
}

// Translates CRLF and lone CR to LF like Python universal newline reads
func universalNewlines(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Order preserving dedupe where the first occurrence wins
func dedupe(entries []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Python str isspace, Unicode white space plus the four information separators
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Python str isalnum on one code point
func pyIsAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func pyFields(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// First n code points of s
func runePrefix(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Integer with comma thousands separators like the Python comma format spec
func thousands(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Python str lower with the dotted capital I expansion and the final sigma rule
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch r {
		case 0x130:
			b.WriteString("i\xcc\x87")
		case 0x3a3:
			b.WriteRune(lowerSigma(rs, i))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// Word break punctuation Python treats as case ignorable for the final sigma rule
var caseIgnorablePunct = map[rune]bool{
	0x27: true, 0x2e: true, 0x3a: true, 0xb7: true, 0x387: true, 0x55f: true, 0x5f4: true, 0x2018: true,
	0x2019: true, 0x2024: true, 0x2027: true, 0xfe13: true, 0xfe52: true, 0xfe55: true, 0xff07: true,
	0xff0e: true, 0xff1a: true,
}

func caseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) || caseIgnorablePunct[r]
}

func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// Final small sigma when a cased letter precedes and none follows past case ignorable runes
func lowerSigma(rs []rune, i int) rune {
	j := i - 1
	for j >= 0 && caseIgnorable(rs[j]) {
		j--
	}
	final := j >= 0 && cased(rs[j])
	if final && i+1 < len(rs) {
		j = i + 1
		for j < len(rs) && caseIgnorable(rs[j]) {
			j++
		}
		final = j == len(rs) || !cased(rs[j])
	}
	if final {
		return 0x3c2
	}
	return 0x3c3
}

// Python truthiness of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case []map[string]any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// True only for a real boolean false, like the Python is False test
func isFalse(v any) bool {
	b, ok := v.(bool)
	return ok && !b
}

// Python type name of a decoded JSON value, integral numbers reading as int
func pyTypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "int"
		}
		return "float"
	case int, int64:
		return "int"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "float"
		}
		return "int"
	case []any, []string, []map[string]any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Python str of a decoded JSON value
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e21 {
			return strconv.FormatFloat(x, 'f', 0, 64)
		}
		return pyFloatRepr(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return string(x)
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return pyStr(items)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Python float repr, shortest round trip digits with exponent outside the fixed range
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	exp := 0
	if f != 0 {
		exp = int(math.Floor(math.Log10(math.Abs(f))))
	}
	if exp < -4 || exp >= 16 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, e, _ := strings.Cut(s, "e")
		sign := e[0]
		digits := strings.TrimLeft(e[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return mant + "e" + string(sign) + digits
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// Python repr, strings quoted and escaped, everything else as str
func pyRepr(v any) string {
	s, ok := v.(string)
	if !ok {
		return pyStr(v)
	}
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == utf8.RuneError || !unicode.IsPrint(r) && r != ' ':
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// Python str of a value, the fallback when it is falsy like an or expression
func pyStrOr(v any, fallback string) string {
	if !pyTruthy(v) {
		return fallback
	}
	return pyStr(v)
}

// Python exception class and message for crash paths reported like the registry dispatcher
type pyError struct {
	class string
	msg   string
}

func (e *pyError) Error() string { return e.msg }

// Text a falsy value collapses to, a crash for a truthy non string before a str method call
func strMethodArg(v any) (string, error) {
	if !pyTruthy(v) {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", &pyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'strip'", pyTypeName(v))}
}

// Text the threat scan reads, a crash for a value Python could not slice and normalize
func scanText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case []any:
		for _, item := range x {
			if err := hashable(item); err != nil {
				return "", err
			}
		}
		return "", &pyError{class: "TypeError", msg: "normalize() argument 2 must be str, not list"}
	case map[string]any:
		return "", &pyError{class: "TypeError", msg: "unhashable type: 'slice'"}
	}
	return "", &pyError{class: "TypeError", msg: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(v))}
}

// Crash for a value Python cannot hash in a set or dict membership test
func hashable(v any) error {
	switch v.(type) {
	case []any, []string, []map[string]any, map[string]any:
		return &pyError{class: "TypeError", msg: fmt.Sprintf("unhashable type: '%s'", pyTypeName(v))}
	}
	return nil
}

// Stat failures Python pathlib treats as a missing path
func missingPathError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EBADF)
}

// Python OSError text, the errno label, capitalized strerror and quoted filename
func osErrorText(err error) string {
	var pe *pyError
	if errors.As(err, &pe) {
		return pe.msg
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	msg := errno.Error()
	if msg != "" {
		r, size := utf8.DecodeRuneInString(msg)
		msg = string(unicode.ToUpper(r)) + msg[size:]
	}
	text := fmt.Sprintf("[%s %d] %s", errnoLabel, int(errno), msg)
	var pathErr *os.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &linkErr):
		text += fmt.Sprintf(": %s -> %s", pyRepr(linkErr.Old), pyRepr(linkErr.New))
	case errors.As(err, &pathErr):
		text += ": " + pyRepr(pathErr.Path)
	}
	return text
}
