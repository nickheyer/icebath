package pyjsonschema

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Python recursion bookkeeping, the frames CPython pushes for the ported code with warm re, abc and lru caches

// Recursion limit of sys.getrecursionlimit in an interpreter that never changes it
const recursionLimit = 1000

// Per run pattern cache and the Python frame depth the run has reached
type state struct {
	regexes map[string]*regexp2.Regexp
	depth   int
}

// Run state whose caller sits callerDepth Python frames deep
func newState(callerDepth int) *state {
	return &state{regexes: map[string]*regexp2.Regexp{}, depth: callerDepth}
}

// Pushes n frames, raising the RecursionError CPython raises for a frame past the recursion limit, nil state counting nothing
func (st *state) enter(n int) {
	if st == nil {
		return
	}
	st.depth += n
	if st.depth > recursionLimit {
		raise("RecursionError", "maximum recursion depth exceeded")
	}
}

func (st *state) leave(n int) {
	if st != nil {
		st.depth -= n
	}
}

// One Python frame held for the length of the Go call, deferred leave returned
func (st *state) call() func() {
	st.enter(1)
	return func() { st.leave(1) }
}

// A helper chain n frames deep that returns before anything else runs
func (st *state) touch(n int) {
	st.enter(n)
	st.leave(n)
}

// Generator frame, on the stack while the generator runs and off it while it waits at a yield
func (st *state) gen(seq errSeq) errSeq {
	return func(yield func(*vError) bool) {
		st.enter(1)
		defer st.leave(1)
		for e := range seq {
			st.leave(1)
			ok := yield(e)
			st.enter(1)
			if !ok {
				return
			}
		}
	}
}

// Depth of the bracketed host check _urlsplit runs, zero for a netloc without brackets
func bracketedNetlocDepth(netloc string) int {
	if !strings.Contains(netloc, "[") || !strings.Contains(netloc, "]") {
		return 0
	}
	host := netloc[strings.LastIndexByte(netloc, '@')+1:]
	before, bracketed, ok := strings.Cut(host, "[")
	if !ok || before != "" {
		return 1
	}
	if strings.HasPrefix(bracketed, "v") {
		return 4
	}
	return 6
}

// Frames of _urlsplit below its caller, _splitnetloc or _checknetloc and the bracketed host chain
func urlsplitDepth(url string) int {
	rest := strings.TrimLeft(url, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	if i := strings.IndexByte(rest, ':'); i > 0 && isSchemeText(rest[:i]) {
		rest = rest[i+1:]
	}
	if !strings.HasPrefix(rest, "//") {
		return 2
	}
	end := len(rest)
	for _, c := range "/?#" {
		if k := strings.IndexRune(rest[2:], c); k >= 0 && k+2 < end {
			end = k + 2
		}
	}
	return 2 + bracketedNetlocDepth(rest[2:end])
}

func isSchemeText(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Frames of urljoin below its caller
func urljoinDepth(base, ref string) int {
	if base == "" || ref == "" {
		return 1
	}
	return 1 + max(urlsplitDepth(base), urlsplitDepth(ref))
}

// Frames of urldefrag below its caller
func urldefragDepth(url string) int {
	if !strings.Contains(url, "#") {
		return 2
	}
	return 1 + urlsplitDepth(url)
}

// Frames of unquote below its caller
func unquoteDepth(s string) int {
	if strings.Contains(s, "%") {
		return 3
	}
	return 1
}

// Frames validator_for pushes below its caller, deeper when $schema goes through the URIDict lookup
func validatorForDepth(schema any) int {
	d, ok := schema.(*pyjson.Object)
	if !ok {
		return 1
	}
	if _, has := d.Vals["$schema"]; !has {
		return 1
	}
	return 7
}

// Frames of attrs evolve below its caller, fields and get_generic_base under it
const attrsEvolveDepth = 3

// Frames of Validator.is_type below its caller for one type name and instance
func (d *draft) isTypeDepth(instance any, t any) int {
	switch t {
	case "number":
		if isBool(instance) {
			return 3
		}
		return 4
	case "integer":
		if d.types["integer"] == nil {
			return 3
		}
		if _, legacy := d.types["any"]; legacy || d.name == "draft4" {
			return 3
		}
		return 4
	}
	return 3
}
