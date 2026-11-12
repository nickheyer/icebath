package desktoplog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	// Size at which a log moves to its backups
	LogMaxBytes = 10 * 1024 * 1024
	// Backups kept beside a rotated log
	LogBackupCount = 3
	// Size past which every generation is deleted instead of rotated
	LogDiscardBytes = LogMaxBytes * 4
	// Interval of the in place reclaim of a log another process keeps open
	ActiveLogPollInterval = 5 * time.Minute

	flushDelay     = 120 * time.Millisecond
	bufferMaxChars = 64 * 1024
	recentCap      = 300
)

// Path of the nth backup of a log
func LogBackupPath(base string, n int) string { return fmt.Sprintf("%s.%d", base, n) }

// Size and truncation of a log another process keeps appending to
type ActiveLogIO interface {
	// Live size, false when the file does not exist yet
	Size(file string) (int64, bool)
	Truncate(file string) error
}

// Truncates an oversized active log in place, never renaming it away from its writer
func ReclaimActiveLogIfOversized(file string, io ActiveLogIO) (bool, error) {
	size, ok := io.Size(file)
	if !ok || size < LogMaxBytes {
		return false, nil
	}
	if err := io.Truncate(file); err != nil {
		return false, err
	}
	return true, nil
}

// One rotation step, a removal when Dst is empty and a rename otherwise
type RotationOp struct {
	Op  string
	Src string
	Dst string
}

// Rotation steps for a log of the given size, deleting every generation past the discard ceiling
func PlanLogRotation(size int64, base string) []RotationOp {
	if size < LogMaxBytes {
		return nil
	}
	if size > LogDiscardBytes {
		ops := []RotationOp{{Op: "rm", Src: base}}
		for i := 1; i <= LogBackupCount; i++ {
			ops = append(ops, RotationOp{Op: "rm", Src: LogBackupPath(base, i)})
		}
		return ops
	}
	ops := []RotationOp{{Op: "rm", Src: LogBackupPath(base, LogBackupCount)}}
	for i := LogBackupCount - 1; i >= 1; i-- {
		ops = append(ops, RotationOp{Op: "mv", Src: LogBackupPath(base, i), Dst: LogBackupPath(base, i+1)})
	}
	return append(ops, RotationOp{Op: "mv", Src: base, Dst: LogBackupPath(base, 1)})
}

// Applies the rotation plan for the live size of base, ignoring every failure
func RotateLogIfNeeded(base string) {
	st, err := os.Stat(base)
	if err != nil {
		return
	}
	for _, op := range PlanLogRotation(st.Size(), base) {
		if op.Op == "rm" {
			if st, err := os.Lstat(op.Src); err == nil && !st.IsDir() {
				_ = os.Remove(op.Src)
			}
		} else {
			_ = os.Rename(op.Src, op.Dst)
		}
	}
}

// Local time stamp in the asctime shape of Python logs, 2026-09-28 13:18:46,062
func FormatLogStamp(t time.Time) string {
	return fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d,%03d", t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1e6)
}

// Desktop log line of text under a local time stamp
func FormatDesktopLogLine(text, stamp string) string { return stamp + " [icebath] " + text }

var lineBreak = regexp.MustCompile(`\r?\n`)

// Trims the whitespace JavaScript String.prototype.trim removes
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

// Length of s in UTF-16 code units as JavaScript counts it
func JSLength(s string) int {
	n := 0
	for _, r := range s {
		n += len(utf16.Encode([]rune{r}))
	}
	return n
}

// First n UTF-16 code units of s, a split surrogate pair written as U+FFFD as Node encodes it
func JSSlice(s string, n int) string {
	units := 0
	for i, r := range s {
		w := len(utf16.Encode([]rune{r}))
		if units+w > n {
			if units < n {
				return s[:i] + "�"
			}
			return s[:i]
		}
		units += w
	}
	return s
}

// Desktop log of the main process, a ring of recent lines plus a debounced append to desktop.log
type Log struct {
	path    string
	now     func() time.Time
	mu      sync.Mutex
	recent  []string
	buffer  string
	timer   *time.Timer
	pending []string
	writing bool
	idle    *sync.Cond
}

// Log appending to path, stamping lines with now
func New(path string, now func() time.Time) *Log {
	l := &Log{path: path, now: now}
	l.idle = sync.NewCond(&l.mu)
	return l
}

// File the log appends to
func (l *Log) Path() string { return l.path }

// Records a chunk of text, one stamp shared by all its lines, and schedules the append
func (l *Log) Remember(chunk string) {
	text := jsTrim(chunk)
	if text == "" {
		return
	}
	stamp := FormatLogStamp(l.now())
	lines := lineBreak.Split(text, -1)
	for i, line := range lines {
		lines[i] = FormatDesktopLogLine(line, stamp)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = append(l.recent, lines...)
	if len(l.recent) > recentCap {
		l.recent = append([]string(nil), l.recent[len(l.recent)-recentCap:]...)
	}
	l.buffer += strings.Join(lines, "\n") + "\n"
	if JSLength(l.buffer) >= bufferMaxChars {
		if l.timer != nil {
			l.timer.Stop()
			l.timer = nil
		}
		l.flushAsyncLocked()
		return
	}
	if l.timer == nil {
		l.timer = time.AfterFunc(flushDelay, func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.timer = nil
			l.flushAsyncLocked()
		})
	}
}

// Last n remembered lines, oldest first
func (l *Log) Recent(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := len(l.recent) - n
	if start < 0 {
		start = 0
	}
	return append([]string{}, l.recent[start:]...)
}

func (l *Log) appendChunk(chunk string) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o777); err != nil {
		return err
	}
	RotateLogIfNeeded(l.path)
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(chunk)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Queues the buffer for the ordered background writer
func (l *Log) flushAsyncLocked() {
	if l.buffer == "" {
		return
	}
	l.pending = append(l.pending, l.buffer)
	l.buffer = ""
	if l.writing {
		return
	}
	l.writing = true
	go func() {
		l.mu.Lock()
		for len(l.pending) > 0 {
			chunk := l.pending[0]
			l.pending = l.pending[1:]
			l.mu.Unlock()
			_ = l.appendChunk(chunk)
			l.mu.Lock()
		}
		l.writing = false
		l.idle.Broadcast()
		l.mu.Unlock()
	}()
}

// Appends the buffered lines now, for a window that may be gone before the debounced flush
func (l *Log) FlushSync() {
	l.mu.Lock()
	chunk := l.buffer
	l.buffer = ""
	l.mu.Unlock()
	if chunk == "" {
		return
	}
	_ = l.appendChunk(chunk)
}

// Waits until every queued append has landed, for shutdown
func (l *Log) Drain() {
	l.mu.Lock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.flushAsyncLocked()
	for l.writing {
		l.idle.Wait()
	}
	l.mu.Unlock()
}
