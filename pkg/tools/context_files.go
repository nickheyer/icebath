package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	contextFileReadTimeoutSecs = 5.0
	ContextFileMaxChars        = 20000
	ContextTruncateHeadRatio   = 0.7
	ContextTruncateTailRatio   = 0.2
	contextFileWindowFraction  = 0.06
	contextFileDynamicCeiling  = 500000
)

// Readonly config of the env home, empty with a debug log when it cannot be read
func ContextConfigReadonly(env *Env, what string) map[string]any {
	h := envHome(env)
	tree, err := config.LoadReadonly(context.Background(), h, config.NewSecrets(h).Lookup())
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read %s from config: %v", what, err))
		return map[string]any{}
	}
	return tree
}

// Positive int, float or bool config value as a float, false for anything else
func positiveConfigNumber(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case bool:
		if n {
			f = 1
		}
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	case float64:
		f = n
	default:
		return 0, false
	}
	return f, f > 0
}

// Configured context_file_read_timeout in seconds, else five
func contextFileReadTimeout(env *Env) float64 {
	if f, ok := positiveConfigNumber(ContextConfigReadonly(env, "context_file_read_timeout")["context_file_read_timeout"]); ok {
		return f
	}
	return contextFileReadTimeoutSecs
}

// Text of path decoded as utf-8-sig with universal newlines, read errors returned unchanged
func readTextFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		pos := 0
		for pos < len(data) {
			r, size := utf8.DecodeRune(data[pos:])
			if r == utf8.RuneError && size <= 1 {
				break
			}
			pos += size
		}
		return "", fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", data[pos], pos)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// File reader the timed context read runs, swapped by tests for a read that blocks
var ReadContextFileText = readTextFile

// Reads path on its own goroutine, false after the timeout so a slow file cannot stall prompt building
func ReadTextWithTimeout(env *Env, path string, timeout *float64) (string, bool, error) {
	secs := 0.0
	if timeout != nil {
		secs = *timeout
	} else {
		secs = contextFileReadTimeout(env)
	}
	type readResult struct {
		text string
		err  error
	}
	result := make(chan readResult, 1)
	read := ReadContextFileText
	go func() {
		text, err := read(path)
		result <- readResult{text, err}
	}()
	timer := time.NewTimer(time.Duration(secs * float64(time.Second)))
	defer timer.Stop()
	select {
	case r := <-result:
		if r.err != nil {
			return "", false, r.err
		}
		return r.text, true, nil
	case <-timer.C:
		slog.Warn(fmt.Sprintf("Context file %s read timed out after %.1fs; skipping", path, secs))
		return "", false, nil
	}
}

// Injection scanned content, a blocked notice for project files and a warning only for user authored files
func ScanContextContent(content, filename string, userAuthored bool) string {
	content = strings.TrimPrefix(content, "\ufeff")
	findings, _ := ScanForThreats(content, "context")
	if len(findings) == 0 {
		return content
	}
	joined := strings.Join(findings, ", ")
	if userAuthored {
		slog.Warn(fmt.Sprintf("Context file %s matched injection pattern(s) %s; loaded anyway because it is the "+
			"user's own file in ICEBATH_HOME — review it if you did not write that text", filename, joined))
		return content
	}
	slog.Warn(fmt.Sprintf("Context file %s blocked: %s", filename, joined))
	return fmt.Sprintf("[BLOCKED: %s contained potential prompt injection (%s). Content not loaded.]", filename, joined)
}

// Character cap scaled to the model window within the twenty thousand floor and five hundred thousand ceiling
func dynamicContextFileMaxChars(contextLength *int) int {
	if contextLength == nil || *contextLength <= 0 {
		return ContextFileMaxChars
	}
	budget := int(float64(*contextLength) * CharsPerToken * contextFileWindowFraction)
	return max(ContextFileMaxChars, min(budget, contextFileDynamicCeiling))
}

// Context file truncation limit, an explicit context_file_max_chars winning over the dynamic cap
func ContextFileMaxCharsFor(env *Env, contextLength *int) int {
	if f, ok := positiveConfigNumber(ContextConfigReadonly(env, "context_file_max_chars")["context_file_max_chars"]); ok {
		return int(f)
	}
	return dynamicContextFileMaxChars(contextLength)
}

type truncationWarningList struct {
	mu    sync.Mutex
	items []string
}

type truncationWarningBinding struct {
	mu   sync.Mutex
	list *truncationWarningList
}

type truncationWarningsKey struct{}

var rootTruncationWarnings = &truncationWarningBinding{}

// Truncation warning binding of ctx, the process root binding when ctx carries none
func truncationBinding(ctx context.Context) *truncationWarningBinding {
	if ctx != nil {
		if b, ok := ctx.Value(truncationWarningsKey{}).(*truncationWarningBinding); ok {
			return b
		}
	}
	return rootTruncationWarnings
}

// Child context whose truncation warnings binding starts as a copy of the parent binding
func CopyTruncationWarningsContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	parent := truncationBinding(ctx)
	parent.mu.Lock()
	list := parent.list
	parent.mu.Unlock()
	return context.WithValue(ctx, truncationWarningsKey{}, &truncationWarningBinding{list: list})
}

// Returns and clears the truncation warnings accumulated in this context
func DrainTruncationWarnings(ctx context.Context) []string {
	b := truncationBinding(ctx)
	b.mu.Lock()
	list := b.list
	b.mu.Unlock()
	if list == nil {
		return []string{}
	}
	list.mu.Lock()
	defer list.mu.Unlock()
	drained := append([]string{}, list.items...)
	list.items = list.items[:0]
	return drained
}

// Appends a warning to the context binding, creating its list on first use
func queueTruncationWarning(ctx context.Context, msg string) {
	b := truncationBinding(ctx)
	b.mu.Lock()
	if b.list == nil {
		b.list = &truncationWarningList{}
	}
	list := b.list
	b.mu.Unlock()
	list.mu.Lock()
	list.items = append(list.items, msg)
	list.mu.Unlock()
}

// Options of TruncateContent, nil pointers being the Python None defaults
type TruncateOptions struct {
	MaxChars      *int
	ContextLength *int
	ReadPath      string
	NoQueue       bool
}

// Head and tail kept with a marker naming omitted headings and the file to read for the full text
func TruncateContent(ctx context.Context, env *Env, content, filename string, o TruncateOptions) string {
	maxChars := 0
	if o.MaxChars != nil {
		maxChars = *o.MaxChars
	} else {
		maxChars = ContextFileMaxCharsFor(env, o.ContextLength)
	}
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}
	readPath := o.ReadPath
	if readPath == "" {
		readPath = filename
	}
	remedy := "trim the file, pin a larger context_file_max_chars, or use a larger-context model!"
	if o.NoQueue {
		remedy = "the full file stays readable with read_file: " + readPath
	}
	msg := fmt.Sprintf("⚠️  Context file %s TRUNCATED: %d chars exceeds limit of %d — %s", filename, len(runes), maxChars, remedy)
	slog.Warn(msg)
	if !o.NoQueue {
		queueTruncationWarning(ctx, msg)
	}
	headChars := int(float64(maxChars) * ContextTruncateHeadRatio)
	tailChars := int(float64(maxChars) * ContextTruncateTailRatio)
	omitted := omittedHeadings(runes, headChars, len(runes)-tailChars, 15)
	sections := ""
	if len(omitted) > 0 {
		sections = " Omitted sections: " + strings.Join(omitted, "; ") + "."
	}
	marker := fmt.Sprintf("\n\n[...truncated %s: kept %d+%d of %d chars. The middle is omitted.%s If you need the full "+
		"instructions, read the complete file with the read_file tool: %s]\n\n", filename, headChars, tailChars, len(runes), sections, readPath)
	return pySliceTo(runes, headChars) + marker + pySliceFrom(runes, -tailChars)
}

// Python content[:n] over code points
func pySliceTo(runes []rune, n int) string {
	if n < 0 {
		n += len(runes)
	}
	return string(runes[:max(0, min(n, len(runes)))])
}

// Python content[n:] over code points, a zero start keeping everything
func pySliceFrom(runes []rune, n int) string {
	if n < 0 {
		n += len(runes)
	}
	return string(runes[max(0, min(n, len(runes))):])
}

var markdownHeadingRe = regexp.MustCompile(`^#{1,6} `)

// Whether line opens with one to six hashes, a space and a non whitespace character
func isMarkdownHeading(line string) bool {
	loc := markdownHeadingRe.FindStringIndex(line)
	if loc == nil {
		return false
	}
	r, size := utf8.DecodeRuneInString(line[loc[1]:])
	return size > 0 && !pystr.IsSpace(r)
}

// Markdown headings starting inside the omitted span, fenced code comments skipped, capped with an ellipsis
func omittedHeadings(runes []rune, start, end, limit int) []string {
	headings := []string{}
	offset, fenced := 0, false
	for _, line := range pystr.Splitlines(string(runes), true) {
		stripped := pystr.Strip(line)
		if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			fenced = !fenced
		} else if !fenced && start <= offset && offset < end && isMarkdownHeading(stripped) {
			headings = append(headings, pystr.Strip(strings.TrimLeft(stripped, "#")))
		}
		offset += utf8.RuneCountInString(line)
	}
	if len(headings) > limit {
		return append(headings[:limit:limit], "...")
	}
	return headings
}
