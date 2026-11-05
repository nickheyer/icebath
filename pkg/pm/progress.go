package pm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
)

// TailLines is how many output lines a failure prints
const TailLines = 80

// MaxLine bounds one unterminated line, its end being the informative part
const MaxLine = 4096

const redrawInterval = 50 * time.Millisecond

var (
	verboseTrue  = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	verboseFalse = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

// Reports whether child output streams unchanged, forced by ICEBATH_VERBOSE or implied by CI
func VerboseOutput(env map[string]string) bool {
	get := os.Getenv
	if env != nil {
		get = func(k string) string { return env[k] }
	}
	explicit := strings.ToLower(pyStrip(get("ICEBATH_VERBOSE")))
	if verboseTrue[explicit] {
		return true
	}
	if verboseFalse[explicit] {
		return false
	}
	for _, key := range []string{"CI", "GITHUB_ACTIONS"} {
		v := strings.ToLower(pyStrip(get(key)))
		if v != "" && !verboseFalse[v] {
			return true
		}
	}
	return false
}

// Reports whether a stream is an interactive terminal
func isTerminalStream(stream io.Writer) bool {
	f, ok := stream.(*os.File)
	return ok && f != nil && term.IsTerminal(f.Fd())
}

// Terminal width the way shutil get_terminal_size reports it, COLUMNS first then stdout, else 80
func terminalColumns() int {
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 0 {
		return v
	}
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return 80
}

// LiveTail contains a child's output behind one status line and prints the tail on failure
type LiveTail struct {
	Label    *string
	Stream   io.Writer
	Hide     func(string) bool
	Indent   string
	Live     bool
	Tail     []string
	partial  string
	pending  []byte
	drawn    int
	lastDraw time.Time
}

// Opens a status line for label, a nil label running silently until a failure
func NewLiveTail(label *string, stream io.Writer, hide func(string) bool, indent string) *LiveTail {
	if stream == nil {
		stream = os.Stdout
	}
	t := &LiveTail{Label: label, Stream: stream, Hide: hide, Indent: indent}
	t.Live = label != nil && isTerminalStream(stream)
	if label != nil {
		if t.Live {
			t.draw(fmt.Sprintf("%s→ %s…", indent, *label))
		} else {
			t.emit(fmt.Sprintf("%s→ %s…\n", indent, *label))
		}
	}
	return t
}

// Takes child output bytes decoded like a text pipe, a split character waiting for its remaining bytes
func (t *LiveTail) Write(p []byte) (int, error) {
	text, rest := decodeUTF8Replace(append(t.pending, p...), false)
	t.pending = rest
	t.WriteString(text)
	return len(p), nil
}

// Ends the byte stream, an unfinished character decoding as one replacement character
func (t *LiveTail) flushPending() {
	if len(t.pending) > 0 {
		text, _ := decodeUTF8Replace(t.pending, true)
		t.pending = nil
		t.WriteString(text)
	}
}

// Takes child output text, bare carriage returns counting as line ends
func (t *LiveTail) WriteString(text string) {
	text = universalNewlines(text)
	first := strings.IndexByte(text, '\n')
	if first < 0 {
		if text != "" && utf8.RuneStart(text[0]) {
			if kept := lastRunes(text, MaxLine); len(kept) < len(text) {
				t.partial = kept
				return
			}
		}
		t.partial = lastRunes(t.partial+text, MaxLine)
		return
	}
	last := strings.LastIndexByte(text, '\n')
	head := t.partial + text[:first]
	t.partial = strings.Clone(lastRunes(text[last+1:], MaxLine))
	t.line(head)
	for rest := text[first+1 : last+1]; rest != ""; {
		cut := strings.IndexByte(rest, '\n')
		t.line(rest[:cut])
		rest = rest[cut+1:]
	}
}

// Clears the status line without a verdict, for a step that will be retried
func (t *LiveTail) Discard() {
	t.flushPending()
	if t.partial != "" {
		t.line(t.partial)
		t.partial = ""
	}
	if t.Live {
		t.draw("")
		t.emit("\r")
	}
}

// Ends the status line with a success mark, or a failure mark and the captured tail
func (t *LiveTail) Close(ok bool) {
	t.flushPending()
	if t.partial != "" {
		t.line(t.partial)
		t.partial = ""
	}
	if t.Live {
		t.draw("")
		t.emit("\r")
	}
	if ok {
		if t.Label != nil {
			t.emit(fmt.Sprintf("%s✓ %s\n", t.Indent, *t.Label))
		}
		return
	}
	name := "command"
	if t.Label != nil && *t.Label != "" {
		name = *t.Label
	}
	if len(t.Tail) == 0 {
		t.emit(fmt.Sprintf("%s✗ %s failed\n", t.Indent, name))
		return
	}
	t.emit(fmt.Sprintf("%s✗ %s failed; last %d lines of output:\n", t.Indent, name, len(t.Tail)))
	var b strings.Builder
	for _, line := range t.Tail {
		fmt.Fprintf(&b, "%s    %s\n", t.Indent, line)
	}
	t.emit(b.String())
}

// Records one output line in the tail and redraws the status line at most every interval
func (t *LiveTail) line(line string) {
	line = strings.TrimRightFunc(ansiEscape.ReplaceAllString(lastRunes(line, MaxLine), ""), pyIsSpace)
	if pyStrip(line) == "" {
		return
	}
	t.Tail = append(t.Tail, line)
	if len(t.Tail) > TailLines {
		t.Tail = t.Tail[len(t.Tail)-TailLines:]
	}
	if !t.Live || (t.Hide != nil && t.Hide(line)) {
		return
	}
	now := time.Now()
	if now.Sub(t.lastDraw) < redrawInterval {
		return
	}
	t.lastDraw = now
	t.draw(fmt.Sprintf("%s→ %s… %s", t.Indent, *t.Label, pyStrip(line)))
}

// Rewrites the status line, padding over the previous text instead of an ANSI erase
func (t *LiveTail) draw(text string) {
	width := max(20, terminalColumns()-1)
	r := []rune(text)
	if len(r) > width {
		r = r[:width]
	}
	t.emit("\r" + string(r) + strings.Repeat(" ", max(0, t.drawn-len(r))))
	t.drawn = len(r)
}

// Writes and flushes text on the stream
func (t *LiveTail) emit(text string) {
	io.WriteString(t.Stream, text)
	if f, ok := t.Stream.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

// Last n runes of a string
func lastRunes(s string, n int) string { return lastChars(s, n) }

// CalledProcessError is a contained child that exited nonzero, with the output it printed
type CalledProcessError struct {
	Argv   []string
	Code   int
	Output string
}

func (e *CalledProcessError) Error() string {
	command := pyListRepr(e.Argv)
	if e.Code < 0 {
		if name := signalName(-e.Code); name != "" {
			return fmt.Sprintf("Command '%s' died with <Signals.%s: %d>.", command, name, -e.Code)
		}
		return fmt.Sprintf("Command '%s' died with unknown signal %d.", command, -e.Code)
	}
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", command, e.Code)
}

// Exit status as Python reports it, the negated signal number for a child a signal killed
func pyReturnCode(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -int(status.Signal())
	}
	return exitErr.ExitCode()
}

// Reports Python str isspace, the Unicode whitespace plus the ASCII separators 0x1c through 0x1f
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Trims both ends the way Python str strip does with no argument
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// Translates CRLF and lone CR to LF the way a universal newlines text stream reads them
func universalNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Length a well formed sequence led by this byte needs, with the range its second byte must fall in
func utf8Lead(lead byte) (int, byte, byte) {
	switch {
	case lead < 0x80:
		return 1, 0, 0
	case lead >= 0xC2 && lead <= 0xDF:
		return 2, 0x80, 0xBF
	case lead == 0xE0:
		return 3, 0xA0, 0xBF
	case lead == 0xED:
		return 3, 0x80, 0x9F
	case lead >= 0xE1 && lead <= 0xEF:
		return 3, 0x80, 0xBF
	case lead == 0xF0:
		return 4, 0x90, 0xBF
	case lead == 0xF4:
		return 4, 0x80, 0x8F
	case lead >= 0xF1 && lead <= 0xF3:
		return 4, 0x80, 0xBF
	}
	return 0, 0, 0
}

// Decodes like Python utf-8 errors replace, one U+FFFD per maximal invalid subpart, keeping a cut tail unless final
func decodeUTF8Replace(data []byte, final bool) (string, []byte) {
	if utf8.Valid(data) {
		return string(data), nil
	}
	out, used := appendUTF8Replace(nil, data, final)
	if used < len(data) {
		return string(out), append([]byte{}, data[used:]...)
	}
	return string(out), nil
}

// Appends the Python utf-8 errors replace decoding of data to dst and returns how many bytes it consumed
func appendUTF8Replace(dst, data []byte, final bool) ([]byte, int) {
	if utf8.Valid(data) {
		return append(dst, data...), len(data)
	}
	for i := 0; i < len(data); {
		need, lo, hi := utf8Lead(data[i])
		if need == 1 {
			dst = append(dst, data[i])
			i++
			continue
		}
		n := 1
		for need > 0 && n < need && i+n < len(data) {
			c := data[i+n]
			if (n == 1 && (c < lo || c > hi)) || (n > 1 && (c < 0x80 || c > 0xBF)) {
				break
			}
			n++
		}
		if need > 0 && n == need {
			dst = append(dst, data[i:i+n]...)
			i += n
			continue
		}
		if need > 0 && i+n == len(data) && !final {
			return dst, i
		}
		dst = utf8.AppendRune(dst, utf8.RuneError)
		i += n
	}
	return dst, len(data)
}

// Python repr of a list of strings
func pyListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// ContainOptions shape a contained child run
type ContainOptions struct {
	Stream io.Writer
	Hide   func(string) bool
	Indent string
}

// Runs a prepared child under the output policy and returns its captured output, failing on nonzero exit
func RunContained(cmd *exec.Cmd, label string, opts ContainOptions) (string, error) {
	argv := append([]string{}, cmd.Args...)
	if VerboseOutput(nil) {
		out := opts.Stream
		if out == nil {
			out = os.Stdout
		}
		fmt.Fprintf(out, "%s→ %s…\n", opts.Indent, label)
		if cmd.Stdout == nil {
			cmd.Stdout = os.Stdout
		}
		if cmd.Stderr == nil {
			cmd.Stderr = os.Stderr
		}
		return "", calledProcess(argv, cmd.Run(), "")
	}
	tail := NewLiveTail(&label, opts.Stream, opts.Hide, opts.Indent)
	if !tail.Live {
		out, err := cmd.CombinedOutput()
		decoded, _ := decodeUTF8Replace(out, true)
		text := universalNewlines(decoded)
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				tail.WriteString(text)
			}
			tail.Close(false)
			return text, calledProcess(argv, err, text)
		}
		tail.Close(true)
		return text, nil
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		tail.Close(false)
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		tail.Close(false)
		return "", err
	}
	reader := bufio.NewReader(pipe)
	for {
		chunk, rerr := reader.ReadString('\n')
		if chunk != "" {
			decoded, _ := decodeUTF8Replace([]byte(chunk), true)
			tail.WriteString(decoded)
		}
		if rerr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		tail.Close(false)
		return "", waitErr
	}
	tail.Close(waitErr == nil)
	output := strings.Join(tail.Tail, "\n")
	return output, calledProcess(argv, waitErr, output)
}

// Turns an exit error into a called process error, passing start failures through
func calledProcess(argv []string, err error, output string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &CalledProcessError{Argv: argv, Code: pyReturnCode(exitErr), Output: output}
	}
	return err
}
