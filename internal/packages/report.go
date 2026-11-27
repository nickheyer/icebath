package packages

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/term"
)

// Where an operation tells the person what it does
type Reporter interface {
	// A line of what happened
	Line(text string)
	// What goes on now, shown in place of the last status until the next line; nobody sees it off a terminal
	Status(text string)
	// Whether a terminal shows the statuses, and how wide it is
	Terminal() (bool, int)
}

// Reporter that says nothing, for features installing on their own
type quiet struct{}

func (quiet) Line(string)           {}
func (quiet) Status(string)         {}
func (quiet) Terminal() (bool, int) { return false, 0 }

// The reporter, one saying nothing for none
func reporter(r Reporter) Reporter {
	if r == nil {
		return quiet{}
	}
	return r
}

// Download, unpack and check of one package as the person watches: redrawn in place on a terminal, a line every 4 MiB off one
type progress struct {
	r        Reporter
	name     string
	last     int64
	lastTime time.Time
	// Something was fetched, so the install says when it is done
	fetched bool
}

func newProgress(r Reporter, name string) *progress {
	return &progress{r: reporter(r), name: name}
}

func (p *progress) show(line string) {
	if live, _ := p.r.Terminal(); live {
		p.r.Status(line)
		return
	}
	p.r.Line(line)
}

// One step of the install: download with its bytes, unpack or verify with the archive it is on
func (p *progress) report(stage string, done, total int64, label string) {
	p.fetched = true
	switch stage {
	case "unpack":
		p.last, p.lastTime = 0, time.Time{}
		suffix := ""
		if label != "" {
			suffix = " " + label
		}
		p.show(fmt.Sprintf("  %s: unpacking%s", p.name, suffix))
		return
	case "verify":
		return
	}
	if total <= 0 {
		return
	}
	if done < total {
		if live, _ := p.r.Terminal(); live {
			if now := time.Now(); !p.lastTime.IsZero() && now.Sub(p.lastTime) < 100*time.Millisecond {
				return
			} else {
				p.lastTime = now
			}
		} else if done-p.last < 4<<20 {
			return
		}
	}
	p.last = done
	p.show(fmt.Sprintf("  %s: %5.1f%%  %s / %s", p.name, float64(done)/float64(total)*100, mib(done), mib(total)))
}

// Clears the progress so the verdict line starts clean
func (p *progress) finish() {
	if live, _ := p.r.Terminal(); live {
		p.r.Status("")
	}
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// Lines a failed child's output keeps for its report
const tailLines = 80

// A child's output held behind one status line, its last lines printed when it fails
type liveTail struct {
	r       Reporter
	label   string
	indent  string
	live    bool
	width   int
	tail    []string
	partial string
	drawn   time.Time
}

// Opens the status of a labelled step, an empty label running silently until a failure
func newLiveTail(r Reporter, label, indent string) *liveTail {
	r = reporter(r)
	t := &liveTail{r: r, label: label, indent: indent}
	t.live, t.width = r.Terminal()
	t.live = t.live && label != ""
	switch {
	case t.live:
		t.status(fmt.Sprintf("%s→ %s…", indent, label))
	case label != "":
		r.Line(fmt.Sprintf("%s→ %s…", indent, label))
	}
	return t
}

// Takes output text, carriage returns ending lines too
func (t *liveTail) write(text string) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(t.partial+text, "\n")
	t.partial = strs.Tail(lines[len(lines)-1], 4096)
	for _, line := range lines[:len(lines)-1] {
		t.line(line)
	}
}

func (t *liveTail) line(line string) {
	line = strings.TrimRightFunc(term.StripANSI(strs.Tail(line, 4096)), unicode.IsSpace)
	if strings.TrimSpace(line) == "" {
		return
	}
	t.tail = append(t.tail, line)
	if len(t.tail) > tailLines {
		t.tail = t.tail[len(t.tail)-tailLines:]
	}
	if !t.live || time.Since(t.drawn) < 50*time.Millisecond {
		return
	}
	t.drawn = time.Now()
	t.status(fmt.Sprintf("%s→ %s… %s", t.indent, t.label, strings.TrimSpace(line)))
}

func (t *liveTail) status(text string) {
	width := max(20, t.width-1)
	if r := []rune(text); len(r) > width {
		text = string(r[:width])
	}
	t.r.Status(text)
}

// Clears the status without a verdict, for a step run again
func (t *liveTail) discard() {
	t.flush()
	if t.live {
		t.r.Status("")
	}
}

// Ends the status with a mark of success, or of failure with the output's last lines
func (t *liveTail) close(ok bool) {
	t.discard()
	if ok {
		if t.label != "" {
			t.r.Line(fmt.Sprintf("%s✓ %s", t.indent, t.label))
		}
		return
	}
	name := cmp.Or(t.label, "command")
	if len(t.tail) == 0 {
		t.r.Line(fmt.Sprintf("%s✗ %s failed", t.indent, name))
		return
	}
	t.r.Line(fmt.Sprintf("%s✗ %s failed; last %d lines of output:", t.indent, name, len(t.tail)))
	for _, line := range t.tail {
		t.r.Line(t.indent + "    " + line)
	}
}

func (t *liveTail) flush() {
	if t.partial != "" {
		t.line(t.partial)
		t.partial = ""
	}
}

// Whether children stream their output as it is, forced by ICEBATH_VERBOSE or implied by CI
func verbose() bool {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_VERBOSE"))); v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	for _, key := range []string{"CI", "GITHUB_ACTIONS"} {
		switch v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v {
		case "", "0", "false", "no", "off":
		default:
			return true
		}
	}
	return false
}
