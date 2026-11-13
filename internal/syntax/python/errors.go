package python

import (
	"errors"
	"fmt"
)

// Exception class the reference raises for a rejected source
type ErrorClass uint8

// Syntax error classes
const (
	ClassSyntax ErrorClass = iota
	ClassIndentation
	ClassTab
)

// Class name as the reference prints it before the message
func (c ErrorClass) String() string {
	switch c {
	case ClassIndentation:
		return "IndentationError"
	case ClassTab:
		return "TabError"
	}
	return "SyntaxError"
}

// Rejected source with the message, one based line and character column, end position and offending line
type SyntaxError struct {
	Class    ErrorClass
	Msg      string
	Filename string
	Line     int
	Col      int
	EndLine  int
	EndCol   int
	Text     string
	unplaced bool
}

// Message followed by the file name and line, which every error carries but the refusal of a null byte
func (e *SyntaxError) Error() string {
	if e.unplaced {
		return e.Msg
	}
	return fmt.Sprintf("%s (%s, line %d)", e.Msg, e.Filename, e.Line)
}

// Bound a source exceeded before a tree could be built
type LimitKind uint8

// Limit kinds
const (
	LimitParserStack LimitKind = iota
	LimitTreeDepth
)

// Most rule frames the reference parser stacks before it overflows, also bounding the chains this parser recurses through
const maxStack = 6000

// Most nodes on one root to leaf path, a module statement holding 52153 chained additions
const maxTreeDepth = 52156

// Source too deep to parse safely, Kind telling the parser stack from the tree depth bound
type LimitError struct {
	Kind LimitKind
}

// Exception name the reference raises for the exceeded bound
func (e *LimitError) Class() string {
	if e.Kind == LimitTreeDepth {
		return "RecursionError"
	}
	return "MemoryError"
}

// Message the reference prints for the exceeded bound
func (e *LimitError) Error() string {
	if e.Kind == LimitTreeDepth {
		return "Stack overflow (used 8152 kB) during compilation"
	}
	return "Parser stack overflowed - Python source too complex to parse"
}

// Failure kind of a literal evaluation
type LiteralErrorKind uint8

// Literal evaluation failures
const (
	LiteralMalformed LiteralErrorKind = iota
	LiteralUnhashable
	LiteralOverflow
)

// Expression that is not a literal, or a literal whose value cannot be built
type LiteralError struct {
	Kind LiteralErrorKind
	Msg  string
}

// Exception name the reference raises for the failure
func (e *LiteralError) Class() string {
	switch e.Kind {
	case LiteralUnhashable:
		return "TypeError"
	case LiteralOverflow:
		return "OverflowError"
	}
	return "ValueError"
}

func (e *LiteralError) Error() string { return e.Msg }

// Decimal rendering of an integer past 4300 digits, refused as the reference refuses it
var ErrDigitLimit = errors.New("Exceeds the limit (4300 digits) for integer string conversion; use sys.set_int_max_str_digits() to increase the limit")
