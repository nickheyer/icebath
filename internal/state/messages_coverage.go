package state

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Leading gateway timestamp prefix of a rendered user turn, the human form or the older ISO form
var messageTimestampPrefix = regexp.MustCompile(`^\[(?:[A-Z][a-z]{2} \p{Nd}{4}-\p{Nd}{2}-\p{Nd}{2} \p{Nd}{2}:\p{Nd}{2}:\p{Nd}{2}(?: [A-Za-z0-9_+\-/:]+)?|\p{Nd}{4}-\p{Nd}{2}-\p{Nd}{2}T[^\]]+)\]` + fenceSpace + `*`)

// Text with every leading gateway timestamp prefix removed
func stripLeadingMessageTimestamps(text string) string {
	for {
		loc := messageTimestampPrefix.FindStringIndex(text)
		if loc == nil {
			return text
		}
		text = text[loc[1]:]
	}
}

// One id-less entry of the history a compressor held, a message or a row an alternation repair retired
type Held struct {
	Message *Message
	Retired *RetiredRow
}

// Text a message carries, its string or its text parts joined by newlines
func textOf(c Content) string {
	if text, ok := c.Text(); ok {
		return text
	}
	parts, ok := contentParts(c)
	if !ok {
		return ""
	}
	var texts []string
	for _, p := range parts {
		switch p.partType() {
		case "text", "input_text", "output_text":
			text, _ := p.partText()
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// Reports inner sitting in outer as whole lines, blanks beside the hit counting as line edges
func holdsAsBlock(inner, outer string) bool {
	for from := 0; ; {
		at := strings.Index(outer[from:], inner)
		if at < 0 {
			return false
		}
		start := from + at
		end := start + len(inner)
		before := outer[:start]
		if i := strings.LastIndexByte(before, '\n'); i >= 0 {
			before = before[i+1:]
		}
		after, _, _ := strings.Cut(outer[end:], "\n")
		if strings.TrimFunc(before, previewSpace) == "" && strings.TrimFunc(after, previewSpace) == "" {
			return true
		}
		from = start + 1
		if from > len(outer) {
			return false
		}
	}
}

// Order insensitive comparison text of a JSON value
func jsonCompareKey(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	enc, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(enc)
}

// Reports held content that is stored content with at least one image part swapped for a text part
func imagesReplaced(stored, held Content) bool {
	was, ok1 := contentParts(stored)
	now, ok2 := contentParts(held)
	if !ok1 || !ok2 || len(was) != len(now) {
		return false
	}
	swapped := false
	for i := range was {
		if partsContentKey(was[i]) == partsContentKey(now[i]) {
			continue
		}
		switch was[i].partType() {
		case "image", "image_url", "input_image":
		default:
			return false
		}
		if now[i].partType() != "text" {
			return false
		}
		swapped = true
	}
	return swapped
}

// Comparison text of one content part
func partsContentKey(p contentPart) string {
	enc, _ := partsContent([]contentPart{p}).MarshalJSON()
	return jsonCompareKey(enc)
}

// Comparison text of content with every falsy value collapsed to none
func falsyCollapsedKey(c Content) string {
	if !c.truthy() {
		return ""
	}
	return contentCompareKey(c)
}

// Comparison text of a tool calls value with every falsy value collapsed to none
func toolCallsCompareKey(parsed json.RawMessage) string {
	if !jsonTruthy(parsed) {
		return ""
	}
	return jsonCompareKey(parsed)
}

// Held message named by a row id, its position, identity and message identity
type namedHeld struct {
	index    int
	identity string
	uid      string
}

// Held message named by its message identity, its position and identity
type uidHeld struct {
	index    int
	identity string
}

// Held messages by the name they carry, a row id first and else a message identity
func indexHeldMessages(compacted []*Message) (map[int64]namedHeld, map[string][]uidHeld, error) {
	byRow, byUID := map[int64]namedHeld{}, map[string][]uidHeld{}
	for i, m := range compacted {
		if m == nil {
			continue
		}
		identity, err := liveRowIdentity(m)
		if err != nil {
			return nil, nil, err
		}
		if m.RowID > 0 {
			if _, seen := byRow[m.RowID]; !seen {
				byRow[m.RowID] = namedHeld{i, identity, m.UID}
			}
		} else if m.UID != "" {
			byUID[m.UID] = append(byUID[m.UID], uidHeld{i, identity})
		}
	}
	return byRow, byUID, nil
}

// Text cell of a row as nullable text for comparisons, an empty cell reading as none
func textOrNone(v any) string { return textCell(v) }

// Reports whether a held message replays the sidecars the next request would read from a stored row
func (s *Store) replaysAs(row *msgRow, held *Message, sameContent bool) (bool, error) {
	ts, _ := floatCell(row.timestamp)
	serialized, err := s.serializedMessageRow(row.session(), held, ts)
	if err != nil {
		return false, err
	}
	stored, storedIsText := decodeContent(row.content).Text()
	heldText, heldIsText := held.Content.Text()
	rowAPI, rowHasAPI := row.apiContent.(string)
	if rowHasAPI && storedIsText && rowAPI == stored {
		rowHasAPI = false
	}
	heldAPI, heldHasAPI := serialized.apiContent.(string)
	if heldHasAPI && heldIsText && heldAPI == heldText {
		heldHasAPI = false
	}
	for _, col := range replaySidecars {
		if v := serialized.col(col); v != nil && !cellsEqual(v, row.col(col)) {
			return false, nil
		}
	}
	if heldHasAPI || !rowHasAPI {
		return heldHasAPI == rowHasAPI && (!heldHasAPI || heldAPI == rowAPI), nil
	}
	return !sameContent || (heldIsText && heldText == rowAPI), nil
}

// First unclaimed held message naming a row that replays exactly as the row does, false when none does
func (s *Store) claimStoredCopy(row *msgRow, compacted []*Message, byRow map[int64]namedHeld, byUID map[string][]uidHeld, claimed map[int]bool) (int, bool, error) {
	identity, err := storedRowIdentity(row)
	if err != nil {
		return 0, false, err
	}
	var candidates []int
	if named, ok := byRow[row.id]; ok && named.identity == identity {
		candidates = append(candidates, named.index)
	}
	if uid := textCell(row.messageUID); uid != "" {
		for _, held := range byUID[uid] {
			if held.identity == identity {
				candidates = append(candidates, held.index)
			}
		}
	}
	for _, index := range candidates {
		if claimed[index] {
			continue
		}
		ok, err := s.replaysAs(row, compacted[index], true)
		if err != nil {
			return 0, false, err
		}
		if ok {
			return index, true, nil
		}
	}
	return 0, false, nil
}

// Reports whether held differs from the stored row only the way the compressor rewrites a carry it keeps
func (s *Store) rewriteExplains(row *msgRow, held *Message) (bool, error) {
	role := row.roleText()
	if held.Role != role || held.ToolCallID.V != textOrNone(row.toolCallID) {
		return false, nil
	}
	var storedCalls json.RawMessage
	if text, ok := row.toolCalls.(string); ok {
		storedCalls = parseStoredJSON(text)
	}
	if toolCallsCompareKey(parseToolCalls(held.ToolCalls)) != toolCallsCompareKey(storedCalls) {
		return false, nil
	}
	stored := loadedViewContent(role, decodeContent(row.content))
	content := loadedViewContent(role, held.Content)
	replays, err := s.replaysAs(row, held, contentCompareKey(stored) == contentCompareKey(content) && stored.kind == content.kind)
	if err != nil || !replays {
		return false, err
	}
	if imagesReplaced(stored, content) {
		return true, nil
	}
	if held.Snapshot == "" || held.Snapshot != rowSnapshot(row) {
		return false, nil
	}
	storedText := textOf(stored)
	return strings.TrimFunc(storedText, previewSpace) != "" && holdsAsBlock(storedText, textOf(content)), nil
}

// The one unclaimed held message naming a row whose different content the compressor's rewrite explains
func (s *Store) rewrittenCarry(row *msgRow, uid string, compacted []*Message, byRow map[int64]namedHeld, byUID map[string][]uidHeld, claimed map[int]bool) (int, bool, error) {
	var candidates []int
	if named, ok := byRow[row.id]; ok && named.uid == uid {
		candidates = []int{named.index}
	} else {
		for _, held := range byUID[uid] {
			candidates = append(candidates, held.index)
		}
	}
	var free []int
	for _, index := range candidates {
		if !claimed[index] {
			free = append(free, index)
		}
	}
	if len(free) != 1 {
		return 0, false, nil
	}
	ok, err := s.rewriteExplains(row, compacted[free[0]])
	return free[0], ok, err
}

// Identities of tail rows a held message names that an active row outside the tail also carries
func uidsActiveOutside(ctx context.Context, c Conn, rows []*msgRow, tailIDs []int64, named map[string]bool) (map[string]bool, error) {
	var wanted []string
	for _, r := range rows {
		if uid := textCell(r.messageUID); uid != "" && named[uid] && !containsString(wanted, uid) {
			wanted = append(wanted, uid)
		}
	}
	out := map[string]bool{}
	if len(wanted) == 0 {
		return out, nil
	}
	sort.Strings(wanted)
	sessionID := rows[0].session()
	for _, chunk := range chunks(wanted, sqlInChunk) {
		found, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, message_uid FROM messages WHERE session_id = ? AND active = 1 "+
			"AND message_uid IN ("+bindMarks(len(chunk))+")", append([]any{sessionID}, stringArgs(chunk)...)...))
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			if !slices.Contains(tailIDs, r.id) {
				out[textCell(r.messageUID)] = true
			}
		}
	}
	return out, nil
}

// Tail rows the compacted set already carries, each with the index of its carrying message, and the rows a rewritten carry claimed
func (s *Store) carriedTailIDs(ctx context.Context, c Conn, tailIDs []int64, compacted []*Message) (map[int64]int, map[int64]bool, error) {
	carried, weak := map[int64]int{}, map[int64]bool{}
	if len(tailIDs) == 0 || len(compacted) == 0 {
		return carried, weak, nil
	}
	byRow, byUID, err := indexHeldMessages(compacted)
	if err != nil {
		return nil, nil, err
	}
	rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT * FROM messages WHERE id IN ("+bindMarks(len(tailIDs))+") ORDER BY id", int64Args(tailIDs)...))
	if err != nil {
		return nil, nil, err
	}
	claimed := map[int]bool{}
	for _, row := range rows {
		index, ok, err := s.claimStoredCopy(row, compacted, byRow, byUID, claimed)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			carried[row.id] = index
			claimed[index] = true
		}
	}
	named := map[string]bool{}
	for uid := range byUID {
		named[uid] = true
	}
	for _, n := range byRow {
		if n.uid != "" {
			named[n.uid] = true
		}
	}
	elsewhere, err := uidsActiveOutside(ctx, c, rows, tailIDs, named)
	if err != nil {
		return nil, nil, err
	}
	tailUIDs := map[string]int{}
	for _, row := range rows {
		if uid := textCell(row.messageUID); uid != "" {
			tailUIDs[uid]++
		}
	}
	for _, row := range rows {
		uid := textCell(row.messageUID)
		if _, done := carried[row.id]; done || uid == "" || tailUIDs[uid] != 1 || elsewhere[uid] {
			continue
		}
		index, ok, err := s.rewrittenCarry(row, uid, compacted, byRow, byUID, claimed)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			carried[row.id] = index
			claimed[index] = true
			weak[row.id] = true
		}
	}
	return carried, weak, nil
}

// How many of the carried tail's messages name a row in carried
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

// Resolves carried forward live messages to their active durable originals, by row id or a unique identity and timestamp
func (s *Store) resolveCarriedRowIDs(ctx context.Context, c Conn, sessionID string, carriedMessages []*Message) ([]int64, error) {
	if len(carriedMessages) == 0 {
		return nil, nil
	}
	type carriedRef struct {
		identity string
		rowID    int64
		ts       float64
		hasTS    bool
	}
	var carried []carriedRef
	named := 0
	for _, m := range carriedMessages {
		if m == nil {
			continue
		}
		identity, err := liveRowIdentity(m)
		if err != nil {
			return nil, err
		}
		ref := carriedRef{identity: identity, rowID: max(m.RowID, 0)}
		ref.ts, ref.hasTS = trustedMessageTimestamp(m.Timestamp)
		if ref.rowID > 0 {
			named++
		}
		carried = append(carried, ref)
	}
	byID := map[int64]string{}
	byKey := map[string][]int64{}
	index := func(ids []int64) error {
		clear(byID)
		clear(byKey)
		narrow := ""
		if len(ids) > 0 {
			narrow = " AND id IN (" + bindMarks(len(ids)) + ")"
		}
		rows, err := queryMsgRows(c.QueryContext(ctx, "SELECT id, role, content, tool_call_id, tool_calls, timestamp FROM messages "+
			"WHERE session_id = ? AND active = 1"+narrow+" ORDER BY id", append([]any{sessionID}, int64Args(ids)...)...))
		if err != nil {
			return err
		}
		for _, r := range rows {
			identity, err := storedRowIdentity(r)
			if err != nil {
				return err
			}
			byID[r.id] = identity
			if ts, ok := cellEpoch(r.timestamp, "message timestamp"); ok {
				key := identity + "\x00" + shortestFloatText(ts)
				byKey[key] = append(byKey[key], r.id)
			}
		}
		return nil
	}
	var rowIDs []int64
	for _, ref := range carried {
		if ref.rowID > 0 {
			rowIDs = append(rowIDs, ref.rowID)
		}
	}
	allNamed := named == len(carried)
	if allNamed {
		if err := index(rowIDs); err != nil {
			return nil, err
		}
		for _, ref := range carried {
			if byID[ref.rowID] != ref.identity {
				allNamed = false
				break
			}
		}
	}
	if !allNamed {
		if err := index(nil); err != nil {
			return nil, err
		}
	}
	var resolved []int64
	for _, ref := range carried {
		if identity, ok := byID[ref.rowID]; ref.rowID > 0 && ok && identity == ref.identity {
			if !slices.Contains(resolved, ref.rowID) {
				resolved = append(resolved, ref.rowID)
			}
			continue
		}
		if !ref.hasTS {
			continue
		}
		if matches := byKey[ref.identity+"\x00"+shortestFloatText(ref.ts)]; len(matches) == 1 && !slices.Contains(resolved, matches[0]) {
			resolved = append(resolved, matches[0])
		}
	}
	return resolved, nil
}

// Active row ids whose stored role and content equal a text message, none when it was never persisted
func matchingActiveIDs(ctx context.Context, c Conn, sessionID string, m *Message) ([]int64, error) {
	text, ok := m.Content.Text()
	if !ok {
		return nil, nil
	}
	stored, err := loadedViewContent(m.role(), TextContent(text)).encode()
	if err != nil {
		return nil, err
	}
	return queryInt64s(ctx, c, "SELECT id FROM messages WHERE session_id = ? AND active = 1 AND role = ? AND content = ?",
		sessionID, emptyNull(m.Role), stored)
}

// Call ids the tool calls of a stored row carry, a call without one reading as empty
func storedCallIDs(cell any) []string {
	text, ok := cell.(string)
	if !ok {
		return nil
	}
	elements, ok := arrayElements(parseStoredJSON(text))
	if !ok {
		return nil
	}
	var ids []string
	for _, e := range elements {
		members, isObject := objectMembers(e)
		if !isObject {
			continue
		}
		v, _ := memberValue(members, "id")
		ids = append(ids, jsonString(v))
	}
	return ids
}

// Active rows equal to a row an alternation repair retired without an id, at or below the watermark when given
func matchingRetiredIDs(ctx context.Context, c Conn, sessionID string, retired *RetiredRow, watermark *int64) ([]int64, error) {
	query := "SELECT id, content, tool_call_id, tool_calls FROM messages WHERE session_id = ? AND active = 1 AND role = ?"
	args := []any{sessionID, retired.Role}
	if watermark != nil {
		query += " AND id <= ?"
		args = append(args, *watermark)
	}
	rows, err := queryMsgRows(c.QueryContext(ctx, query+" ORDER BY id", args...))
	if err != nil {
		return nil, err
	}
	want := falsyCollapsedKey(retired.Content)
	var matches []int64
	for _, r := range rows {
		if falsyCollapsedKey(loadedViewContent(retired.Role, decodeContent(r.content))) != want {
			continue
		}
		if textCell(r.toolCallID) != retired.ToolCallID {
			continue
		}
		if !slices.Equal(storedCallIDs(r.toolCalls), retired.ToolCallIDs) {
			continue
		}
		matches = append(matches, r.id)
	}
	return matches, nil
}

// Active rows an alternation repair merged into a user message, empty for none and false when the run is ambiguous
func (s *Store) mergedUserRun(ctx context.Context, c Conn, sessionID string, m *Message, watermark *int64) ([]int64, bool, error) {
	content, isText := m.Content.Text()
	if m.Role != "user" || !isText || m.MergedDurableRows < 2 {
		return nil, true, nil
	}
	width := m.MergedDurableRows
	content = stripLeadingMessageTimestamps(content)
	query := "SELECT id, role, content FROM messages WHERE session_id = ? AND active = 1"
	args := []any{sessionID}
	if watermark != nil {
		query += " AND id <= ?"
		args = append(args, *watermark)
	}
	all, err := queryMsgRows(c.QueryContext(ctx, query+" ORDER BY id", args...))
	if err != nil {
		return nil, false, err
	}
	retiredIDs := map[int64]bool{}
	for i := range m.RetiredDurableRows {
		retired := &m.RetiredDurableRows[i]
		if retired.Own {
			continue
		}
		matches, err := matchingRetiredIDs(ctx, c, sessionID, retired, watermark)
		if err != nil {
			return nil, false, err
		}
		if len(matches) != 1 {
			return nil, false, nil
		}
		retiredIDs[matches[0]] = true
	}
	type part struct {
		id     int64
		role   string
		text   string
		isText bool
	}
	var rows []part
	for _, r := range all {
		if retiredIDs[r.id] {
			continue
		}
		text, ok := loadedViewContent(r.roleText(), decodeContent(r.content)).Text()
		rows = append(rows, part{r.id, r.roleText(), text, ok})
	}
	var runs [][]int64
	for start := range rows {
		merged := ""
		for end := start; end < len(rows); end++ {
			p := rows[end]
			if p.role != "user" || !p.isText {
				break
			}
			switch {
			case merged != "" && p.text != "":
				merged = merged + "\n\n" + p.text
			case merged == "":
				merged = p.text
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
	return nil, true, nil
}

// Ids safe to archive as summarized and ids merged into another held message, false when a durable held row cannot be named
func (s *Store) provedCoverage(ctx context.Context, c Conn, sessionID string, coveredIDs []int64, unresolved []Held, watermark *int64) ([]int64, map[int64]bool, bool, error) {
	if coveredIDs == nil {
		return nil, nil, false, nil
	}
	var proved []int64
	add := func(ids ...int64) {
		for _, id := range ids {
			if !slices.Contains(proved, id) {
				proved = append(proved, id)
			}
		}
	}
	for _, id := range coveredIDs {
		if id > 0 {
			add(id)
		}
	}
	mergedAway := map[int64]bool{}
	for _, h := range unresolved {
		if h.Retired != nil {
			matches, err := matchingRetiredIDs(ctx, c, sessionID, h.Retired, watermark)
			if err != nil {
				return nil, nil, false, err
			}
			if len(matches) != 1 {
				return nil, nil, false, nil
			}
			add(matches...)
			if !h.Retired.Own {
				mergedAway[matches[0]] = true
			}
			continue
		}
		m := h.Message
		if m == nil {
			continue
		}
		if slices.ContainsFunc(m.RetiredDurableRows, func(r RetiredRow) bool { return r.Own }) {
			continue
		}
		run, ok, err := s.mergedUserRun(ctx, c, sessionID, m, watermark)
		if err != nil {
			return nil, nil, false, err
		}
		if !ok || (m.MergedDurableRows != 0 && len(run) == 0) {
			return nil, nil, false, nil
		}
		var matches []int64
		if len(run) == 0 {
			if matches, err = matchingActiveIDs(ctx, c, sessionID, m); err != nil {
				return nil, nil, false, err
			}
		}
		if len(matches) > 1 || (m.Persisted && len(matches) != 1 && len(run) == 0) {
			return nil, nil, false, nil
		}
		if len(matches) > 0 {
			add(matches...)
		} else {
			add(run...)
		}
		if len(run) > 1 {
			for _, id := range run[1:] {
				if !slices.Contains(m.AbsorbedRowIDs, id) {
					mergedAway[id] = true
				}
			}
		}
	}
	return proved, mergedAway, true, nil
}

// Durable rows behind the carried tail beyond one per message, for the positional rewind
func uncountedMergedRows(tail []*Message) int {
	total := 0
	for _, m := range tail {
		if m == nil || m.RowID != 0 {
			continue
		}
		unlisted := 0
		if m.MergedDurableRows != 0 {
			unlisted = max(0, m.MergedDurableRows-1-len(m.AbsorbedRowIDs))
		}
		total += unlisted + m.UnnamedDurableRows
	}
	return total
}

// Newest covered rows behind the carried tail, a message that merged rows standing for each of them
func tailOriginals(coveredActive []int64, tailCount int, mergedAway map[int64]bool) []int64 {
	width := tailCount
	for {
		window := coveredActive[max(len(coveredActive)-width, 0):]
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
