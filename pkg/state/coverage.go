package state

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Gateway timestamp prefix the merged user run matcher strips before comparing joined text
var coverageTimestampPrefixRE = mustPyRe(`^\[(?:` +
	`(?P<dow>[A-Z][a-z]{2}) ` +
	`(?P<date>\d{4}-\d{2}-\d{2}) ` +
	`(?P<time>\d{2}:\d{2}:\d{2})` +
	`(?: (?P<tz>[A-Za-z0-9_+\-/:]+))?` +
	`|(?P<iso>\d{4}-\d{2}-\d{2}T[^\]]+)` +
	`)\]\s*`)

var coverageTextPartTypes = setOf("text", "input_text", "output_text")

var compressorImagePartTypes = setOf("image_url", "input_image", "image")

// Text with every leading gateway timestamp prefix removed, as the gateway strips it
func stripTimestampPrefixes(text string) string {
	for text != "" {
		m, _ := coverageTimestampPrefixRE.FindStringMatch(text)
		if m == nil {
			break
		}
		start, length := m.ByteRange()
		text = text[start+length:]
	}
	return text
}

// String content as is, text parts joined with newlines, image parts and anything else empty
func coverageTextOf(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	items, ok := listItems(content)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		part, isMap := dictValue(item)
		if !isMap {
			continue
		}
		if kind, _ := part["type"].(string); !coverageTextPartTypes[kind] {
			continue
		}
		if pyTruthy(part["text"]) {
			parts = append(parts, pyrepr.Str(part["text"]))
		} else {
			parts = append(parts, "")
		}
	}
	return strings.Join(parts, "\n")
}

// True when inner sits in outer as whole lines, blanks beside the hit counting as line edges
func holdsAsBlock(inner, outer string) bool {
	start := strings.Index(outer, inner)
	for start != -1 {
		end := start + len(inner)
		before := outer[:start]
		if i := strings.LastIndex(before, "\n"); i >= 0 {
			before = before[i+1:]
		}
		after := outer[end:]
		if i := strings.Index(after, "\n"); i >= 0 {
			after = after[:i]
		}
		if pyStrip(before) == "" && pyStrip(after) == "" {
			return true
		}
		step := 1
		if start < len(outer) {
			_, step = utf8.DecodeRuneInString(outer[start:])
		}
		if start+step > len(outer) {
			return false
		}
		next := strings.Index(outer[start+step:], inner)
		if next == -1 {
			return false
		}
		start += step + next
	}
	return false
}

// Image block of a multimodal content list
func isCompressorImagePart(part any) bool {
	m, ok := dictValue(part)
	if !ok {
		return false
	}
	kind, isStr := m["type"].(string)
	return isStr && compressorImagePartTypes[kind]
}

// True when held is stored with at least one image part swapped for a text part, all else equal
func imagesReplaced(stored, held any) bool {
	was, ok1 := listItems(stored)
	now, ok2 := listItems(held)
	if !ok1 || !ok2 || len(was) != len(now) {
		return false
	}
	swapped := false
	for i := range was {
		if pyrepr.Equal(was[i], now[i]) {
			continue
		}
		part, isMap := dictValue(now[i])
		if !isCompressorImagePart(was[i]) || !isMap || part["type"] != "text" {
			return false
		}
		swapped = true
	}
	return swapped
}

// Python float of a stored timestamp cell, falsy cells read as zero
func rowTimestampFloat(v any) (float64, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	return pyFloatOf(v)
}

// Positive int row id a dict names, false for a bool, non int or non positive value
func namedRowID(v any) (int64, bool) {
	if _, isBool := v.(bool); isBool {
		return 0, false
	}
	n, ok := pyIntID(v)
	return n, ok && n > 0
}

// First occurrence order of ids with duplicates removed
func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func int64sToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func messageIdentity(message map[string]any) (pyrepr.Tuple, error) {
	role := any("unknown")
	if r, ok := message["role"]; ok {
		role = r
	}
	return rowIdentity(role, message["content"], message["tool_call_id"], parseToolCalls(message["tool_calls"]))
}

func storedRowIdentity(row map[string]any) (pyrepr.Tuple, error) {
	return rowIdentity(row["role"], decodeMessageContent(row["content"]), row["tool_call_id"], parseToolCalls(row["tool_calls"]))
}

func rowUID(row map[string]any) string {
	if pyTruthy(row[MessageUID]) {
		return pyrepr.Str(row[MessageUID])
	}
	return ""
}

type heldByRow struct {
	index    int
	identity pyrepr.Tuple
	uid      string
	hasUID   bool
}

type heldByUID struct {
	index    int
	identity pyrepr.Tuple
}

// Held dicts by the name they carry, row id first and message uid otherwise, unnamed dicts never claim
func indexHeldDicts(compacted []map[string]any) (map[int64]heldByRow, map[string][]heldByUID, error) {
	byRow := map[int64]heldByRow{}
	byUID := map[string][]heldByUID{}
	for index, message := range compacted {
		if message == nil {
			continue
		}
		identity, err := messageIdentity(message)
		if err != nil {
			return nil, nil, err
		}
		uid, hasUID := MessageUIDOrNone(message)
		if rowID, ok := namedRowID(message["_row_id"]); ok {
			if _, seen := byRow[rowID]; !seen {
				byRow[rowID] = heldByRow{index, identity, uid, hasUID}
			}
		} else if hasUID {
			byUID[uid] = append(byUID[uid], heldByUID{index, identity})
		}
	}
	return byRow, byUID, nil
}

// Tail rows the compacted messages already carry, mapped to the carrying index, with the rewritten carries apart
func (d *SessionDB) carriedTailIDs(ctx context.Context, conn SQLiteConn, tailIDs []int64, compacted []map[string]any) (map[int64]int, map[int64]bool, error) {
	carried, weak := map[int64]int{}, map[int64]bool{}
	if len(tailIDs) == 0 || len(compacted) == 0 {
		return carried, weak, nil
	}
	byRow, byUID, err := indexHeldDicts(compacted)
	if err != nil {
		return nil, nil, err
	}
	rows, err := connRows(ctx, conn, "SELECT * FROM messages WHERE id IN ("+placeholders(len(tailIDs))+") ORDER BY id", int64sToAny(tailIDs)...)
	if err != nil {
		return nil, nil, err
	}
	claimed := func() map[int]bool {
		out := make(map[int]bool, len(carried))
		for _, index := range carried {
			out[index] = true
		}
		return out
	}
	for _, row := range rows {
		index, ok, err := d.claimStoredCopy(row, compacted, byRow, byUID, claimed())
		if err != nil {
			return nil, nil, err
		}
		if ok {
			id, _ := pyIntID(row["id"])
			carried[id] = index
		}
	}
	namedUIDs := map[string]bool{}
	for uid := range byUID {
		namedUIDs[uid] = true
	}
	for _, named := range byRow {
		if named.hasUID {
			namedUIDs[named.uid] = true
		}
	}
	heldElsewhere, err := uidsActiveOutside(ctx, conn, rows, tailIDs, namedUIDs)
	if err != nil {
		return nil, nil, err
	}
	tailUIDs := map[string]int{}
	for _, row := range rows {
		if uid := rowUID(row); uid != "" {
			tailUIDs[uid]++
		}
	}
	for _, row := range rows {
		rowID, _ := pyIntID(row["id"])
		uid := rowUID(row)
		if _, done := carried[rowID]; done || uid == "" || tailUIDs[uid] != 1 || heldElsewhere[uid] {
			continue
		}
		index, ok, err := d.rewrittenCarry(row, uid, compacted, byRow, byUID, claimed())
		if err != nil {
			return nil, nil, err
		}
		if ok {
			carried[rowID] = index
			weak[rowID] = true
		}
	}
	return carried, weak, nil
}

// First pass claim, the unclaimed dict naming the row that replays exactly as the row does
func (d *SessionDB) claimStoredCopy(row map[string]any, compacted []map[string]any, byRow map[int64]heldByRow, byUID map[string][]heldByUID, claimed map[int]bool) (int, bool, error) {
	identity, err := storedRowIdentity(row)
	if err != nil {
		return 0, false, err
	}
	rowID, _ := pyIntID(row["id"])
	var candidates []int
	if named, ok := byRow[rowID]; ok && pyrepr.Equal(named.identity, identity) {
		candidates = append(candidates, named.index)
	}
	if uid := rowUID(row); uid != "" {
		for _, held := range byUID[uid] {
			if pyrepr.Equal(held.identity, identity) {
				candidates = append(candidates, held.index)
			}
		}
	}
	for _, index := range candidates {
		if claimed[index] {
			continue
		}
		replays, err := d.replaysAs(row, compacted[index], true)
		if err != nil {
			return 0, false, err
		}
		if replays {
			return index, true, nil
		}
	}
	return 0, false, nil
}

// Second pass claim, the one unclaimed dict naming the row whose content the compressor rewrite explains
func (d *SessionDB) rewrittenCarry(row map[string]any, uid string, compacted []map[string]any, byRow map[int64]heldByRow, byUID map[string][]heldByUID, claimed map[int]bool) (int, bool, error) {
	rowID, _ := pyIntID(row["id"])
	var candidates []int
	if named, ok := byRow[rowID]; ok && named.hasUID && named.uid == uid {
		candidates = []int{named.index}
	} else {
		for _, held := range byUID[uid] {
			candidates = append(candidates, held.index)
		}
	}
	candidates = slices.DeleteFunc(candidates, func(index int) bool { return claimed[index] })
	if len(candidates) != 1 {
		return 0, false, nil
	}
	explains, err := d.rewriteExplains(row, compacted[candidates[0]])
	if err != nil || !explains {
		return 0, false, err
	}
	return candidates[0], true, nil
}

// Whether held replays the api_content and reasoning sidecars the next request would read from the row
func (d *SessionDB) replaysAs(row, held map[string]any, sameContent bool) (bool, error) {
	ts, err := rowTimestampFloat(row["timestamp"])
	if err != nil {
		return false, err
	}
	sessionID, _ := row["session_id"].(string)
	serialized, err := d.serializedMessageRow(sessionID, held, ts)
	if err != nil {
		return false, err
	}
	content := decodeMessageContent(row["content"])
	rowAPI, heldAPI := row["api_content"], serialized["api_content"]
	if pyrepr.Equal(rowAPI, content) {
		rowAPI = nil
	}
	if pyrepr.Equal(heldAPI, held["content"]) {
		heldAPI = nil
	}
	for _, column := range ReplaySidecars {
		if serialized[column] != nil && !pyrepr.Equal(serialized[column], row[column]) {
			return false, nil
		}
	}
	if heldAPI != nil || rowAPI == nil {
		return pyrepr.Equal(heldAPI, rowAPI), nil
	}
	return !sameContent || pyrepr.Equal(held["content"], rowAPI), nil
}

// Whether held carries the stored row digest of the row's current version
func copyOfCurrentRow(row, held map[string]any) bool {
	snapshot, ok := held[DBRowSnapshot].(string)
	return ok && snapshot == TranscriptRowSnapshot(row)
}

func orNone(v any) any {
	if pyTruthy(v) {
		return v
	}
	return nil
}

// True when held differs from the stored row only the way the compressor rewrites a carry it keeps
func (d *SessionDB) rewriteExplains(row, held map[string]any) (bool, error) {
	role := row["role"]
	if !pyrepr.Equal(held["role"], role) || !pyrepr.Equal(orNone(held["tool_call_id"]), orNone(row["tool_call_id"])) {
		return false, nil
	}
	if !pyrepr.Equal(orNone(parseToolCalls(held["tool_calls"])), orNone(parseToolCalls(row["tool_calls"]))) {
		return false, nil
	}
	roleText, _ := role.(string)
	stored := loadedViewContent(roleText, decodeMessageContent(row["content"]))
	content := loadedViewContent(roleText, held["content"])
	replays, err := d.replaysAs(row, held, pyrepr.Equal(stored, content))
	if err != nil || !replays {
		return false, err
	}
	if imagesReplaced(stored, content) {
		return true, nil
	}
	if !copyOfCurrentRow(row, held) {
		return false, nil
	}
	storedText := coverageTextOf(stored)
	return pyStrip(storedText) != "" && holdsAsBlock(storedText, coverageTextOf(content)), nil
}

// Named uids of tail rows that an active row below the watermark also carries
func uidsActiveOutside(ctx context.Context, conn SQLiteConn, rows []map[string]any, tailIDs []int64, namedUIDs map[string]bool) (map[string]bool, error) {
	wantedSet := map[string]bool{}
	for _, row := range rows {
		if uid, ok := row[MessageUID].(string); ok && namedUIDs[uid] {
			wantedSet[uid] = true
		}
	}
	found := map[string]bool{}
	if len(wantedSet) == 0 {
		return found, nil
	}
	wanted := make([]string, 0, len(wantedSet))
	for uid := range wantedSet {
		wanted = append(wanted, uid)
	}
	slices.Sort(wanted)
	tail := map[int64]bool{}
	for _, id := range tailIDs {
		tail[id] = true
	}
	sessionID := rows[0]["session_id"]
	for _, chunk := range idChunks(wanted, sqlInChunk) {
		args := append([]any{sessionID}, stringsToAny(chunk)...)
		hits, err := connRows(ctx, conn, "SELECT id, "+MessageUID+" FROM messages WHERE session_id = ? AND active = 1 AND "+MessageUID+" IN ("+placeholders(len(chunk))+")", args...)
		if err != nil {
			return nil, err
		}
		for _, hit := range hits {
			if id, _ := pyIntID(hit["id"]); !tail[id] {
				found[pyrepr.Str(hit[MessageUID])] = true
			}
		}
	}
	return found, nil
}

// Count of the carried tail's dicts that name a row in carried, so the positional rewind takes fewer
func carriedTailSlots(carried map[int64]int, compactedCount, tailCount int) int {
	if tailCount <= 0 {
		return 0
	}
	n := 0
	for _, index := range carried {
		if index >= compactedCount-tailCount {
			n++
		}
	}
	return n
}

type carriedKey struct {
	identity pyrepr.Tuple
	rowID    int64
	hasRowID bool
	stamp    any
}

// Active durable originals of carried forward dicts, by matching row id first, else a unique identity and timestamp
func (d *SessionDB) resolveCarriedRowIDs(ctx context.Context, conn SQLiteConn, sessionID string, carriedMessages []map[string]any) ([]int64, error) {
	if len(carriedMessages) == 0 {
		return []int64{}, nil
	}
	var carried []carriedKey
	for _, message := range carriedMessages {
		if message == nil {
			continue
		}
		identity, err := messageIdentity(message)
		if err != nil {
			return nil, err
		}
		rowID, ok := namedRowID(message["_row_id"])
		carried = append(carried, carriedKey{identity, rowID, ok, message["timestamp"]})
	}
	index := func(ids []int64) (map[int64]pyrepr.Tuple, map[string][]int64, error) {
		byID, byKey := map[int64]pyrepr.Tuple{}, map[string][]int64{}
		narrow, args := "", []any{sessionID}
		if len(ids) > 0 {
			narrow = " AND id IN (" + placeholders(len(ids)) + ")"
			args = append(args, int64sToAny(ids)...)
		}
		rows, err := connRows(ctx, conn, "SELECT id, role, content, tool_call_id, tool_calls, timestamp FROM messages WHERE session_id = ? AND active = 1"+narrow+" ORDER BY id", args...)
		if err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			rid, _ := pyIntID(row["id"])
			identity, err := storedRowIdentity(row)
			if err != nil {
				return nil, nil, err
			}
			byID[rid] = identity
			if ts, ok := pydatetime.CoerceEpoch(row["timestamp"], "", "message timestamp"); ok {
				key := scalarKey(append(slices.Clone([]any(identity)), ts)...)
				byKey[key] = append(byKey[key], rid)
			}
		}
		return byID, byKey, nil
	}
	var rowIDs []int64
	for _, c := range carried {
		if c.hasRowID {
			rowIDs = append(rowIDs, c.rowID)
		}
	}
	allNamed := len(rowIDs) == len(carried)
	narrowIDs := rowIDs
	if !allNamed {
		narrowIDs = nil
	}
	byID, byKey, err := index(narrowIDs)
	if err != nil {
		return nil, err
	}
	if allNamed {
		for _, c := range carried {
			if stored, ok := byID[c.rowID]; !ok || !pyrepr.Equal(stored, c.identity) {
				if byID, byKey, err = index(nil); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	var resolved []int64
	for _, c := range carried {
		if c.hasRowID {
			if stored, ok := byID[c.rowID]; ok && pyrepr.Equal(stored, c.identity) {
				resolved = append(resolved, c.rowID)
				continue
			}
		}
		ts, ok := pydatetime.CoerceEpoch(c.stamp, "", "message timestamp")
		if !ok {
			continue
		}
		if matches := byKey[scalarKey(append(slices.Clone([]any(c.identity)), ts)...)]; len(matches) == 1 {
			resolved = append(resolved, matches[0])
		}
	}
	return uniqueIDs(resolved), nil
}

// Active row ids whose stored role and content equal the message, empty when it was never persisted
func matchingActiveIDs(ctx context.Context, conn SQLiteConn, sessionID string, message map[string]any) ([]int64, error) {
	content, ok := message["content"].(string)
	if !ok {
		return nil, nil
	}
	role := "unknown"
	if r, present := message["role"]; present {
		role, _ = r.(string)
	}
	stored := encodeMessageContent(loadedViewContent(role, content))
	rows, err := connRows(ctx, conn, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND content = ?", sessionID, message["role"], stored)
	if err != nil {
		return nil, err
	}
	return rowIDList(rows), nil
}

func rowIDList(rows []map[string]any) []int64 {
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		id, _ := pyIntID(row["id"])
		out = append(out, id)
	}
	return out
}

// Python list of a value, a string split into characters, a TypeError for a non iterable
func pyListOf(v any) ([]any, error) {
	if !pyTruthy(v) {
		return nil, nil
	}
	return seqIter(v)
}

// Active rows equal to a row the alternation repair retired without an id, at or below the watermark
func matchingRetiredIDs(ctx context.Context, conn SQLiteConn, sessionID string, retired map[string]any, watermark *int64) ([]int64, error) {
	role := ""
	if pyTruthy(retired["role"]) {
		role = pyrepr.Str(retired["role"])
	}
	query, args := "SELECT id, content, tool_call_id, tool_calls FROM messages WHERE session_id = ? AND active = 1 AND role = ?", []any{sessionID, role}
	if watermark != nil {
		query += " AND id <= ?"
		args = append(args, *watermark)
	}
	rows, err := connRows(ctx, conn, query+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	wantCalls, err := pyListOf(retired["tool_call_ids"])
	if err != nil {
		return nil, err
	}
	if wantCalls == nil {
		wantCalls = []any{}
	}
	var matches []int64
	for _, row := range rows {
		content := loadedViewContent(role, decodeMessageContent(row["content"]))
		if !pyrepr.Equal(orNone(content), orNone(retired["content"])) {
			continue
		}
		if !pyrepr.Equal(orNone(row["tool_call_id"]), orNone(retired["tool_call_id"])) {
			continue
		}
		ids := []any{}
		if calls, isList := listItems(orNone(parseToolCalls(row["tool_calls"]))); isList {
			for _, call := range calls {
				if m, isMap := dictValue(call); isMap {
					ids = append(ids, m["id"])
				}
			}
		}
		if !pyrepr.Equal(ids, wantCalls) {
			continue
		}
		id, _ := pyIntID(row["id"])
		matches = append(matches, id)
	}
	return matches, nil
}

// Active rows an alternation repair merged into the message, empty for none, false when ambiguous
func mergedUserRun(ctx context.Context, conn SQLiteConn, sessionID string, message map[string]any, watermark *int64) ([]int64, bool, error) {
	content, isStr := message["content"].(string)
	width64, isInt := exactInt(message[MergedDurableRows])
	width := int(width64)
	if message["role"] != "user" || !isStr || !isInt || width < 2 {
		return []int64{}, true, nil
	}
	content = stripTimestampPrefixes(content)
	query, args := "SELECT id, role, content FROM messages WHERE session_id = ? AND active = 1", []any{sessionID}
	if watermark != nil {
		query += " AND id <= ?"
		args = append(args, *watermark)
	}
	stored, err := connRows(ctx, conn, query+" ORDER BY id", args...)
	if err != nil {
		return nil, false, err
	}
	retiredIDs := map[int64]bool{}
	retiredRows, err := pyListOf(message[RetiredDurableRows])
	if err != nil {
		return nil, false, err
	}
	for _, item := range retiredRows {
		retired, isMap := dictValue(item)
		if !isMap {
			return nil, false, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", seqTypeName(item))
		}
		if pyTruthy(retired[OwnRow]) {
			continue
		}
		matches, err := matchingRetiredIDs(ctx, conn, sessionID, retired, watermark)
		if err != nil {
			return nil, false, err
		}
		if len(matches) != 1 {
			return nil, false, nil
		}
		retiredIDs[matches[0]] = true
	}
	type runRow struct {
		id   int64
		role any
		part any
	}
	var rows []runRow
	for _, row := range stored {
		id, _ := pyIntID(row["id"])
		if retiredIDs[id] {
			continue
		}
		roleText, _ := row["role"].(string)
		rows = append(rows, runRow{id, row["role"], loadedViewContent(roleText, decodeMessageContent(row["content"]))})
	}
	var runs [][]int64
	for start := range rows {
		merged := ""
		for end := start; end < len(rows); end++ {
			part, partStr := rows[end].part.(string)
			if rows[end].role != "user" || !partStr {
				break
			}
			if merged != "" && part != "" {
				merged = merged + "\n\n" + part
			} else if merged == "" {
				merged = part
			}
			if !strings.HasPrefix(content, merged) {
				break
			}
			if end-start+1 == width {
				if merged == content {
					run := make([]int64, 0, width)
					for i := start; i <= end; i++ {
						run = append(run, rows[i].id)
					}
					runs = append(runs, run)
				}
				break
			}
		}
	}
	if len(runs) > 1 {
		return nil, false, nil
	}
	if len(runs) == 1 {
		return runs[0], true, nil
	}
	return []int64{}, true, nil
}

// Ids safe to archive as summarized and ids merged into another held dict, false when a held row has no name
func provedCoverage(ctx context.Context, conn SQLiteConn, sessionID string, coveredIDs []int64, unresolvedHeld []map[string]any, watermark *int64) ([]int64, map[int64]bool, bool, error) {
	if coveredIDs == nil {
		return nil, nil, false, nil
	}
	var proved []int64
	for _, id := range coveredIDs {
		if id > 0 {
			proved = append(proved, id)
		}
	}
	mergedAway := map[int64]bool{}
	for _, message := range unresolvedHeld {
		if message == nil {
			continue
		}
		if pyTruthy(message[RetiredRow]) {
			matches, err := matchingRetiredIDs(ctx, conn, sessionID, message, watermark)
			if err != nil || len(matches) != 1 {
				return nil, nil, false, err
			}
			proved = append(proved, matches...)
			if !pyTruthy(message[OwnRow]) {
				mergedAway[matches[0]] = true
			}
			continue
		}
		retiredRows, err := pyListOf(message[RetiredDurableRows])
		if err != nil {
			return nil, nil, false, err
		}
		if slices.ContainsFunc(retiredRows, func(item any) bool {
			m, ok := dictValue(item)
			return ok && pyTruthy(m[OwnRow])
		}) {
			continue
		}
		run, named, err := mergedUserRun(ctx, conn, sessionID, message, watermark)
		if err != nil {
			return nil, nil, false, err
		}
		if pyTruthy(message[MergedDurableRows]) && (!named || len(run) == 0) {
			return nil, nil, false, nil
		}
		var matches []int64
		if len(run) == 0 {
			if matches, err = matchingActiveIDs(ctx, conn, sessionID, message); err != nil {
				return nil, nil, false, err
			}
		}
		if len(matches) > 1 || (pyTruthy(message[DBPersistedMarker]) && len(matches) != 1 && len(run) == 0) {
			return nil, nil, false, nil
		}
		if len(matches) > 0 {
			proved = append(proved, matches...)
		} else {
			proved = append(proved, run...)
		}
		absorbed := map[int64]bool{}
		absorbedItems, err := pyListOf(message[AbsorbedRowIDs])
		if err != nil {
			return nil, nil, false, err
		}
		for _, item := range absorbedItems {
			if n, ok := pyIntID(item); ok {
				absorbed[n] = true
			} else if f, isFloat := item.(float64); isFloat && f == float64(int64(f)) {
				absorbed[int64(f)] = true
			}
		}
		if len(run) > 1 {
			for _, id := range run[1:] {
				if !absorbed[id] {
					mergedAway[id] = true
				}
			}
		}
	}
	return uniqueIDs(proved), mergedAway, true, nil
}

// Python len of a value or the error len raises
func pyLenOf(v any) (int, error) {
	if s, ok := v.(string); ok {
		return pyLen(s), nil
	}
	if m, ok := dictValue(v); ok {
		return len(m), nil
	}
	items, err := seqIter(v)
	return len(items), err
}

// Durable rows behind the carried tail beyond one per dict, for the positional rewind
func uncountedMergedRows(tail []map[string]any) (int, error) {
	total := 0
	for _, message := range tail {
		if message == nil {
			continue
		}
		switch message["_row_id"].(type) {
		case int, int64, bool:
			continue
		}
		unlisted := int64(0)
		if merged, ok := exactInt(message[MergedDurableRows]); ok {
			absorbed := int64(0)
			if pyTruthy(message[AbsorbedRowIDs]) {
				n, err := pyLenOf(message[AbsorbedRowIDs])
				if err != nil {
					return 0, err
				}
				absorbed = int64(n)
			}
			unlisted = merged - 1 - absorbed
		}
		unnamed, _ := exactInt(message[UnnamedDurableRows])
		total += int(max(0, unlisted) + unnamed)
	}
	return total, nil
}

// Newest rows behind tailCount carried dicts, a dict that merged rows standing for each of them
func tailOriginals(coveredActive []int64, tailCount int, mergedAway map[int64]bool) []int64 {
	width := tailCount
	for {
		window := pyTail(coveredActive, width)
		need := tailCount
		for _, id := range window {
			if mergedAway[id] {
				need++
			}
		}
		if need <= width || width >= len(coveredActive) {
			return window
		}
		width = need
	}
}

// Last n items as Python negative slicing gives them, the whole slice for zero or past its length
func pyTail[T any](items []T, n int) []T {
	if n <= 0 || n >= len(items) {
		return items
	}
	return items[len(items)-n:]
}
