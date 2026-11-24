// Package ruamelyaml is a port of the ruamel.yaml 0.18.16 load stack and the libyaml 0.1.7 parser bundled in ruamel.yaml.clib 0.2.15
//
// Derived from ruamel.yaml by Anthon van der Neut (MIT license) and libyaml by Kirill Simonov and Ingy dot Net (MIT license)
package ruamelyaml

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Position in the input, with the decoded buffer when the mark can print a snippet
type Mark struct {
	Name    string
	Index   int
	Line    int
	Column  int
	Buffer  []rune
	Pointer int
	snippet bool
}

func isSnippetBreak(r rune) bool {
	return r == 0 || r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
}

// Source line around the mark with a caret under its column, empty when the mark carries no buffer
func (m *Mark) Snippet() (string, bool) {
	if !m.snippet {
		return "", false
	}
	const indent, maxLength = 4, 75
	head, start := "", m.Pointer
	for start > 0 && !isSnippetBreak(m.Buffer[start-1]) {
		start--
		if float64(m.Pointer-start) > float64(maxLength)/2-1 {
			head = " ... "
			start += 5
			break
		}
	}
	tail, end := "", m.Pointer
	for end < len(m.Buffer) && !isSnippetBreak(m.Buffer[end]) {
		end++
		if float64(end-m.Pointer) > float64(maxLength)/2-1 {
			tail = " ... "
			end -= 5
			break
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString(head)
	b.WriteString(string(pySlice(m.Buffer, start, end)))
	b.WriteString(tail)
	b.WriteByte('\n')
	b.WriteString(strings.Repeat(" ", indent+m.Pointer-start+utf8.RuneCountInString(head)))
	fmt.Fprintf(&b, "^ (line: %d)", m.Line+1)
	return b.String(), true
}

// Python slice semantics over runes, clamped and empty when start passes end
func pySlice(b []rune, start, end int) []rune {
	if start < 0 {
		start += len(b)
		if start < 0 {
			start = 0
		}
	}
	if end < 0 {
		end += len(b)
		if end < 0 {
			end = 0
		}
	}
	if end > len(b) {
		end = len(b)
	}
	if start > len(b) {
		start = len(b)
	}
	if start >= end {
		return nil
	}
	return b[start:end]
}

// Where line, a colon and the snippet when the mark has one
func (m *Mark) String() string {
	where := fmt.Sprintf("  in \"%s\", line %d, column %d", m.Name, m.Line+1, m.Column+1)
	if s, ok := m.Snippet(); ok {
		where += ":\n" + s
	}
	return where
}

// Python class name, message text and exception kind every load failure carries
type PyError interface {
	error
	PyTypeName() string
}

// ruamel MarkedYAMLError and its subclasses, the class name selecting ScannerError, ParserError and the rest
type MarkedError struct {
	Class       string
	Context     *string
	ContextMark *Mark
	Problem     *string
	ProblemMark *Mark
	Note        string
}

func strp(s string) *string { return &s }

func (e *MarkedError) PyTypeName() string { return e.Class }

// Lines of context, context mark unless it equals the problem mark, problem, problem mark and dedented note
func (e *MarkedError) Error() string {
	var lines []string
	if e.Context != nil {
		lines = append(lines, *e.Context)
	}
	if e.ContextMark != nil && (e.Problem == nil || e.ProblemMark == nil || e.ContextMark.Name != e.ProblemMark.Name ||
		e.ContextMark.Line != e.ProblemMark.Line || e.ContextMark.Column != e.ProblemMark.Column) {
		lines = append(lines, e.ContextMark.String())
	}
	if e.Problem != nil {
		lines = append(lines, *e.Problem)
	}
	if e.ProblemMark != nil {
		lines = append(lines, e.ProblemMark.String())
	}
	if e.Note != "" {
		lines = append(lines, dedent(e.Note))
	}
	return strings.Join(lines, "\n")
}

func isSpaceLine(l string) bool {
	if l == "" {
		return false
	}
	for _, r := range l {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Text with the whitespace prefix common to its non blank lines removed and blank lines emptied, as textwrap.dedent
func dedent(text string) string {
	lines := strings.Split(text, "\n")
	var l1, l2 string
	first := true
	for _, l := range lines {
		if l == "" || isSpaceLine(l) {
			continue
		}
		if first || l < l1 {
			l1 = l
		}
		if first || l > l2 {
			l2 = l
		}
		first = false
	}
	r1, r2 := []rune(l1), []rune(l2)
	margin := 0
	for i, c := range r1 {
		margin = i
		if c != r2[i] || (c != ' ' && c != '\t') {
			break
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if isSpaceLine(l) {
			continue
		}
		rs := []rune(l)
		if margin < len(rs) {
			out[i] = string(rs[margin:])
		}
	}
	return strings.Join(out, "\n")
}

// Whether the error derives from ruamel YAMLError, the class the safe loader retries with the pure parser
func IsYAMLError(err error) bool {
	switch e := err.(type) {
	case *MarkedError, *ReaderError:
		return true
	case *PyException:
		return e.YAMLError
	}
	return false
}

// ruamel ReaderError for an undecodable byte or a character outside the printable set
type ReaderError struct {
	Name      string
	Position  int
	Character int
	Encoding  string
	Reason    string
}

func (e *ReaderError) PyTypeName() string { return "ReaderError" }

func (e *ReaderError) Error() string {
	return fmt.Sprintf("unacceptable character #x%04x: %s\n  in \"%s\", position %d", e.Character, e.Reason, e.Name, e.Position)
}

// Python exception other than a YAMLError raised while loading, such as ValueError or KeyError, with its str text
type PyException struct {
	Class     string
	Msg       string
	YAMLError bool
}

func (e *PyException) PyTypeName() string { return e.Class }
func (e *PyException) Error() string      { return e.Msg }

// Python exception of a class with the given str text
func Raise(class, msg string) *PyException { return &PyException{Class: class, Msg: msg} }
