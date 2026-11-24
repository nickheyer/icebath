package pythonutil

import (
	"strconv"
	"strings"
)

// Category of a syntax error, told apart the way Python reports it
type ErrorKind uint8

const (
	// Source that breaks the grammar or a compile time rule
	KindSyntax ErrorKind = iota
	// An unexpected, missing or unmatched indent
	KindIndentation
	// Tabs and spaces mixed so the indentation is ambiguous
	KindTab
)

// Words naming the kind: syntax error, indentation error or tab error
func (k ErrorKind) String() string {
	switch k {
	case KindIndentation:
		return "indentation error"
	case KindTab:
		return "tab error"
	}
	return "syntax error"
}

// Python source the parser or the compiler rejects, with Python's own message, 1-based character columns and the source line, Line zero when the error has no position
type SyntaxError struct {
	Kind                             ErrorKind
	Msg                              string
	Line, Column, EndLine, EndColumn int
	Text                             string
	Filename                         string
}

// Location and message as file:line:column: message, the parts that are not known left out
func (e *SyntaxError) Error() string {
	var b strings.Builder
	if e.Filename != "" {
		b.WriteString(e.Filename)
		b.WriteByte(':')
	}
	if e.Line > 0 {
		b.WriteString(strconv.Itoa(e.Line))
		if e.Column > 0 {
			b.WriteByte(':')
			b.WriteString(strconv.Itoa(e.Column))
		}
		b.WriteByte(':')
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Source nested deeper than the parser accepts, the guard that keeps hostile input from exhausting the stack
type LimitError struct {
	Line, Limit int
}

// Limit crossed and the line where it happened
func (e *LimitError) Error() string {
	msg := "source nests deeper than " + strconv.Itoa(e.Limit) + " levels"
	if e.Line > 0 {
		msg += " at line " + strconv.Itoa(e.Line)
	}
	return msg
}

// Expression that parses but is not a literal, or a literal whose set member or dict key cannot be one
type LiteralError struct {
	Line int
	Msg  string
}

// Message prefixed with the line of the offending expression when it is known
func (e *LiteralError) Error() string {
	if e.Line > 0 {
		return "line " + strconv.Itoa(e.Line) + ": " + e.Msg
	}
	return e.Msg
}

// Gives the error the filename it is reported against and clamps columns Python leaves negative
func (e *SyntaxError) finish(filename string) *SyntaxError {
	if e.Filename == "" {
		e.Filename = filename
	}
	if e.Line <= 0 {
		e.Line, e.Column, e.EndLine, e.EndColumn = 0, 0, 0, 0
		return e
	}
	e.Column = max(e.Column, 0)
	if e.EndLine <= 0 {
		e.EndLine = e.Line
	}
	e.EndColumn = max(e.EndColumn, 0)
	return e
}
