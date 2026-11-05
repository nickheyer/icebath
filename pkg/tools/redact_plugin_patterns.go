package tools

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type prefixMatcher struct {
	ext        *regexp2.Regexp
	substrings []string
}

type sourcePatterns struct {
	source   string
	patterns []string
}

var (
	pluginPatternsMu     sync.Mutex
	pluginPrefixPatterns []*sourcePatterns
	activePrefix         atomic.Pointer[prefixMatcher]
)

func init() {
	activePrefix.Store(&prefixMatcher{substrings: prefixSubstrings})
}

// Literal prefix substrings of the builtin and plugin patterns in registration order
func currentPrefixSubstrings() []string {
	return activePrefix.Load().substrings
}

// Token spans of every builtin or plugin prefix pattern bounded by non token characters
func prefixScan(text string) [][]int {
	m := activePrefix.Load()
	if m.ext == nil {
		return scanMatches(prefixAltRe, text, prefixBoundaryOK(text))
	}
	var out [][]int
	match, err := m.ext.FindStringMatch(text)
	for err == nil && match != nil {
		start, n := match.ByteRange()
		out = append(out, []int{start, start + n})
		match, err = m.ext.FindNextMatch(match)
	}
	return out
}

// Whether any bounded prefix token occurs in the text
func prefixSearch(text string) bool {
	m := activePrefix.Load()
	if m.ext == nil {
		return len(scanMatches(prefixAltRe, text, prefixBoundaryOK(text))) > 0
	}
	ok, err := m.ext.MatchString(text)
	return err == nil && ok
}

// Every plugin registered pattern in registration order
func pluginPatterns() []string {
	var out []string
	for _, sp := range pluginPrefixPatterns {
		out = append(out, sp.patterns...)
	}
	return out
}

// Recompiles the prefix alternation and pre screen substrings from builtins plus plugin patterns
func rebuildPrefixMatcher() error {
	plugin := pluginPatterns()
	if len(plugin) == 0 {
		activePrefix.Store(&prefixMatcher{substrings: prefixSubstrings})
		return nil
	}
	combined := append(append([]string{}, prefixPatterns...), plugin...)
	re, err := pyre.Compile(`(?<![A-Za-z0-9_-])(` + strings.Join(combined, "|") + `)(?![A-Za-z0-9_-])`)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var subs []string
	for _, p := range combined {
		lit := literalPrefix(p)
		if !seen[lit] {
			seen[lit] = true
			subs = append(subs, lit)
		}
	}
	activePrefix.Store(&prefixMatcher{ext: re, substrings: subs})
	return nil
}

// Index of the closing bracket of the character class opening at i
func skipCharClass(pattern []rune, i int) int {
	if i+1 < len(pattern) && pattern[i+1] == ']' {
		i += 2
	} else {
		i++
	}
	for i < len(pattern) && pattern[i] != ']' {
		if pattern[i] == '\\' {
			i += 2
		} else {
			i++
		}
	}
	return i
}

// Whether a star, plus or open ended brace quantifier starts at j
func unboundedQuantifierFollows(pattern []rune, j int) bool {
	if j >= len(pattern) {
		return false
	}
	switch pattern[j] {
	case '*', '+':
		return true
	case '{':
		k := indexRune(pattern, '}', j)
		if k == -1 {
			return false
		}
		body := pattern[j+1 : k]
		if len(body) < 2 || body[len(body)-1] != ',' {
			return false
		}
		for _, r := range body[:len(body)-1] {
			if !pystr.IsDigit(r) {
				return false
			}
		}
		return true
	}
	return false
}

func indexRune(pattern []rune, r rune, from int) int {
	for k := from; k < len(pattern); k++ {
		if pattern[k] == r {
			return k
		}
	}
	return -1
}

// Top level alternation and an unbounded repeat over a group holding one, found in one scan
func patternStructure(pattern string) (topLevelAlt, nested bool) {
	src := []rune(pattern)
	containsUnbounded := []bool{false}
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if ch == '\\' {
			i++
			continue
		}
		switch {
		case ch == '[':
			i = skipCharClass(src, i)
		case ch == '(':
			containsUnbounded = append(containsUnbounded, false)
		case ch == ')':
			inner := false
			if len(containsUnbounded) > 1 {
				inner = containsUnbounded[len(containsUnbounded)-1]
				containsUnbounded = containsUnbounded[:len(containsUnbounded)-1]
			}
			if inner && unboundedQuantifierFollows(src, i+1) {
				nested = true
			}
			containsUnbounded[len(containsUnbounded)-1] = containsUnbounded[len(containsUnbounded)-1] || inner
		case ch == '|' && len(containsUnbounded) == 1:
			topLevelAlt = true
		case unboundedQuantifierFollows(src, i):
			containsUnbounded[len(containsUnbounded)-1] = true
			if ch == '{' {
				i = indexRune(src, '}', i)
			}
		}
	}
	return topLevelAlt, nested
}

func hasTopLevelAlternation(pattern string) bool {
	alt, _ := patternStructure(pattern)
	return alt
}

func hasNestedUnboundedRepeat(pattern string) bool {
	_, nested := patternStructure(pattern)
	return nested
}

var patternRejectRules = []struct {
	reject  func(string) bool
	message string
}{
	{hasTopLevelAlternation, "%s: skipping redaction pattern %s — top-level alternation escapes the literal-prefix " +
		"guarantee (in 'ab|.*' the prefix binds only the first branch); wrap alternation in " +
		"a group after the prefix, e.g. 'ab(?:x|y)'"},
	{hasNestedUnboundedRepeat, "%s: skipping redaction pattern %s — nested unbounded quantifiers (e.g. '(a+)+') can " +
		"backtrack catastrophically, and registered patterns run on every log line and tool output"},
	{func(pattern string) bool { return len([]rune(literalPrefix(pattern))) < 2 },
		"%s: skipping redaction pattern %s — must start with at least 2 literal characters " +
			"(needed for the pre-screen substring gate)"},
}

// Elements of a Python iterable argument, the TypeError text when it is not iterable
func iterPatterns(patterns any) ([]any, error) {
	if patterns == nil || !pyrepr.Truthy(patterns) {
		return nil, nil
	}
	switch v := patterns.(type) {
	case string:
		var out []any
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case []string:
		out := make([]any, len(v))
		for i, s := range v {
			out[i] = s
		}
		return out, nil
	case []any:
		return v, nil
	case pyrepr.Mapping:
		var out []any
		for k := range v.PyItems() {
			out = append(out, k)
		}
		return out, nil
	}
	rv := reflect.ValueOf(patterns)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = rv.Index(i).Interface()
		}
		return out, nil
	case reflect.Map:
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface()) })
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k.Interface()
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(patterns))
}

// RegisterRedactionPatterns additively registers credential token regexes and returns how many were accepted
func RegisterRedactionPatterns(patterns any, source string) (int, error) {
	items, err := iterPatterns(patterns)
	if err != nil {
		return 0, err
	}
	pluginPatternsMu.Lock()
	known := append(append([]string{}, prefixPatterns...), pluginPatterns()...)
	pluginPatternsMu.Unlock()
	var accepted []string
	for _, raw := range items {
		pattern, ok := raw.(string)
		if !ok || pystr.Strip(pattern) == "" {
			slog.Warn(fmt.Sprintf("%s: skipping empty/non-string redaction pattern", source))
			continue
		}
		pattern = pystr.Strip(pattern)
		if _, cerr := pyre.Compile(pattern); cerr != nil {
			var overflow *pyre.OverflowError
			if errors.As(cerr, &overflow) {
				return 0, cerr
			}
			slog.Warn(fmt.Sprintf("%s: skipping invalid redaction pattern %s (%s)", source, pyrepr.StrRepr(pattern), cerr))
			continue
		}
		rejected := ""
		for _, rule := range patternRejectRules {
			if rule.reject(pattern) {
				rejected = fmt.Sprintf(rule.message, source, pyrepr.StrRepr(pattern))
				break
			}
		}
		if rejected != "" {
			slog.Warn(rejected)
			continue
		}
		if containsString(known, pattern) || containsString(accepted, pattern) {
			slog.Debug(fmt.Sprintf("%s: redaction pattern %s already registered", source, pyrepr.StrRepr(pattern)))
			continue
		}
		accepted = append(accepted, pattern)
	}
	if len(accepted) > 0 {
		pluginPatternsMu.Lock()
		var bucket *sourcePatterns
		for _, sp := range pluginPrefixPatterns {
			if sp.source == source {
				bucket = sp
			}
		}
		if bucket == nil {
			bucket = &sourcePatterns{source: source}
			pluginPrefixPatterns = append(pluginPrefixPatterns, bucket)
		}
		bucket.patterns = append(bucket.patterns, accepted...)
		err := rebuildPrefixMatcher()
		pluginPatternsMu.Unlock()
		if err != nil {
			return 0, err
		}
		slog.Info(fmt.Sprintf("%s: registered %d redaction pattern(s)", source, len(accepted)))
	}
	return len(accepted), nil
}

// ResetPluginRedactionPatterns drops every plugin registered pattern and restores the builtin matcher
func ResetPluginRedactionPatterns() {
	pluginPatternsMu.Lock()
	defer pluginPatternsMu.Unlock()
	pluginPrefixPatterns = nil
	activePrefix.Store(&prefixMatcher{substrings: prefixSubstrings})
}
