package jsonschemautil

import (
	"errors"
	"strconv"
	"strings"
)

// Reported, wrapped in a *SchemaError, for a reference to a document that is neither in the schema, bundled, given with WithResource nor served by a loader, or to a place or anchor its document lacks
var ErrUnresolvable = errors.New("unresolvable reference")

// Wrapped by a Loader for a URI it does not serve, so the next loader is asked
var ErrUnsupportedURI = errors.New("unsupported URI")

// Failure of a value against a schema: what is wrong, where in the value, and which keyword at which place in the schema
type ValidationError struct {
	// What is wrong, in plain words with values quoted as JSON
	Message string
	// Keyword that failed, empty for a false schema or a validation that cannot finish for a reason other than a pattern match running out of time
	Keyword string
	// Steps from the root of the value to the failing part: property names as string, array positions as int
	InstancePath []any
	// JSON pointer from the root schema to the failing keyword along the path evaluation took, references included
	SchemaPath string
	// Absolute location of the failing keyword: the URI of its schema resource with a JSON pointer fragment, written as the pointer reads, without percent-encoding
	SchemaURI string
	// Failures of the subschemas behind this one, for anyOf, oneOf and the keywords that summarize several failures
	Causes []*ValidationError
	value  any
	node   *node
}

// The message, preceded by where in the value it applies and followed by the schema keyword that failed
func (e *ValidationError) Error() string {
	var b strings.Builder
	if len(e.InstancePath) > 0 {
		b.WriteString("at ")
		b.WriteString(e.Location())
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.SchemaPath != "" {
		b.WriteString(" (schema ")
		b.WriteString(e.SchemaPath)
		b.WriteByte(')')
	}
	return b.String()
}

// JSON pointer of the failing part of the value, empty for the value itself
func (e *ValidationError) Location() string { return pointer(e.InstancePath) }

// Schema that cannot be compiled: it breaks its metaschema, a reference cannot be resolved, or a keyword holds a value it cannot use
type SchemaError struct {
	// Where the problem is: a URI with a JSON pointer fragment written without percent-encoding, # alone for the root of the schema
	Location string
	// What is wrong
	Message string
	// The metaschema rule the schema breaks, its InstancePath leading into the schema; nil for other problems
	Violation *ValidationError
	// Underlying cause, such as ErrUnresolvable, a loader failure or a pattern syntax error; nil when there is none
	Err error
}

// The problem with its location in the schema
func (e *SchemaError) Error() string {
	if e.Location == "" || e.Location == "#" {
		return "invalid schema: " + e.Message
	}
	return "invalid schema at " + e.Location + ": " + e.Message
}

// The underlying cause; the metaschema violation is kept out of the chain so it is never taken for a failure of the value
func (e *SchemaError) Unwrap() error { return e.Err }

// JSON pointer of a path of property names and array positions
func pointer(path []any) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range path {
		b.WriteByte('/')
		switch x := p.(type) {
		case int:
			b.WriteString(strconv.Itoa(x))
		case string:
			writeToken(&b, x)
		}
	}
	return b.String()
}

// Writes a JSON pointer reference token with ~ and / escaped
func writeToken(b *strings.Builder, token string) {
	for i := range len(token) {
		switch c := token[i]; c {
		case '~':
			b.WriteString("~0")
		case '/':
			b.WriteString("~1")
		default:
			b.WriteByte(c)
		}
	}
}

// JSON pointer reference token of a key, ~ and / escaped
func escapeToken(token string) string {
	if !strings.ContainsAny(token, "~/") {
		return token
	}
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// Key a JSON pointer reference token names
func unescapeToken(token string) string {
	if !strings.Contains(token, "~") {
		return token
	}
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}
