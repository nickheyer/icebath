package pydantic

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Version of the pinned pydantic release, the path segment of every error documentation link
const docsVersion = "2.14"

// One problem a validation found, the type loc msg and input of a pydantic error entry
type ErrorDetails struct {
	Type  string
	Loc   []any
	Msg   string
	Input any
}

// Location joined with dots, string parts as text and indexes as decimals
func (e ErrorDetails) LocText() string {
	parts := make([]string, len(e.Loc))
	for k, p := range e.Loc {
		parts[k] = pyrepr.Str(p)
	}
	return strings.Join(parts, ".")
}

// Every problem one validation found, titled with the model that rejected the value
type ValidationError struct {
	Title  string
	Issues []ErrorDetails
}

// The problems in validation order
func (e *ValidationError) Errors() []ErrorDetails { return e.Issues }

// Repr of an input as pydantic-core prints it, the middle elided past fifty characters
func truncatedInputRepr(v any) string {
	runes := []rune(pyrepr.Repr(v))
	if len(runes) <= 50 {
		return string(runes)
	}
	return string(runes[:25]) + "..." + string(runes[len(runes)-24:])
}

// Text pydantic renders for the error, one block per problem with its location line when it has one
func (e *ValidationError) Error() string {
	plural := "s"
	if len(e.Issues) == 1 {
		plural = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation error%s for %s", len(e.Issues), plural, e.Title)
	for _, issue := range e.Issues {
		b.WriteString("\n")
		if len(issue.Loc) > 0 {
			b.WriteString(issue.LocText() + "\n")
		}
		fmt.Fprintf(&b, "  %s [type=%s, input_value=%s, input_type=%s]\n    For further information visit https://errors.pydantic.dev/%s/v/%s",
			issue.Msg, issue.Type, truncatedInputRepr(issue.Input), pyrepr.TypeName(issue.Input), docsVersion, issue.Type)
	}
	return b.String()
}

// One problem with an empty location
func fail(typ, msg string, input any) []ErrorDetails {
	return []ErrorDetails{{Type: typ, Loc: []any{}, Msg: msg, Input: input}}
}

// The problems with key prepended to each location
func under(key any, errs []ErrorDetails) []ErrorDetails {
	out := make([]ErrorDetails, len(errs))
	for i, e := range errs {
		loc := make([]any, 0, len(e.Loc)+1)
		e.Loc = append(append(loc, key), e.Loc...)
		out[i] = e
	}
	return out
}

// Values as pydantic lists expected choices, reprs joined with commas and a final or
func expectedText(reprs []string) string {
	if len(reprs) <= 1 {
		return strings.Join(reprs, "")
	}
	return strings.Join(reprs[:len(reprs)-1], ", ") + " or " + reprs[len(reprs)-1]
}

// Plural suffix for a count in a pydantic length message
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
