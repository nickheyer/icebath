package logging

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
)

// Error for a component filter that names no known component
var ErrUnknownComponent = errors.New("unknown log component")

// Whole read limit, chunk sizes of a backward read and the pause between polls of a followed log
const (
	wholeReadLimit = 1 << 20
	firstChunk     = 8192
	largestChunk   = 65536
	followPoll     = 300 * time.Millisecond
)

// Level words in the order the minimum level filter ranks them
var lineLevels = [...]string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"}

// Patterns of the timestamp, the level word and the source name a log line carries
var (
	lineStamp  = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2})`)
	lineLevel  = regexp.MustCompile(`\s(DEBUG|INFO|WARNING|ERROR|CRITICAL)\s`)
	lineSource = regexp.MustCompile(`\s(?:DEBUG|INFO|WARNING|ERROR|CRITICAL)(?:\s+\[.*?\])?\s+(\S+):`)
	sinceText  = regexp.MustCompile(`^(\d+)\s*([smhd])$`)
)

// Lines the log readers select, each zero field selecting every line
type Filter struct {
	Level     string
	Session   string
	Since     time.Time
	Component string
}

// Reports whether a level word is one the minimum level filter ranks
func ValidLevel(name string) bool { return slices.Contains(lineLevels[:], name) }

// Rank of a level word, the lowest for a word the filter does not know
func levelRank(name string) int { return max(slices.Index(lineLevels[:], name), 0) }

// Reports whether any field of f narrows the lines
func (f Filter) active() bool {
	return f.Level != "" || f.Session != "" || !f.Since.IsZero() || f.Component != ""
}

// Stateful filter over consecutive lines of one log, an unstamped line taking the verdict of the record above it
type lineFilter struct {
	f        Filter
	prefixes []string
	carry    int
}

// Filter of f with its component resolved, ErrUnknownComponent for a component nobody knows
func (f Filter) compile() (*lineFilter, error) {
	lf := &lineFilter{f: f}
	if f.Component != "" {
		p, ok := ComponentPrefixes(f.Component)
		if !ok {
			return nil, ErrUnknownComponent
		}
		lf.prefixes = p
	}
	return lf, nil
}

// Local time at the first timestamp in line, false when the line carries none or an impossible one
func lineTime(line string) (time.Time, bool) {
	m := lineStamp.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.Replace(m[1], "T", " ", 1), time.Local)
	return t, err == nil
}

// Reports whether one line passes every filter, a line without a timestamp or level passing those two
func (lf *lineFilter) matches(line string) bool {
	if !lf.f.Since.IsZero() {
		if t, ok := lineTime(line); ok && t.Before(lf.f.Since) {
			return false
		}
	}
	if lf.f.Level != "" {
		if m := lineLevel.FindStringSubmatch(line); m != nil && levelRank(m[1]) < levelRank(lf.f.Level) {
			return false
		}
	}
	if lf.f.Session != "" && !strings.Contains(line, lf.f.Session) {
		return false
	}
	if lf.f.Component == "" {
		return true
	}
	m := lineSource.FindStringSubmatch(line)
	return m != nil && underPrefix(m[1], lf.prefixes)
}

// Verdict for the next line, a continuation line before any stamp kept only when no time or level filter applies
func (lf *lineFilter) keep(line string) bool {
	if _, stamped := lineTime(line); stamped {
		lf.carry = -1
		if lf.matches(line) {
			lf.carry = 1
		}
		return lf.carry > 0
	}
	if lf.carry == 0 {
		return lf.f.Since.IsZero() && lf.f.Level == "" && lf.matches(line)
	}
	return lf.carry > 0
}

// Last n lines of the log at path passing f, all lines read for zero n
func Tail(path string, n int, f Filter) ([]string, error) {
	lf, err := f.compile()
	if err != nil {
		return nil, err
	}
	if !f.active() {
		return readLast(path, n)
	}
	raw, err := readLast(path, max(n*20, 2000))
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, line := range raw {
		if lf.keep(line) {
			kept = append(kept, line)
		}
	}
	return lastN(kept, n), nil
}

// Final n lines, all of them for zero, and all but the first minus n for a negative n
func lastN(lines []string, n int) []string {
	switch {
	case n == 0:
		return lines
	case n < 0:
		return lines[min(-n, len(lines)):]
	}
	return lines[max(len(lines)-n, 0):]
}

// Last n lines, the whole file read up to a megabyte and growing chunks from the end past it
func readLast(path string, n int) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return readAll(path, n)
	}
	if st.Size() == 0 {
		return nil, nil
	}
	if st.Size() <= wholeReadLimit {
		return readAll(path, n)
	}
	lines, err := readEnd(path, st.Size(), n)
	if err != nil {
		return readAll(path, n)
	}
	return lines, nil
}

// Last n lines of the whole log, a leading byte order mark dropped and every line ending read as newline
func readAll(path string, n int) ([]string, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return lastN(splitLines(decodeReplacing(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))), n), nil
}

// Lines of text with CRLF and lone CR read as newlines, each keeping its newline
func splitLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []string
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+1])
		text = text[i+1:]
	}
	return out
}

// Last n non-blank lines of a large log, read backwards in growing chunks until enough lines are in hand
func readEnd(path string, size int64, n int) ([]string, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	chunk := int64(firstChunk)
	pos := size
	var lines [][]byte
	for pos > 0 && len(lines) <= n+1 {
		read := min(chunk, pos)
		pos -= read
		buf := make([]byte, read)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil, err
		}
		parts := bytes.Split(buf, []byte("\n"))
		if lines != nil {
			lines[0] = append(parts[len(parts)-1], lines[0]...)
			lines = append(parts[:len(parts)-1], lines...)
		} else {
			lines = parts
		}
		chunk = min(chunk*2, largestChunk)
	}
	var out []string
	for _, raw := range lines {
		if len(bytes.Trim(raw, " \t\n\r\v\f")) > 0 {
			out = append(out, decodeReplacing(raw)+"\n")
		}
	}
	return lastN(out, n), nil
}

// Text of b with each maximal invalid UTF-8 subpart read as one replacement character, as the Unicode standard recommends
func decodeReplacing(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var s strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			s.WriteRune(utf8.RuneError)
			b = b[invalidSubpart(b):]
			continue
		}
		s.Write(b[:size])
		b = b[size:]
	}
	return s.String()
}

// Length of the invalid sequence opening b, the lead byte and the continuation bytes it validly started
func invalidSubpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xbf), 0
	switch c := b[0]; {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c == 0xed:
		need, hi = 2, 0x9f
	case c >= 0xe1 && c <= 0xef:
		need = 2
	case c == 0xf0:
		need, lo = 3, 0x90
	case c == 0xf4:
		need, hi = 3, 0x8f
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	}
	n := 1
	for n <= need && n < len(b) && b[n] >= lo && b[n] <= hi {
		n, lo, hi = n+1, 0x80, 0xbf
	}
	return n
}

// Sends each line the log at path gains to emit when it passes f, across rotation, until ctx ends
func Follow(ctx context.Context, path string, f Filter, emit func(line string)) error {
	lf, err := f.compile()
	if err != nil {
		return err
	}
	file, err := openShared(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r := bufio.NewReader(file)
	for {
		chunk, err := r.ReadBytes('\n')
		for _, line := range splitLines(decodeReplacing(chunk)) {
			if lf.keep(line) {
				emit(line)
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		if next := reopenRotated(path, file); next != nil {
			_ = file.Close()
			file = next
			r.Reset(file)
			continue
		}
		t := time.NewTimer(followPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// Live log reopened at path once the held one was rotated away, nil while it is still the live log
func reopenRotated(path string, held *os.File) *os.File {
	now, err := os.Stat(path)
	if err != nil {
		return nil
	}
	cur, err := held.Stat()
	if err != nil || os.SameFile(now, cur) {
		return nil
	}
	next, err := openShared(path)
	if err != nil {
		return nil
	}
	return next
}

// Cutoff a relative age such as 1h, 30m or 2d names before now, false for any other text
func ParseSince(text string, now time.Time) (time.Time, bool) {
	m := sinceText.FindStringSubmatch(strings.ToLower(strings.TrimSpace(text)))
	if m == nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	if n > int64(time.Duration(1<<63-1)/unit) {
		return time.Time{}, false
	}
	return now.Add(-time.Duration(n) * unit), true
}

// Log file present for listing, its file name, location, size, last change and whether the install root holds it
type LogListing struct {
	File     string
	Path     string
	Size     int64
	Modified time.Time
	Root     bool
}

// Files in the logs directory of the home of ctx, root logs a profile lacks, and whether the directory exists
func ListLogs(ctx context.Context, r *home.Resolver) ([]LogListing, bool, error) {
	h, err := r.Home(ctx)
	if err != nil {
		return nil, false, err
	}
	logDir := home.Logs.In(h)
	var rootLogs []LogListing
	for _, l := range Logs() {
		if !l.Root {
			continue
		}
		p, err := l.Path(ctx, r)
		if err != nil {
			return nil, false, err
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && filepath.Dir(p) != logDir {
			rootLogs = append(rootLogs, LogListing{File: filepath.Base(p), Path: p, Size: st.Size(), Modified: st.ModTime(), Root: true})
		}
	}
	exists := isDir(logDir)
	if !exists {
		return rootLogs, false, nil
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return nil, true, err
	}
	var out []LogListing
	for _, e := range entries {
		p := filepath.Join(logDir, e.Name())
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && nameSuffix(e.Name()) == ".log" {
			out = append(out, LogListing{File: e.Name(), Path: p, Size: st.Size(), Modified: st.ModTime()})
		}
	}
	return append(out, rootLogs...), true, nil
}

// Final extension of a file name, empty for a name whose only dot leads or ends it
func nameSuffix(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}
