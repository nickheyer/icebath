package pyjsonschema

import (
	"iter"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One jsonschema ValidationError with its relative paths, context and the details iter_errors sets
type vError struct {
	message        string
	validator      any
	validatorValue any
	instance       any
	schema         any
	set            [4]bool
	path           []any
	schemaPath     []any
	context        []*vError
	parent         *vError
	typeChecker    *draft
}

func newError(message string) *vError { return &vError{message: message} }

// Error with validator, validator_value, instance and schema given up front
func newFullError(message string, validator, value, instance, schema any) *vError {
	return &vError{message: message, validator: validator, validatorValue: value, instance: instance, schema: schema, set: [4]bool{true, true, true, true}}
}

func withContext(e *vError, context []*vError) *vError {
	e.context = context
	for _, c := range context {
		c.parent = e
	}
	return e
}

// _set, filling only the details still unset
func (e *vError) setDetails(validator, value, instance, schema any, tc *draft) {
	if e.typeChecker == nil {
		e.typeChecker = tc
	}
	if !e.set[0] {
		e.validator, e.set[0] = validator, true
	}
	if !e.set[1] {
		e.validatorValue, e.set[1] = value, true
	}
	if !e.set[2] {
		e.instance, e.set[2] = instance, true
	}
	if !e.set[3] {
		e.schema, e.set[3] = schema, true
	}
}

func prepend(path []any, v any) []any { return append([]any{v}, path...) }

func (e *vError) absolutePath() []any {
	if e.parent == nil {
		return e.path
	}
	return append(append([]any(nil), e.parent.absolutePath()...), e.path...)
}

// Python text of the error, verbose once validator, value, instance and schema are all set
func (e *vError) str(schemaWord, instanceWord string) string {
	return e.strAt(nil, schemaWord, instanceWord)
}

// _Error.__str__ with its frames, format_as_index and _pretty under it
func (e *vError) strAt(st *state, schemaWord, instanceWord string) string {
	defer st.call()()
	st.touch(1)
	if e.set != [4]bool{true, true, true, true} {
		return e.message
	}
	sp := e.schemaPath
	if len(sp) > 0 {
		sp = sp[:len(sp)-1]
	}
	st.touch(2)
	return e.message + "\n\nFailed validating " + repr(e.validator) + " in " + formatAsIndex(schemaWord, sp) +
		":\n    " + prettyAt(st, e.schema) + "\n\nOn " + formatAsIndex(instanceWord, e.path) + ":\n    " + prettyAt(st, e.instance)
}

func formatAsIndex(container string, indices []any) string {
	if len(indices) == 0 {
		return container
	}
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = repr(idx)
	}
	return container + "[" + strings.Join(parts, "][") + "]"
}

// _pretty, pformat indented under a message line
func prettyAt(st *state, v any) string {
	defer st.call()()
	text := pformatAt(v, st)
	st.touch(1)
	return strings.ReplaceAll(text, "\n", "\n    ")
}

// _matches_type, whether the instance has a type its schema allows
func (e *vError) matchesType() bool {
	d, ok := e.schema.(*pyjson.Object)
	if !ok {
		return false
	}
	expected, ok := d.Vals["type"]
	if !ok {
		return false
	}
	if s, ok := expected.(string); ok {
		return e.typeChecker.checkType(e.instance, s)
	}
	for _, t := range pyIter(expected) {
		if e.typeChecker.checkType(e.instance, t) {
			return true
		}
	}
	return false
}

var weakMatches = map[string]bool{"anyOf": true, "oneOf": true}

// Relevance key of by_relevance, a tuple compared field by field
type relevanceKey struct {
	negLen      int
	path        []any
	notWeak     bool
	notMatching bool
}

func relevance(e *vError, st *state) relevanceKey {
	st.touch(1 + e.matchesTypeDepth())
	s, isStr := e.validator.(string)
	return relevanceKey{negLen: -len(e.path), path: e.path, notWeak: !isStr || !weakMatches[s], notMatching: !e.matchesType()}
}

// Frames of _matches_type and the type checks it runs
func (e *vError) matchesTypeDepth() int {
	d, ok := e.schema.(*pyjson.Object)
	if !ok {
		return 1
	}
	expected, ok := d.Vals["type"]
	if !ok {
		return 1
	}
	if s, ok := expected.(string); ok {
		return e.checkerDepth(s)
	}
	deepest := 2
	for _, t := range pyIter(expected) {
		deepest = max(deepest, 1+e.checkerDepth(t))
		if e.typeChecker.checkType(e.instance, t) {
			break
		}
	}
	return deepest
}

// Frames of _matches_type down through TypeChecker.is_type for one expected type
func (e *vError) checkerDepth(t any) int {
	switch t.(type) {
	case []any, *pyjson.Object:
		return 2
	}
	return e.typeChecker.isTypeDepth(e.instance, t)
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

// Ordering of two relevance keys the way tuples of int, deque and bools compare
func (k relevanceKey) cmp(o relevanceKey) int {
	if k.negLen != o.negLen {
		if k.negLen < o.negLen {
			return -1
		}
		return 1
	}
	if c := pathCmp(k.path, o.path); c != 0 {
		return c
	}
	if c := boolCmp(k.notWeak, o.notWeak); c != 0 {
		return c
	}
	return boolCmp(k.notMatching, o.notMatching)
}

// Deque comparison of two paths, items compared with == and then <
func pathCmp(a, b []any) int {
	i := 0
	for ; i < len(a) && i < len(b); i++ {
		if !pyEq(a[i], b[i]) {
			break
		}
	}
	if i >= len(a) || i >= len(b) {
		switch {
		case len(a) < len(b):
			return -1
		case len(a) > len(b):
			return 1
		}
		return 0
	}
	if pyLess(a[i], b[i]) {
		return -1
	}
	return 1
}

// best_match, the most relevant error descending into anyOf and oneOf context
func bestMatch(errs iter.Seq[*vError], st *state) *vError {
	defer st.call()()
	var best *vError
	var bestKey relevanceKey
	for e := range errs {
		k := relevance(e, st)
		if best == nil || k.cmp(bestKey) > 0 {
			best, bestKey = e, k
		}
	}
	if best == nil {
		return nil
	}
	for len(best.context) > 0 {
		keys := make([]relevanceKey, len(best.context))
		func() {
			defer st.call()()
			for i, c := range best.context {
				keys[i] = relevance(c, st)
			}
		}()
		first, second := -1, -1
		for i := range best.context {
			switch {
			case first < 0 || keys[i].cmp(keys[first]) < 0:
				first, second = i, first
			case second < 0 || keys[i].cmp(keys[second]) < 0:
				second = i
			}
		}
		if second >= 0 && relevance(best.context[first], st).cmp(relevance(best.context[second], st)) == 0 {
			return best
		}
		best = best.context[first]
	}
	return best
}

// Best matching jsonschema error of a validation, Error giving the str Python renders
type ValidationError struct {
	Message string
	// Keyword that failed, nil for a false schema
	Validator    any
	AbsolutePath []any
	err          *vError
}

func (e *ValidationError) Error() string { return e.err.str("schema", "instance") }

func publicError(e *vError) *ValidationError {
	return &ValidationError{Message: e.message, Validator: e.validator, AbsolutePath: e.absolutePath(), err: e}
}

// Metaschema violation check_schema raises, Error giving the str Python renders
type SchemaError struct {
	Message string
	err     *vError
}

func (e *SchemaError) Error() string { return e.err.str("metaschema", "schema") }
