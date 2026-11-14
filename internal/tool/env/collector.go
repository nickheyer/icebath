package env

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	unboundedCaptureChars = math.MaxInt64
	spillCapChars         = 5_000_000
	spillMaxAge           = 7 * 24 * time.Hour
)

type piece struct {
	text  string
	chars int
}

type collector struct {
	mu          sync.Mutex
	maxChars    int
	headLimit   int
	tailLimit   int
	head        []string
	headChars   int
	tail        []piece
	tailChars   int
	total       int
	spillPath   string
	mkdir       func(dir string) error
	spill       *os.File
	spillChars  int
	spillCapped bool
}

func newCollector(maxChars int, spillPath string, mkdir func(dir string) error) *collector {
	if maxChars < 1 {
		maxChars = 1
	}
	head := int(float64(maxChars) * 0.4)
	return &collector{maxChars: maxChars, headLimit: head, tailLimit: maxChars - head, spillPath: spillPath, mkdir: mkdir}
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for count := 0; i < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

func lastRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := len(s)
	for count := 0; i > 0 && count < n; count++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

func groupThousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func truncationNotice(omitted, total int) string {
	return "\n\n... [OUTPUT TRUNCATED - " + groupThousands(omitted) + " chars omitted out of " + groupThousands(total) + " total] ...\n\n"
}

func headTailSplit(budget int) (int, int) {
	head := int(float64(budget) * 0.4)
	return head, budget - head
}

func (c *collector) spillWrite(text string) error {
	if spillNewline != "\n" {
		text = strings.ReplaceAll(text, "\n", spillNewline)
	}
	_, err := c.spill.WriteString(text)
	return err
}

func (c *collector) openSpill() error {
	dir := filepath.Dir(c.spillPath)
	if err := c.mkdir(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &fs.PathError{Op: "spill", Path: dir, Err: errors.New("spill dir is not a directory (symlink?)")}
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(c.spillPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	c.spill = f
	backlog := strings.Join(c.head, "")
	for _, p := range c.tail {
		backlog += p.text
	}
	if err := c.spillWrite(backlog); err != nil {
		return err
	}
	c.spillChars = utf8.RuneCountInString(backlog)
	return nil
}

func (c *collector) maybeSpill(text string, n int) {
	if c.spillPath == "" || c.spillCapped {
		return
	}
	if c.spill == nil {
		if err := c.openSpill(); err != nil {
			c.spillCapped = true
			return
		}
	}
	budget := spillCapChars - c.spillChars
	var err error
	if budget <= 0 || n > budget {
		if err = c.spillWrite(firstRunes(text, budget)); err == nil {
			err = c.spillWrite("\n... [spill capped at 5,000,000 chars] ...\n")
		}
		c.spillCapped = true
	} else {
		err = c.spillWrite(text)
	}
	if err != nil {
		c.spillCapped = true
		return
	}
	c.spillChars += n
}

func (c *collector) closeSpill() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spill == nil {
		return ""
	}
	_ = c.spill.Close()
	c.spill = nil
	return c.spillPath
}

func (c *collector) totalChars() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *collector) add(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := utf8.RuneCountInString(text)
	if c.spillPath != "" && (c.spill != nil || c.total+n > c.maxChars) {
		c.maybeSpill(text, n)
	}
	c.total += n
	start, startByte := 0, 0
	if c.headChars < c.headLimit {
		if take := min(c.headLimit-c.headChars, n); take > 0 {
			part := firstRunes(text, take)
			c.head = append(c.head, part)
			c.headChars += take
			start, startByte = take, len(part)
		}
	}
	remaining := n - start
	if remaining <= 0 || c.tailLimit <= 0 {
		return
	}
	if remaining >= c.tailLimit {
		c.tail = []piece{{text: lastRunes(text, c.tailLimit), chars: c.tailLimit}}
		c.tailChars = c.tailLimit
		return
	}
	c.tail = append(c.tail, piece{text: text[startByte:], chars: remaining})
	c.tailChars += remaining
	for excess := c.tailChars - c.tailLimit; excess > 0; excess = c.tailChars - c.tailLimit {
		first := c.tail[0]
		if first.chars <= excess {
			c.tail = c.tail[1:]
			c.tailChars -= first.chars
			continue
		}
		c.tail[0] = piece{text: lastRunes(first.text, first.chars-excess), chars: first.chars - excess}
		c.tailChars -= excess
	}
}

func (c *collector) render(suffix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	suffixChars := utf8.RuneCountInString(suffix)
	if suffixChars >= c.maxChars {
		return lastRunes(suffix, c.maxChars)
	}
	head := strings.Join(c.head, "")
	var tb strings.Builder
	for _, p := range c.tail {
		tb.WriteString(p.text)
	}
	tail := tb.String()
	available := c.maxChars - suffixChars
	if c.total <= available {
		return head + tail + suffix
	}
	notice := ""
	for range 4 {
		omitted := max(0, c.total-max(0, available-utf8.RuneCountInString(notice)))
		updated := truncationNotice(omitted, c.total)
		if updated == notice {
			break
		}
		notice = updated
	}
	budget := max(0, available-utf8.RuneCountInString(notice))
	headChars, tailChars := headTailSplit(budget)
	return firstRunes(head, headChars) + firstRunes(notice, available) + lastRunes(tail, tailChars) + suffix
}

func pruneSpills(dir string, now time.Time) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	matches, err := filepath.Glob(filepath.Join(dir, "out-*.log"))
	if err != nil {
		return
	}
	cutoff := now.Add(-spillMaxAge)
	for _, old := range matches {
		if st, err := os.Stat(old); err == nil && st.ModTime().Before(cutoff) {
			_ = os.Remove(old)
		}
	}
}
