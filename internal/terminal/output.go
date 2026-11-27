package terminal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Hard ceiling of one spill file
const spillCap = 5_000_000

// Marker between the kept head and tail of output that was cut
func TruncationNotice(omitted, total int, label, unit string) string {
	if label == "" {
		label = "OUTPUT"
	}
	if unit == "" {
		unit = "chars"
	}
	return "\n\n... [" + label + " TRUNCATED - " + strs.Thousands(omitted) + " " + unit + " omitted out of " + strs.Thousands(total) + " total] ...\n\n"
}

// Head and tail budgets of a total budget, 40 percent head
func HeadTail(budget int) (int, int) {
	head := int(float64(budget) * 0.4)
	return head, budget - head
}

// Head and tail of text around a truncation notice once it passes max characters
func Truncate(text string, max int, label string) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	head, tail := HeadTail(max)
	return string(runes[:head]) + TruncationNotice(len(runes)-head-tail, len(runes), label, "chars") + string(runes[len(runes)-tail:])
}

// Bytes as text, one replacement character per invalid byte
func Decode(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		b.WriteRune(r)
		data = data[size:]
	}
	return b.String()
}

// Creates a folder for spill files, refusing a symlinked leaf and keeping a private one to its owner
func SpillDir(path string, private bool) error {
	mode := os.FileMode(0o777)
	if private {
		mode = 0o700
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("spill dir is not a directory (symlink?): %s", path)
	}
	if private && st.Mode().Perm() != 0o700 {
		return os.Chmod(path, 0o700)
	}
	return nil
}

// Creates the file for writing, never following a link, replacing what is there when overwrite is set
func OpenExclusive(path string, private, overwrite bool) (*os.File, error) {
	if overwrite {
		st, err := os.Lstat(path)
		switch {
		case err == nil && st.IsDir():
			return nil, fmt.Errorf("refusing to overwrite a directory: %s", path)
		case err == nil:
			if err := os.Remove(path); err != nil {
				return nil, err
			}
		case !os.IsNotExist(err):
			return nil, err
		}
	}
	mode := os.FileMode(0o666)
	if private {
		mode = 0o600
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, mode)
}

// Writes text through OpenExclusive
func WriteExclusive(path, text string, private, overwrite bool) error {
	f, err := OpenExclusive(path, private, overwrite)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Head and tail window of streamed output, the whole of it copied to a spill file once it overflows
type collector struct {
	mu        sync.Mutex
	max       int
	headLimit int
	tailLimit int
	head      []string
	tail      []string
	headChars int
	tailChars int
	total     int
	spillPath string
	spill     *os.File
	spilled   int
	capped    bool
}

// Collector keeping a 40/60 head and tail window of at most max characters, everything when max is zero
func newCollector(max int, spill string) *collector {
	if max <= 0 {
		max, spill = int(^uint(0)>>1), ""
	}
	head, _ := HeadTail(max)
	return &collector{max: max, headLimit: head, tailLimit: max - head, spillPath: spill}
}

// Copies text to the spill file, opened on the first overflow with what came before
func (c *collector) toSpill(text string) {
	if c.spillPath == "" || c.capped {
		return
	}
	if c.spill == nil {
		if err := SpillDir(filepath.Dir(c.spillPath), true); err != nil {
			c.capped = true
			return
		}
		f, err := OpenExclusive(c.spillPath, true, false)
		if err != nil {
			c.capped = true
			return
		}
		c.spill = f
		backlog := strings.Join(c.head, "") + strings.Join(c.tail, "")
		if _, err := f.WriteString(backlog); err != nil {
			c.capped = true
			return
		}
		c.spilled = len(backlog)
	}
	budget := spillCap - c.spilled
	var err error
	if budget <= 0 || len(text) > budget {
		_, err = c.spill.WriteString(text[:max(budget, 0)])
		if err == nil {
			_, err = c.spill.WriteString("\n... [spill capped at 5,000,000 chars] ...\n")
		}
		c.capped = true
	} else {
		_, err = c.spill.WriteString(text)
	}
	if err != nil {
		c.capped = true
		return
	}
	c.spilled += len(text)
}

func (c *collector) add(text string) {
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(text)
	if c.spillPath != "" && (c.spill != nil || c.total+n > c.max) {
		c.toSpill(text)
	}
	c.total += n
	start := 0
	if c.headChars < c.headLimit {
		if take := min(c.headLimit-c.headChars, n); take > 0 {
			c.head = append(c.head, text[:take])
			c.headChars += take
			start = take
		}
	}
	if n-start <= 0 || c.tailLimit <= 0 {
		return
	}
	if n-start >= c.tailLimit {
		c.tail, c.tailChars = []string{text[n-c.tailLimit:]}, c.tailLimit
		return
	}
	c.tail = append(c.tail, text[start:])
	c.tailChars += n - start
	for excess := c.tailChars - c.tailLimit; excess > 0; excess = c.tailChars - c.tailLimit {
		if first := c.tail[0]; len(first) <= excess {
			c.tail = c.tail[1:]
			c.tailChars -= len(first)
		} else {
			c.tail[0] = first[excess:]
			c.tailChars -= excess
		}
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// Output within the window with a status suffix kept whole
func (c *collector) render(suffix string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(suffix) >= c.max {
		return suffix[len(suffix)-c.max:]
	}
	head, tail := strings.Join(c.head, ""), strings.Join(c.tail, "")
	available := c.max - len(suffix)
	if c.total <= available {
		return head + tail + suffix
	}
	notice := ""
	for range 4 {
		updated := TruncationNotice(max(c.total-max(available-len(notice), 0), 0), c.total, "", "")
		if updated == notice {
			break
		}
		notice = updated
	}
	headChars, tailChars := HeadTail(max(available-len(notice), 0))
	headChars = min(headChars, len(head))
	kept := ""
	if tailChars > 0 {
		kept = tail[len(tail)-min(tailChars, len(tail)):]
	}
	return head[:headChars] + notice[:min(len(notice), available)] + kept + suffix
}

// Closes the spill file, its path when it was written
func (c *collector) closeSpill() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spill == nil {
		return ""
	}
	c.spill.Close()
	c.spill = nil
	return c.spillPath
}

// Result of a finished wait, with the spill file when the output overflowed
func (c *collector) result(output string, code int) *Result {
	result := &Result{Output: output, Code: code}
	if spill := c.closeSpill(); spill != "" {
		result.Total, result.Spilled = c.count(), spill
	}
	return result
}

// Incremental UTF-8 decoding, an incomplete trailing sequence kept for the next chunk
type decoder struct {
	pending []byte
}

func (d *decoder) decode(chunk []byte) string {
	data := chunk
	if len(d.pending) > 0 {
		data, d.pending = append(d.pending, chunk...), nil
	}
	var b strings.Builder
	b.Grow(len(data))
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(data[i:]) {
				d.pending = append([]byte{}, data[i:]...)
				return b.String()
			}
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.Write(data[i : i+size])
		i += size
	}
	return b.String()
}

// Replacement characters for a dangling incomplete sequence
func (d *decoder) finish() string {
	n := len(d.pending)
	d.pending = nil
	return strings.Repeat(string(utf8.RuneError), n)
}

// Goroutine reading a process's output into a collector
type drain struct {
	done chan struct{}
}

func (d *drain) join(timeout time.Duration) {
	select {
	case <-d.done:
	case <-time.After(timeout):
	}
}

// Reads output until it closes, stop is set, or about 300ms after the process exited with nothing more coming
func startDrain(p Proc, output *collector, stop *atomic.Bool) *drain {
	d := &drain{done: make(chan struct{})}
	go func() {
		defer close(d.done)
		stream := p.Output()
		if stream == nil {
			return
		}
		dec := &decoder{}
		defer func() { output.add(dec.finish()) }()
		buf := make([]byte, 4096)
		idle := 0
		for stop == nil || !stop.Load() {
			n, ready, err := readChunk(stream, buf, 100*time.Millisecond)
			if err != nil {
				return
			}
			if ready {
				if n == 0 {
					return
				}
				output.add(dec.decode(buf[:n]))
				idle = 0
				continue
			}
			if _, exited := p.Poll(); exited {
				if idle++; idle >= 3 {
					return
				}
			}
		}
	}()
	return d
}
