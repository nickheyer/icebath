package jsonschemautil

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
)

const (
	// Deepest nesting of a value the validator walks into
	maxValueDepth = 10000
	// Most schemas the validator holds open at once
	maxFrames = 100000
	// Schema applications one validation makes before it gives up: bounds schemas whose references multiply the work, such as anyOf alternatives that refer to anyOf alternatives
	maxSteps = 1 << 22
	// Failures one validation holds at once before it gives up describing them
	maxLiveErrors = 1 << 16
	// Time all the pattern matches of one validation may take together, in per-match timeouts: many matches each just within the timeout still end
	matchBudget = 10
)

// Step from a value into a member: an object key, or an array position when index is not negative
type step struct {
	key   string
	index int
}

// Schema being applied and the identity of the value it applies to
type frame struct {
	node  *node
	value uint64
}

// One validation pass
type state struct {
	// Only whether the value passes matters: no error is recorded and the first failure ends the work
	quick bool
	errs  []*ValidationError
	path  []step
	// Schema keywords and member names from the root schema to the keyword being applied
	kw     []string
	scope  []*unit
	frames []frame
	node   *node
	// Length of kw when the current node was entered
	kwBase int
	// Identity of the value being validated and the last identity handed out
	cur, seq uint64
	// Schema applications made so far
	steps int
	// Time spent matching patterns so far
	matching time.Duration
	// Why the validation could not finish, once it cannot: the value then fails whatever the keywords around the cause would make of it
	halt *ValidationError
}

var statePool = sync.Pool{New: func() any { return new(state) }}

func newState() *state { return statePool.Get().(*state) }

func (st *state) release() {
	clear(st.path[:cap(st.path)])
	clear(st.kw[:cap(st.kw)])
	clear(st.scope[:cap(st.scope)])
	clear(st.frames[:cap(st.frames)])
	*st = state{path: st.path[:0], kw: st.kw[:0], scope: st.scope[:0], frames: st.frames[:0]}
	statePool.Put(st)
}

// Applies a schema to a value, recording errors unless quick and the members its keywords evaluate into ev when ev is not nil
func (st *state) validate(n *node, v any, ev *evaluated) bool {
	if st.halt != nil {
		return false
	}
	if st.steps++; st.steps > maxSteps {
		st.abort("", "validation stopped: the schema takes more than "+strconv.Itoa(maxSteps)+" steps on this value", v)
		return false
	}
	switch n.boolean {
	case boolTrue:
		return true
	case boolFalse:
		if !st.quick && st.room() {
			st.errs = append(st.errs, &ValidationError{Message: "false schema does not allow " + quoteValue(v),
				InstancePath: st.instancePath(), SchemaPath: st.schemaPath(), SchemaURI: n.location, value: v})
		}
		return false
	}
	v = plain(v)
	if len(st.path) > maxValueDepth || len(st.frames) >= maxFrames {
		st.stop(n, v, "the value nests too deeply to validate")
		return false
	}
	for i := len(st.frames) - 1; i >= 0 && st.frames[i].value == st.cur; i-- {
		if st.frames[i].node == n {
			st.stop(n, v, "the schema applies itself to the same value again and would never finish")
			return false
		}
	}
	st.frames = append(st.frames, frame{node: n, value: st.cur})
	entered := len(st.scope) == 0 || st.scope[len(st.scope)-1] != n.unit
	if entered {
		st.scope = append(st.scope, n.unit)
	}
	node, base := st.node, st.kwBase
	st.node, st.kwBase = n, len(st.kw)
	if ev == nil && n.tracks {
		ev = &evaluated{}
	}
	ok := true
	for i := range n.checks {
		c := &n.checks[i]
		st.kw = append(st.kw, c.keyword)
		passed := c.fn(st, v, ev)
		st.kw = st.kw[:len(st.kw)-1]
		if st.halt != nil {
			ok = false
			break
		}
		if !passed {
			ok = false
			if st.quick {
				break
			}
		}
	}
	st.node, st.kwBase = node, base
	if entered {
		st.scope = st.scope[:len(st.scope)-1]
	}
	st.frames = st.frames[:len(st.frames)-1]
	return ok
}

// Records an error for the current keyword
func (st *state) fail(keyword, message string, v any, causes []*ValidationError) {
	if st.quick || !st.room() {
		return
	}
	st.errs = append(st.errs, &ValidationError{Message: message, Keyword: keyword, InstancePath: st.instancePath(),
		SchemaPath: st.schemaPath(), SchemaURI: st.schemaURI(), Causes: causes, value: v, node: st.node})
}

// Whether another error may be recorded: the validation has not stopped, and it stops here once maxLiveErrors are held
func (st *state) room() bool {
	if st.halt != nil {
		return false
	}
	if len(st.errs) >= maxLiveErrors {
		st.abort("", "validation stopped: the value fails in more than "+strconv.Itoa(maxLiveErrors)+" places", nil)
		return false
	}
	return true
}

// Ends the validation as failed for the reason given at the current keyword, quick or not; the first reason is kept
func (st *state) abort(keyword, message string, v any) {
	if st.halt != nil {
		return
	}
	st.halt = &ValidationError{Message: message, Keyword: keyword, InstancePath: st.instancePath(),
		SchemaPath: st.schemaPath(), SchemaURI: st.schemaURI(), value: v, node: st.node}
}

// Whether a pattern matches s, false once the validation has stopped; a match that runs out of time stops it, naming the pattern and the value, and so do matches that together take matchBudget timeouts
func (st *state) matches(re *regexp2.Regexp, keyword, s string) bool {
	if st.halt != nil {
		return false
	}
	bounded := re.MatchTimeout > 0 && re.MatchTimeout <= regexp2.DefaultMatchTimeout/matchBudget
	var start time.Time
	if bounded {
		start = time.Now()
	}
	matched, err := re.MatchString(s)
	if err != nil {
		st.abort(keyword, quote(s, instanceQuoteLimit)+" could not be matched against the pattern "+quote(re.String(), schemaQuoteLimit)+
			" in the time allowed", s)
		return false
	}
	if bounded {
		if st.matching += time.Since(start); st.matching > matchBudget*re.MatchTimeout {
			st.abort(keyword, "validation stopped: matching patterns took more than "+(matchBudget*re.MatchTimeout).String()+" in all", s)
			return false
		}
	}
	return matched
}

// Errors of a finished validation: those recorded, or only the reason it stopped
func (st *state) result() []*ValidationError {
	if st.halt != nil {
		return []*ValidationError{st.halt}
	}
	return st.errs
}

// Ends the validation as failed at a schema that cannot be applied at all, quick or not, so that no not or anyOf around it reads the failure as an answer
func (st *state) stop(n *node, v any, message string) {
	node, base := st.node, st.kwBase
	st.node, st.kwBase = n, len(st.kw)
	st.abort("", message, v)
	st.node, st.kwBase = node, base
}

// Errors recorded from start on, taken out of the list
func (st *state) takeErrors(start int) []*ValidationError {
	if start >= len(st.errs) {
		return nil
	}
	out := append([]*ValidationError(nil), st.errs[start:]...)
	st.drop(start)
	return out
}

// Drops the errors recorded from start on
func (st *state) drop(start int) {
	if start < len(st.errs) {
		clear(st.errs[start:])
		st.errs = st.errs[:start]
	}
}

func (st *state) instancePath() []any {
	out := make([]any, len(st.path))
	for i, s := range st.path {
		if s.index >= 0 {
			out[i] = s.index
		} else {
			out[i] = s.key
		}
	}
	return out
}

func (st *state) schemaPath() string {
	if len(st.kw) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range st.kw {
		b.WriteByte('/')
		writeToken(&b, t)
	}
	return b.String()
}

func (st *state) schemaURI() string {
	if st.node == nil {
		return ""
	}
	tokens := st.kw[min(st.kwBase, len(st.kw)):]
	if len(tokens) == 0 {
		return st.node.location
	}
	var b strings.Builder
	b.WriteString(st.node.location)
	for _, t := range tokens {
		b.WriteByte('/')
		writeToken(&b, t)
	}
	return b.String()
}

func (st *state) push(token string) { st.kw = append(st.kw, token) }

func (st *state) pop() { st.kw = st.kw[:len(st.kw)-1] }

// Replaces the keyword the current check runs under, giving back the one it replaced
func (st *state) rename(token string) string {
	i := len(st.kw) - 1
	old := st.kw[i]
	st.kw[i] = token
	return old
}

// Applies a subschema to a member of the value
func (st *state) member(n *node, v any, s step) bool {
	st.path = append(st.path, s)
	prev := st.cur
	st.seq++
	st.cur = st.seq
	ok := st.validate(n, v, nil)
	st.cur = prev
	st.path = st.path[:len(st.path)-1]
	return ok
}

// Whether a subschema passes on a member of the value, recording no errors
func (st *state) memberPasses(n *node, v any, s step) bool {
	quick := st.quick
	st.quick = true
	ok := st.member(n, v, s)
	st.quick = quick
	return ok
}

// Applies a subschema to a value derived from the current one at the same place: a property name, decoded content
func (st *state) derived(n *node, v any) bool {
	prev := st.cur
	st.seq++
	st.cur = st.seq
	ok := st.validate(n, v, nil)
	st.cur = prev
	return ok
}

// Applies a subschema to the same value, its evaluated members kept in ev only when it passes
func (st *state) inPlace(n *node, v any, ev *evaluated) bool {
	if ev == nil {
		return st.validate(n, v, nil)
	}
	child := &evaluated{}
	ok := st.validate(n, v, child)
	if ok {
		ev.merge(child)
	}
	return ok
}

// Whether a subschema passes on the same value, recording no errors and keeping its evaluated members in ev when it passes
func (st *state) passes(n *node, v any, ev *evaluated) bool {
	quick := st.quick
	st.quick = true
	ok := st.inPlace(n, v, ev)
	st.quick = quick
	return ok
}

// Whether a subschema passes on the same value, recording no errors and writing what it evaluates into ev as it goes
func (st *state) test(n *node, v any, ev *evaluated) bool {
	quick := st.quick
	st.quick = true
	ok := st.validate(n, v, ev)
	st.quick = quick
	return ok
}

// Target of a dynamic reference: the schema of the outermost resource in the dynamic scope that names the anchor, else the static target
func (st *state) dynamicTarget(static *node, name string) *node {
	for _, u := range st.scope {
		if n := u.dynamic[name]; n != nil {
			return n
		}
	}
	return static
}

// Target of $recursiveRef: the root of the outermost resource reached through resources whose roots all hold "$recursiveAnchor": true, else the static target
func (st *state) recursiveTarget(static *node) *node {
	target := static
	for i := len(st.scope) - 2; i >= 0; i-- {
		u := st.scope[i]
		if !u.res.recursive || u.root == nil {
			break
		}
		target = u.root
	}
	return target
}

// Members of a value that keywords applied so far have evaluated, for unevaluatedProperties and unevaluatedItems
type evaluated struct {
	props    map[string]struct{}
	allProps bool
	// Count of leading items evaluated
	items    int
	allItems bool
	itemSet  map[int]struct{}
}

func freshEvaluated(ev *evaluated) *evaluated {
	if ev == nil {
		return nil
	}
	return &evaluated{}
}

func (e *evaluated) prop(k string) {
	if e.allProps {
		return
	}
	if e.props == nil {
		e.props = map[string]struct{}{}
	}
	e.props[k] = struct{}{}
}

func (e *evaluated) hasProp(k string) bool {
	if e.allProps {
		return true
	}
	_, ok := e.props[k]
	return ok
}

func (e *evaluated) leading(n int) { e.items = max(e.items, n) }

func (e *evaluated) item(i int) {
	if e.allItems || i < e.items {
		return
	}
	if e.itemSet == nil {
		e.itemSet = map[int]struct{}{}
	}
	e.itemSet[i] = struct{}{}
}

func (e *evaluated) hasItem(i int) bool {
	if e.allItems || i < e.items {
		return true
	}
	_, ok := e.itemSet[i]
	return ok
}

func (e *evaluated) merge(o *evaluated) {
	switch {
	case o.allProps:
		e.allProps, e.props = true, nil
	case !e.allProps:
		for k := range o.props {
			e.prop(k)
		}
	}
	e.allItems = e.allItems || o.allItems
	e.leading(o.items)
	for i := range o.itemSet {
		e.item(i)
	}
}
