package tools

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	filesconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/internal/tools/document"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Pagination defaults of read_file and search_files
const (
	readLimit   = 2000
	searchLimit = 50
)

// Past this size a wide read gets a hint toward reading what it needs
const largeFile = 512_000

// Bytes in a row of the hex view of a binary file
const hexRow = 16

var (
	conflictOpen  = regexp.MustCompile(`(?m)^` + space + `*[0-9]+\|<<<<<<< `)
	conflictClose = regexp.MustCompile(`(?m)^` + space + `*[0-9]+\|>>>>>>> `)
)

// Number an argument gives for paging, the default when it gives none or one that is not a number
func paging(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fallback
		}
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return fallback
}

// Merge conflict blocks of a numbered page, those with both their markers
func conflicts(page string) int {
	opens := len(conflictOpen.FindAllStringIndex(page, -1))
	if opens == 0 {
		return 0
	}
	return min(opens, len(conflictClose.FindAllStringIndex(page, -1)))
}

// Page cut to its last whole line within the budget, its first line cut short when even that does not fit, with the lines kept
func cutToBudget(content string, budget int) (string, int, bool) {
	if runes(content) <= budget {
		if content == "" {
			return content, 0, false
		}
		return content, strings.Count(content, "\n") + 1, false
	}
	lines := strings.Split(content, "\n")
	var kept []string
	used := 0
	for _, line := range lines {
		add := runes(line)
		if len(kept) > 0 {
			add++
		}
		if used+add > budget {
			break
		}
		kept = append(kept, line)
		used += add
	}
	if len(kept) == 0 {
		kept = []string{strs.Head(lines[0], budget)}
	}
	return strings.Join(kept, "\n"), len(kept), true
}

// Most characters one read returns
func (o *ops) budget() int { return max(o.cfg.GetInt(filesconfig.ReadMaxChars), 1) }

// Hint of a read cut at the character budget, and whether its first line alone was cut short
func budgetHint(budget, kept, offset int, total string, firstLine string) (string, bool) {
	next := offset + kept
	hint := fmt.Sprintf("Output truncated at the %s-char read budget after %d line(s) (showing lines %d-%d of %s). Use offset=%d to continue.",
		strs.Thousands(budget), kept, offset, next-1, total, next)
	if runes(firstLine) >= budget {
		hint += " Note: the first line alone exceeded the budget and was clamped mid-line; its remainder is not retrievable via offset."
		return hint, true
	}
	return hint, false
}

// Text of a file the model reads with its secrets masked when hiding secrets is on, always for icebath's own config files
func (o *ops) redact(text, resolved string) string {
	if text == "" {
		return text
	}
	secret, forced := o.secretFile(resolved)
	if !forced && !o.cfg.GetBool(toolsconfig.RedactSecrets) {
		return text
	}
	return redact.File(text, secret)
}

// Hex view of a binary file in rows of 16 bytes with their printable characters, offset and limit counting rows
func (o *ops) hexView(resolved string, offset, limit int) string {
	r := o.readRange(resolved, int64(offset-1)*hexRow, limit*hexRow)
	if r.Error != "" {
		r.Error = tools.BoundError(r.Error)
		return result(r)
	}
	var b strings.Builder
	base := int64(offset-1) * hexRow
	for i := 0; i < len(r.data); i += hexRow {
		row := r.data[i:min(i+hexRow, len(r.data))]
		fmt.Fprintf(&b, "%08x: ", base+int64(i))
		for j := range hexRow {
			if j < len(row) {
				fmt.Fprintf(&b, "%02x", row[j])
			} else {
				b.WriteString("  ")
			}
			if j%2 == 1 {
				b.WriteByte(' ')
			}
		}
		b.WriteByte(' ')
		for _, c := range row {
			if c >= 0x20 && c < 0x7f {
				b.WriteByte(c)
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteByte('\n')
	}
	content := strings.TrimRight(b.String(), "\n")
	rows := int((r.FileSize + hexRow - 1) / hexRow)
	end := offset + limit - 1
	view := struct {
		Content        string `json:"content"`
		TotalLines     int    `json:"total_lines"`
		FileSize       int64  `json:"file_size"`
		Truncated      bool   `json:"truncated"`
		HexView        bool   `json:"hex_view"`
		BinaryType     string `json:"binary_type,omitempty"`
		Hint           string `json:"hint,omitempty"`
		TruncatedBy    string `json:"truncated_by,omitempty"`
		NextOffset     int    `json:"next_offset,omitempty"`
		TruncatedLines bool   `json:"truncated_lines,omitempty"`
	}{Content: content, TotalLines: rows, FileSize: r.FileSize, Truncated: rows > end, HexView: true}
	if offset == 1 {
		view.BinaryType = tools.Identify(r.data)
	}
	if rows > end {
		view.Hint = fmt.Sprintf("Use offset=%d to continue reading (showing rows %d-%d of %d, %d bytes per row)", end+1, offset, end, rows, hexRow)
	}
	if budget := o.budget(); runes(content) > budget {
		trimmed, kept, _ := cutToBudget(content, budget)
		line, _, _ := strings.Cut(trimmed, "\n")
		view.Content, view.Truncated, view.TruncatedBy, view.NextOffset = trimmed, true, "bytes", offset+kept
		view.Hint, view.TruncatedLines = budgetHint(budget, kept, offset, strconv.Itoa(rows), line)
	}
	view.Content = o.redact(view.Content, resolved)
	return result(view)
}

// Document read as paginated text: by its extension, or by its leading bytes when byContent; handled false hands the read back to the
// plain read
func (o *ops) document(raw, resolved string, offset, limit int, byContent bool) (string, bool) {
	bin := o.readBytes(resolved, document.MaxBytes)
	if byContent && (bin.Error != "" || !bin.IsBinary) {
		return "", false
	}
	var text string
	var broke *document.Error
	if bin.Error != "" || !bin.IsBinary {
		message := bin.Error
		if message == "" {
			message = "Document bytes unavailable"
		}
		broke = &document.Error{Message: message}
	} else {
		var err error
		text, err = o.r.documents.Extract(o.ctx, bin.data, resolved, o.r.web.Parse(o.cfg))
		switch {
		case errors.Is(err, document.ErrNotDocument):
			return "", false
		case err != nil:
			var ok bool
			if broke, ok = errors.AsType[*document.Error](err); !ok {
				return failure(err.Error()), true
			}
		}
	}
	if broke != nil {
		extension := ext(resolved)
		if extension == "" {
			extension = "no extension"
		}
		if byContent || document.Is(nil, resolved) && ext(resolved) != ".ipynb" {
			return failure(fmt.Sprintf("Cannot read '%s' (%s): document extraction failed — %s. Use terminal utilities to inspect or convert the file.",
				raw, extension, broke.Message)), true
		}
		return "", false
	}
	lines := diff.Split(text)
	total, end := len(lines), offset+limit-1
	lo := min(max(offset-1, 0), total)
	hi := max(min(end, total), lo)
	page := strings.Join(lines[lo:hi], "\n")
	content := ""
	if page != "" {
		content = o.number(page, offset)
	}
	out := struct {
		Content           string `json:"content"`
		TotalLines        int    `json:"total_lines"`
		FileSize          int64  `json:"file_size"`
		Truncated         bool   `json:"truncated"`
		ExtractedDocument bool   `json:"extracted_document"`
		Hint              string `json:"hint,omitempty"`
		TruncatedBy       string `json:"truncated_by,omitempty"`
		NextOffset        int    `json:"next_offset,omitempty"`
		TruncatedLines    bool   `json:"truncated_lines,omitempty"`
	}{Content: content, TotalLines: total, FileSize: bin.FileSize, Truncated: total > end, ExtractedDocument: true}
	if total > end {
		out.Hint = fmt.Sprintf("Use offset=%d to continue reading (showing %d-%d of %d lines)", end+1, offset, min(end, total), total)
	}
	if budget := o.budget(); runes(content) > budget {
		trimmed, kept, _ := cutToBudget(content, budget)
		line, _, _ := strings.Cut(trimmed, "\n")
		out.Content, out.Truncated, out.TruncatedBy, out.NextOffset = trimmed, true, "bytes", offset+kept
		out.Hint, out.TruncatedLines = budgetHint(budget, kept, offset, strconv.Itoa(total), line)
	}
	redacted := false
	if out.Content != "" {
		shown := o.redact(out.Content, resolved)
		redacted, out.Content = shown != out.Content, shown
	}
	if offset == 1 && !out.Truncated && !redacted && !o.longLines(page) && !out.TruncatedLines && o.host {
		o.r.memory.markBaseline(o.track, resolved, "")
		o.r.memory.stamp(o.track, resolved)
		if !o.unguarded() {
			o.r.memory.recordRead(o.track, resolved, false)
		}
	}
	return result(out), true
}

// Answer to a read of a region read before and unchanged since: the unchanged stub, and after two of them a refusal
func (o *ops) stub(key dedupKey, raw string) string {
	m := o.r.memory
	m.mu.Lock()
	hits, _ := o.track.hits.get(key)
	hits++
	o.track.hits.set(key, hits)
	o.track.trim()
	m.mu.Unlock()
	if hits >= 2 {
		return failure(fmt.Sprintf("BLOCKED: You have called read_file on this exact region %d times and the file has NOT changed. STOP calling "+
			"read_file for this path — the content from your earlier read_file result in this conversation is still current. Proceed with your task "+
			"using the information you already have.", hits+1), "path", raw, "already_read", hits+1, guardrail, true)
	}
	return result(struct {
		Status          string `json:"status"`
		Message         string `json:"message"`
		Path            string `json:"path"`
		Dedup           bool   `json:"dedup"`
		ContentReturned bool   `json:"content_returned"`
	}{"unchanged", unchangedMessage, raw, true, false})
}

// Bookkeeping of a read that returned content: what may be stubbed, what the session holds whole, and how many times in a row the same
// region was read
func (o *ops) noteRead(raw, resolved string, offset, limit int, key dedupKey, partial, redacted bool, end, total int, before fileMeta, hasBefore bool,
	snapshot *fileVersion) int {
	m := o.r.memory
	var version fileVersion
	hasVersion := false
	if hasBefore {
		if snapshot != nil {
			version, hasVersion = *snapshot, true
		} else {
			version, hasVersion = readVersion(resolved)
		}
	}
	stable := false
	if hasVersion && version.meta == before {
		current, ok := metadata(resolved)
		stable = ok && current == before
	}
	complete := false
	m.mu.Lock()
	t := o.track
	t.hits.del(key)
	t.served.set(key, struct{}{})
	count := t.repeat(fmt.Sprintf("read\x00%s\x00%d\x00%d", raw, offset, limit))
	if at, ok := mtime(resolved); ok {
		t.stamps.set(resolved, at)
	}
	if stable && count < repeatsBlock {
		t.dedup.set(key, before)
		baseline, has := t.baselines.get(resolved)
		complete = has && baseline == version
		if !complete {
			complete = !partial
			if partial {
				complete, redacted = t.cover(resolved, version, offset, end, total, redacted)
			}
			complete = complete && !redacted
		}
		if complete {
			t.baselines.set(resolved, version)
			t.blind.del(resolved)
		}
	}
	if !complete {
		t.baselines.del(resolved)
	}
	if !stable || count >= repeatsBlock {
		t.dedup.del(key)
		t.served.del(key)
	}
	t.trim()
	m.mu.Unlock()
	if !o.unguarded() {
		m.recordRead(t, resolved, !complete)
	}
	if complete {
		skills.MarkRead(o.ctx, resolved)
	}
	return count
}

// Reads a page of a file behind the device, namespace, credential and document guards, with the unchanged stub for a region read before
func (o *ops) read(raw string, offset, limit int) string {
	if o.device(raw) {
		return failure(fmt.Sprintf("Cannot read '%s': this is a device file that would block or produce infinite output.", raw))
	}
	resolved := o.resolve(raw)
	if o.host {
		if kind := specialKind(resolved); kind != "" {
			return asciiJSON(map[string]any{"success": false, "note": fmt.Sprintf("'%s' is %s, not a regular file — reading it would block "+
				"indefinitely, so no read was attempted. Use terminal utilities if you need to interact with it.", raw, kind)})
		}
	}
	if block := o.readBlock(raw, resolved); block != "" {
		return failure(block)
	}
	if document.Is(nil, resolved) {
		if out, handled := o.document(raw, resolved, offset, limit, false); handled {
			return out
		}
	}
	if binaryExtension(resolved) {
		if probe := o.readFile(resolved, 1, 1); probe.document {
			if out, handled := o.document(raw, resolved, offset, limit, true); handled {
				return out
			}
		}
		if image(resolved) {
			return failure(fmt.Sprintf("'%s' is an image (%s), view it with vision_analyze.", raw, ext(resolved)))
		}
		return o.hexView(resolved, offset, limit)
	}
	m := o.r.memory
	if cached, ok := m.cachedMissing(o.track, "read", resolved); ok {
		return cached
	}
	key := dedupKey{resolved, offset, limit}
	m.mu.Lock()
	cached, hasCached := o.track.dedup.get(key)
	served := o.track.served.has(key)
	m.mu.Unlock()
	var before fileMeta
	hasBefore := false
	if o.host {
		before, hasBefore = metadata(resolved)
	}
	if hasCached && hasBefore && before == cached && served {
		m.mu.Lock()
		baseline, whole := o.track.baselines.get(resolved)
		m.mu.Unlock()
		if whole && baseline.meta == before {
			skills.MarkRead(o.ctx, resolved)
		}
		return o.stub(key, raw)
	}
	r := o.readFile(resolved, offset, limit)
	if r.document {
		if out, handled := o.document(raw, resolved, offset, limit, true); handled {
			return out
		}
	}
	if r.IsBinary && !r.IsImage {
		return o.hexView(resolved, offset, limit)
	}
	if r.NotFound {
		if hint := tools.Expired(raw); hint != "" {
			r.Expired, r.Hint = true, hint
		}
		m.rememberMissing(o.track, "read", resolved, result(r))
	}
	if r.Error != "" || r.IsBinary {
		r.Error = tools.BoundError(r.Error)
		return result(r)
	}
	content := r.Content
	if budget := o.budget(); runes(content) > budget {
		total := strconv.Itoa(r.TotalLines)
		trimmed, kept, _ := cutToBudget(content, budget)
		line, _, _ := strings.Cut(trimmed, "\n")
		content, r.Truncated, r.TruncatedBy, r.NextOffset = trimmed, true, "bytes", offset+kept
		var clamped bool
		r.Hint, clamped = budgetHint(budget, kept, offset, total, line)
		r.TruncatedLines = r.TruncatedLines || clamped
	}
	redacted := false
	if content != "" {
		shown := o.redact(content, resolved)
		redacted, content = shown != content, shown
	}
	r.Content = content
	if n := conflicts(content); n > 0 {
		r.ConflictBlocks = n
		r.Note = fmt.Sprintf("%d unresolved git merge-conflict block(s) (<<<<<<< / ======= / >>>>>>>) in this range. Resolve them (keep one side or "+
			"combine, delete the markers) before editing around them.", n)
	}
	if r.FileSize > largeFile && limit > 200 && r.Truncated && r.Note == "" {
		r.Note = fmt.Sprintf("This file is large (%s bytes). Consider reading only the section you need with offset and limit to keep context usage "+
			"efficient.", strs.Thousands(int(r.FileSize)))
	}
	end := offset + limit - 1
	switch {
	case r.TruncatedBy == "bytes":
		end = r.NextOffset - 1
	case r.TotalLines > 0:
		end = min(end, r.TotalLines)
	}
	count := o.noteRead(raw, resolved, offset, limit, key, offset > 1 || r.Truncated, redacted || r.TruncatedLines, end, r.TotalLines, before,
		hasBefore, r.version)
	if count >= repeatsBlock {
		return failure(fmt.Sprintf("BLOCKED: You have read this exact file region %d times in a row. The content has NOT changed. You already have "+
			"this information. STOP re-reading and proceed with your task.", count), "path", raw, "already_read", count, guardrail, true)
	}
	if count >= repeatsWarn {
		r.Warning = fmt.Sprintf("You have read this exact file region %d times consecutively. The content has not changed since your last read. "+
			"Use the information you already have. If you are stuck in a loop, stop reading and proceed with writing or responding.", count)
	}
	return result(r)
}
