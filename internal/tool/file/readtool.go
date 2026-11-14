package file

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/docconv"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	defaultMaxReadChars = 100_000
	largeFileHintBytes  = 512_000
	symlinkHopLimit     = 20
	keySeparator        = "\x00"
)

var blockedProcSuffixes = [...]string{
	"/fd/0", "/fd/1", "/fd/2",
	"/environ", "/cmdline", "/maps", "/smaps", "/smaps_rollup", "/numa_maps",
	"/mem", "/auxv", "/pagemap",
}

func isBlockedDeviceName(normalized string) bool {
	switch normalized {
	case "/dev/zero", "/dev/random", "/dev/urandom", "/dev/full",
		"/dev/stdin", "/dev/tty", "/dev/console",
		"/dev/stdout", "/dev/stderr",
		"/dev/fd/0", "/dev/fd/1", "/dev/fd/2":
		return true
	}
	if !strings.HasPrefix(normalized, "/proc/") {
		return false
	}
	for _, suffix := range blockedProcSuffixes {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func isExtractableSuffix(ext string) bool {
	switch ext {
	case ".ipynb", ".docx", ".xlsx", ".db", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

func isConverterSuffix(ext string) bool {
	switch ext {
	case ".doc", ".docm", ".ppt", ".pps", ".pot", ".pptx", ".pptm", ".ppsx", ".ppsm",
		".xls", ".xlsm", ".xlsb", ".odt", ".ods", ".odp", ".rtf", ".epub", ".pdf":
		return true
	}
	return false
}

func nameSuffix(path string) string {
	name := path
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 || dot == len(name)-1 {
		return ""
	}
	return name[dot:]
}

func (t *Tools) isBlockedDevicePath(ctx context.Context, path string) bool {
	return isBlockedDeviceName(hostNorm(t.paths.ExpandTilde(ctx, path)))
}

func (t *Tools) isBlockedDevice(ctx context.Context, path, base string, anchored bool) bool {
	expanded := t.paths.ExpandTilde(ctx, path)
	if anchored && !filepath.IsAbs(expanded) {
		expanded = hostStyle().join(base, expanded)
	}
	normalized := hostNorm(expanded)
	if t.isBlockedDevicePath(ctx, normalized) {
		return true
	}
	seen := make(map[string]struct{})
	current := normalized
	for hop := 0; hop < symlinkHopLimit; hop++ {
		target, err := os.Readlink(current)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = hostStyle().join(hostStyle().dir(current), target)
		}
		target = hostNorm(target)
		if t.isBlockedDevicePath(ctx, target) {
			return true
		}
		if _, looped := seen[target]; looped {
			break
		}
		seen[target] = struct{}{}
		current = target
	}
	return t.isBlockedDevicePath(ctx, hostNorm(realPath(normalized)))
}

func specialFileKind(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mode := fi.Mode()
	switch {
	case mode.IsRegular() || mode.IsDir():
		return ""
	case mode&os.ModeNamedPipe != 0:
		return "a FIFO (named pipe)"
	case mode&os.ModeSocket != 0:
		return "a socket"
	case mode&os.ModeCharDevice != 0:
		return "a character device"
	case mode&os.ModeDevice != 0:
		return "a block device"
	}
	return "a special (non-regular) file"
}

func (t *Tools) maxReadChars(ctx context.Context) int {
	cfg, err := t.config.LoadReadonly(ctx)
	if err != nil || cfg.FileReadMaxChars <= 0 {
		return defaultMaxReadChars
	}
	return cfg.FileReadMaxChars
}

func (t *Tools) limits(ctx context.Context) tool.OutputLimits {
	cfg, err := t.config.LoadReadonly(ctx)
	if err != nil {
		return tool.Limits(nil).Resolved()
	}
	return tool.Limits(cfg).Resolved()
}

func truncateToCharBudget(content string, maxChars int) (string, int) {
	if charLen(content) <= maxChars {
		if content == "" {
			return content, 0
		}
		return content, strings.Count(content, "\n") + 1
	}
	lines := strings.Split(content, "\n")
	var kept []string
	running := 0
	for _, line := range lines {
		addition := charLen(line)
		if len(kept) > 0 {
			addition++
		}
		if running+addition > maxChars {
			break
		}
		kept = append(kept, line)
		running += addition
	}
	if len(kept) == 0 {
		kept = append(kept, charPrefix(lines[0], maxChars))
	}
	return strings.Join(kept, "\n"), len(kept)
}

func applyCharBudget(fields *fieldList, content string, offset, totalLines, maxChars int) (string, int) {
	trimmed, linesKept := truncateToCharBudget(content, maxChars)
	nextOffset := offset + linesKept
	fields.set("content", trimmed)
	fields.set("truncated", true)
	fields.set("truncated_by", "bytes")
	fields.set("next_offset", nextOffset)
	hint := fmt.Sprintf("Output truncated at the %s-char read budget after "+
		"%d line(s) (showing lines %d-%d of "+
		"%d). Use offset=%d to continue.", commas(int64(maxChars)), linesKept, offset, nextOffset-1, totalLines, nextOffset)
	fields.set("hint", hint)
	firstLine, _, _ := strings.Cut(trimmed, "\n")
	if charLen(firstLine) >= maxChars {
		fields.set("truncated_lines", true)
		fields.set("hint", hint+" Note: the first line alone exceeded the budget and was "+
			"clamped mid-line; its remainder is not retrievable via offset.")
	}
	return trimmed, nextOffset
}

func (t *Tools) redactRead(ctx context.Context, text, resolved string) string {
	return t.redactor.Text(ctx, text, redact.Mode{FileRead: true, SecretFile: t.redactor.IsSecretFile(ctx, resolved)})
}

func (t *Tools) readExtractedDocument(ctx context.Context, ops *Operations, path, resolved string, offset, limit int, taskID string) (tool.Result, bool, error) {
	if !t.documents.Extractable(ctx, resolved) {
		return tool.Result{}, false, nil
	}
	binary, err := ops.ReadBytes(ctx, resolved, docconv.MaxDocumentBytes)
	if err != nil {
		return tool.Result{}, false, err
	}
	extracted := ""
	failure, transport := "", false
	switch {
	case binary.Error != "":
		failure = binary.Error
	case binary.Base64Content == nil:
		failure = "Document bytes unavailable"
	default:
		data, decodeErr := base64.StdEncoding.DecodeString(*binary.Base64Content)
		if decodeErr != nil {
			failure, transport = decodeErr.Error(), true
			break
		}
		text, extractErr := t.documents.ExtractBytes(ctx, data, resolved)
		var refused *docconv.ExtractError
		switch {
		case extractErr == nil:
			extracted = text
		case errors.As(extractErr, &refused):
			failure = refused.Error()
		default:
			return tool.Result{}, false, extractErr
		}
	}
	if failure != "" {
		slog.DebugContext(ctx, "document extraction failed", "path", path, "reason", failure)
		ext := strings.ToLower(nameSuffix(resolved))
		binaryDocument := isConverterSuffix(ext) || (isExtractableSuffix(ext) && ext != ".ipynb")
		if binaryDocument && !transport && !strings.HasPrefix(failure, "Unsupported document type") {
			return tool.Failure("Cannot read '" + path + "' (" + ext + "): document " +
				"extraction failed — " + validText(failure) + ". Use terminal utilities " +
				"to inspect or convert the file."), true, nil
		}
		return tool.Result{}, false, nil
	}

	lines := splitLines(extracted, false)
	totalLines := len(lines)
	endLine := offset + limit - 1
	var page []string
	if offset-1 < totalLines {
		page = lines[offset-1 : min(endLine, totalLines)]
	}
	pageText := strings.Join(page, "\n")
	maxLine := t.limits(ctx).MaxLineLength
	truncatedLines := anyLineLonger(pageText, maxLine)
	content := ""
	if pageText != "" {
		content = addLineNumbers(pageText, offset, maxLine)
	}
	truncated := totalLines > endLine
	fields := fieldList{
		{Key: "content", Value: content},
		{Key: "total_lines", Value: totalLines},
		{Key: "file_size", Value: binary.FileSize},
		{Key: "truncated", Value: truncated},
		{Key: "extracted_document", Value: true},
	}
	if truncated {
		fields.set("hint", fmt.Sprintf("Use offset=%d to continue reading "+
			"(showing %d-%d of %d lines)", endLine+1, offset, min(endLine, totalLines), totalLines))
	}
	if maxChars := t.maxReadChars(ctx); charLen(content) > maxChars {
		content, _ = applyCharBudget(&fields, content, offset, totalLines, maxChars)
		truncated = true
	}
	redacted := false
	if content != "" {
		rendered := content
		content = t.redactRead(ctx, rendered, resolved)
		redacted = content != rendered
		fields.set("content", content)
	}
	if offset == 1 && !truncated && !redacted && !truncatedLines && !fields.has("truncated_lines") {
		t.tasks.tracker.MarkFullWriteBaseline(resolved, taskID, "")
		t.tasks.tracker.UpdateReadTimestamp(resolved, taskID)
		t.tasks.state.RecordRead(taskID, resolved, false)
	}
	return encode(fields), true, nil
}

func (t *Tools) dedupStubOrBlock(taskID string, key ReadKey, path string) tool.Result {
	hits := 0
	t.tasks.tracker.Update(taskID, func(reads *TaskReads) {
		hits = reads.DedupHit(key)
	})
	if hits >= 2 {
		return tool.Failure(
			"BLOCKED: You have called read_file on this "+
				"exact region "+strconv.Itoa(hits+1)+" times and the file "+
				"has NOT changed. STOP calling read_file for "+
				"this path — the content from your earlier "+
				"read_file result in this conversation is "+
				"still current. Proceed with your task using "+
				"the information you already have.",
			tool.Field{Key: "path", Value: validText(path)},
			tool.Field{Key: "already_read", Value: hits + 1},
			tool.Field{Key: tool.GuardrailRefusalKey, Value: true})
	}
	return encode(fieldList{
		{Key: "status", Value: "unchanged"},
		{Key: "message", Value: readDedupStatusMessage},
		{Key: "path", Value: path},
		{Key: "dedup", Value: true},
		{Key: "content_returned", Value: false},
	})
}

type readOutcome struct {
	path, resolved string
	offset, limit  int
	key            ReadKey
	partial        bool
	redacted       bool
	endLine        int
	totalLines     int
	before         Version
	haveBefore     bool
	snapshot       *Version
}

func readLoopKey(path string, offset, limit int) string {
	return strings.Join([]string{"read", path, strconv.Itoa(offset), strconv.Itoa(limit)}, keySeparator)
}

func (t *Tools) recordSuccessfulRead(ctx context.Context, c *tool.Call, taskID string, r readOutcome) int {
	var version Version
	haveVersion := false
	if r.haveBefore {
		if r.snapshot != nil {
			version, haveVersion = *r.snapshot, true
		} else {
			version, haveVersion = FileVersion(r.resolved)
		}
	}
	stable := false
	if haveVersion {
		metadata := version
		metadata.Digest = [32]byte{}
		current, ok := fileMetadata(r.resolved)
		stable = metadata == r.before && ok && r.before == current
	}
	modified, haveModified := modTime(r.resolved)
	complete := false
	count := 0
	t.tasks.tracker.Update(taskID, func(reads *TaskReads) {
		reads.ClearDedupHit(r.key)
		reads.MarkServed(r.key)
		reads.NoteHistory(ReadKey{Path: r.path, Offset: r.offset, Limit: r.limit})
		count = reads.Bump(readLoopKey(r.path, r.offset, r.limit))
		if haveModified {
			reads.SetReadTime(r.resolved, modified)
		}
		if stable && count < 4 {
			reads.SetDedup(r.key, r.before)
			baseline, held := reads.Baseline(r.resolved)
			complete = held && baseline == version
			if !complete {
				complete = !r.partial
				redacted := r.redacted
				if r.partial {
					complete, redacted = reads.NoteCoverage(r.resolved, version, r.offset, r.endLine, r.totalLines, redacted)
				}
				complete = complete && !redacted
			}
			if complete {
				reads.SetBaseline(r.resolved, version)
				reads.DropBlindPatch(r.resolved)
			}
		}
		if !complete {
			reads.baselines.delete(r.resolved)
		}
		if !stable || count >= 4 {
			reads.DropDedup(r.key)
			reads.UnmarkServed(r.key)
		}
	})
	t.tasks.state.RecordRead(taskID, r.resolved, !complete)
	if complete {
		if err := t.review.MarkSkillRead(ctx, c, r.resolved); err != nil {
			slog.DebugContext(ctx, "background-review read-mark failed", "error", err)
		}
	}
	return count
}

func (t *Tools) readFile(ctx context.Context, c *tool.Call, path string, offset, limit int) (tool.Result, error) {
	taskID := taskOf(c)
	offset, limit = ReadPage(offset, limit, t.limits(ctx))

	if msg := NTNamespaceError(path, "Read"); msg != "" {
		return tool.Failure(msg), nil
	}
	deviceBase, anchored := "", false
	if !filepath.IsAbs(home.ExpandUser(path)) {
		base, err := t.paths.BaseDir(ctx, taskID)
		if err != nil {
			return tool.Result{}, err
		}
		deviceBase, anchored = base, true
	}
	if t.isBlockedDevice(ctx, path, deviceBase, anchored) {
		return tool.Failure("Cannot read '" + validText(path) + "': this is a device file that would " +
			"block or produce infinite output."), nil
	}
	resolved, err := t.paths.Resolve(ctx, path, taskID)
	if err != nil {
		return tool.Result{}, err
	}
	ops, err := t.ops(ctx, c)
	if err != nil {
		return tool.Result{}, err
	}
	hostPaths := ops.env.HostFilesystem()
	if hostPaths {
		if kind := specialFileKind(resolved); kind != "" {
			note := "'" + validText(path) + "' is " + kind + ", not a regular file — reading " +
				"it would block indefinitely, so no read was " +
				"attempted. Use terminal utilities if you need to " +
				"interact with it."
			text, err := fileutil.EncodeJSON(Object{{Key: "success", Value: false}, {Key: "note", Value: note}}, fileutil.JSONStyle{ASCII: true})
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Text: string(text)}, nil
		}
	}
	blocked, err := t.safety.ReadBlockError(ctx, resolved)
	if err != nil {
		return tool.Result{}, err
	}
	if blocked != "" {
		return tool.Failure(validText(blocked)), nil
	}
	extracted, done, err := t.readExtractedDocument(ctx, ops, path, resolved, offset, limit, taskID)
	if err != nil || done {
		return extracted, err
	}
	if HasBinaryExtension(resolved) {
		return tool.Failure("Cannot read binary file '" + validText(path) + "' (" + strings.ToLower(nameSuffix(resolved)) + "). " +
			"Use vision_analyze for images, or terminal to inspect binary files."), nil
	}
	if cached, ok := t.tasks.tracker.CheckNotFound("read", resolved, taskID); ok {
		return tool.Result{Text: cached}, nil
	}

	key := ReadKey{Path: resolved, Offset: offset, Limit: limit}
	var cachedVersion Version
	haveCached, served := false, false
	t.tasks.tracker.Update(taskID, func(reads *TaskReads) {
		cachedVersion, haveCached = reads.Dedup(key)
		served = reads.Served(key)
	})
	var before Version
	haveBefore := false
	if hostPaths {
		before, haveBefore = fileMetadata(resolved)
	}
	if haveCached && !t.review.Background(ctx, c) && haveBefore && before == cachedVersion && served {
		return t.dedupStubOrBlock(taskID, key, path), nil
	}

	target := path
	if hostPaths {
		target = resolved
	}
	result, err := ops.Read(ctx, target, offset, limit)
	if err != nil {
		return tool.Result{}, err
	}
	fields := fieldList(result.Fields())
	if strings.HasPrefix(result.Error, "File not found:") {
		for _, f := range tool.ExpiredArchiveFields(path) {
			fields.set(f.Key, f.Value)
		}
		t.tasks.tracker.RecordNotFound("read", resolved, taskID, encode(fields).Text)
	}
	if result.Error != "" || result.IsBinary {
		return encode(fields), nil
	}

	content := result.Content
	truncated := result.Truncated
	nextOffset, byBudget := 0, false
	if maxChars := t.maxReadChars(ctx); charLen(content) > maxChars {
		content, nextOffset = applyCharBudget(&fields, content, offset, result.TotalLines, maxChars)
		truncated, byBudget = true, true
	}
	redacted := false
	if content != "" {
		unredacted := content
		content = t.redactRead(ctx, unredacted, resolved)
		redacted = content != unredacted
		fields.set("content", content)
		if conflicts := ConflictBlocks(content); conflicts > 0 {
			fields.set("conflict_blocks", conflicts)
			fields.set("_hint", strconv.Itoa(conflicts)+" unresolved git merge-conflict block(s) (<<<<<<< / ======= / >>>>>>>) in this "+
				"range. Resolve them (keep one side or combine, delete the markers) before editing around them.")
		}
	}
	if result.FileSize > largeFileHintBytes && limit > 200 && truncated && !fields.has("_hint") {
		fields.set("_hint", "This file is large ("+commas(result.FileSize)+" bytes). "+
			"Consider reading only the section you need with offset and limit "+
			"to keep context usage efficient.")
	}

	endLine := offset + limit - 1
	if byBudget {
		endLine = nextOffset - 1
	} else if result.TotalLines > 0 {
		endLine = min(endLine, result.TotalLines)
	}
	count := t.recordSuccessfulRead(ctx, c, taskID, readOutcome{
		path: path, resolved: resolved, offset: offset, limit: limit, key: key,
		partial:  offset > 1 || truncated,
		redacted: redacted || fields.has("truncated_lines"),
		endLine:  endLine, totalLines: result.TotalLines,
		before: before, haveBefore: haveBefore, snapshot: result.Version,
	})
	if count >= 4 {
		return tool.Failure(
			"BLOCKED: You have read this exact file region "+strconv.Itoa(count)+" times in a row. "+
				"The content has NOT changed. You already have this information. "+
				"STOP re-reading and proceed with your task.",
			tool.Field{Key: "path", Value: validText(path)},
			tool.Field{Key: "already_read", Value: count},
			tool.Field{Key: tool.GuardrailRefusalKey, Value: true}), nil
	}
	if count >= 3 {
		fields.set("_warning", "You have read this exact file region "+strconv.Itoa(count)+" times consecutively. "+
			"The content has not changed since your last read. Use the information you already have. "+
			"If you are stuck in a loop, stop reading and proceed with writing or responding.")
	}
	return encode(fields), nil
}
