package redact

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
)

const pluginMatchTimeout = 5 * time.Second

type bodyKind int

const (
	bodyRun bodyKind = iota
	bodyExact
	bodyDotted
	bodyDigitsDash
	bodyOneOfDash
	bodyOptionalOrg
)

type builtinPattern struct {
	pattern string
	lit     string
	kind    bodyKind
	class   func(byte) bool
	min     int
}

func isAlnumUnderscore(c byte) bool { return isAlnum(c) || c == '_' }
func isAlnumDash(c byte) bool       { return isAlnum(c) || c == '-' }
func isTokenOrEquals(c byte) bool   { return isTokenChar(c) || c == '=' }
func isTokenOrDot(c byte) bool      { return isTokenChar(c) || c == '.' }
func isUpperDigit(c byte) bool      { return (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') }

func run(lit string, class func(byte) bool, min int, pattern string) builtinPattern {
	return builtinPattern{pattern: pattern, lit: lit, kind: bodyRun, class: class, min: min}
}

var builtins = []builtinPattern{
	{pattern: `sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`, lit: "sk-", kind: bodyDotted, class: isTokenChar, min: 9},
	run("ctx7sk-", isTokenChar, 10, `ctx7sk-[A-Za-z0-9_-]{10,}`),
	run("ghp_", isAlnum, 10, `ghp_[A-Za-z0-9]{10,}`),
	run("github_pat_", isAlnumUnderscore, 10, `github_pat_[A-Za-z0-9_]{10,}`),
	run("gho_", isAlnum, 10, `gho_[A-Za-z0-9]{10,}`),
	run("ghu_", isAlnum, 10, `ghu_[A-Za-z0-9]{10,}`),
	run("ghs_", isAlnum, 10, `ghs_[A-Za-z0-9]{10,}`),
	run("ghr_", isAlnum, 10, `ghr_[A-Za-z0-9]{10,}`),
	{pattern: `xapp-\d+-[A-Za-z0-9-]{10,}`, lit: "xapp-", kind: bodyDigitsDash, class: isAlnumDash, min: 10},
	{pattern: `xox[baprs]-[A-Za-z0-9-]{10,}`, lit: "xox", kind: bodyOneOfDash, class: isAlnumDash, min: 10},
	run("AIza", isTokenChar, 30, `AIza[A-Za-z0-9_-]{30,}`),
	run("pplx-", isAlnum, 10, `pplx-[A-Za-z0-9]{10,}`),
	run("fal_", isTokenChar, 10, `fal_[A-Za-z0-9_-]{10,}`),
	run("fc-", isAlnum, 10, `fc-[A-Za-z0-9]{10,}`),
	run("bb_live_", isTokenChar, 10, `bb_live_[A-Za-z0-9_-]{10,}`),
	run("gAAAA", isTokenOrEquals, 20, `gAAAA[A-Za-z0-9_=-]{20,}`),
	{pattern: `AKIA[A-Z0-9]{16}`, lit: "AKIA", kind: bodyExact, class: isUpperDigit, min: 16},
	run("sk_live_", isAlnum, 10, `sk_live_[A-Za-z0-9]{10,}`),
	run("sk_test_", isAlnum, 10, `sk_test_[A-Za-z0-9]{10,}`),
	run("rk_live_", isAlnum, 10, `rk_live_[A-Za-z0-9]{10,}`),
	run("SG.", isTokenChar, 10, `SG\.[A-Za-z0-9_-]{10,}`),
	run("hf_", isAlnum, 10, `hf_[A-Za-z0-9]{10,}`),
	run("r8_", isAlnum, 10, `r8_[A-Za-z0-9]{10,}`),
	run("npm_", isAlnum, 10, `npm_[A-Za-z0-9]{10,}`),
	run("pypi-", isTokenChar, 10, `pypi-[A-Za-z0-9_-]{10,}`),
	run("dop_v1_", isAlnum, 10, `dop_v1_[A-Za-z0-9]{10,}`),
	run("doo_v1_", isAlnum, 10, `doo_v1_[A-Za-z0-9]{10,}`),
	{pattern: `am_(?:org_)?[A-Za-z0-9]{20,}`, lit: "am_", kind: bodyOptionalOrg, class: isAlnum, min: 20},
	run("sk_", isAlnumUnderscore, 10, `sk_[A-Za-z0-9_]{10,}`),
	run("tvly-", isAlnum, 10, `tvly-[A-Za-z0-9]{10,}`),
	run("exa_", isAlnum, 10, `exa_[A-Za-z0-9]{10,}`),
	run("gsk_", isAlnum, 10, `gsk_[A-Za-z0-9]{10,}`),
	run("syt_", isAlnum, 10, `syt_[A-Za-z0-9]{10,}`),
	run("retaindb_", isAlnum, 10, `retaindb_[A-Za-z0-9]{10,}`),
	run("hsk-", isAlnum, 10, `hsk-[A-Za-z0-9]{10,}`),
	run("mem0_", isAlnum, 10, `mem0_[A-Za-z0-9]{10,}`),
	run("brv_", isAlnum, 10, `brv_[A-Za-z0-9]{10,}`),
	run("xai-", isAlnum, 30, `xai-[A-Za-z0-9]{30,}`),
	run("ntn_", isAlnum, 10, `ntn_[A-Za-z0-9]{10,}`),
	run("fw-", isAlnum, 30, `fw-[A-Za-z0-9]{30,}`),
	run("fw_", isAlnum, 30, `fw_[A-Za-z0-9]{30,}`),
	run("fpk_", isAlnum, 30, `fpk_[A-Za-z0-9]{30,}`),
	run("glpat-", isTokenChar, 10, `glpat-[A-Za-z0-9_\-]{10,}`),
	run("gloas-", isTokenChar, 10, `gloas-[A-Za-z0-9_\-]{10,}`),
	run("gldt-", isTokenChar, 10, `gldt-[A-Za-z0-9_\-]{10,}`),
	run("glrt-", isTokenOrDot, 10, `glrt-[A-Za-z0-9_.\-]{10,}`),
	run("glrtr-", isTokenOrDot, 10, `glrtr-[A-Za-z0-9_.\-]{10,}`),
	run("glcbt-", isTokenChar, 10, `glcbt-[A-Za-z0-9_\-]{10,}`),
	run("glptt-", isTokenChar, 10, `glptt-[A-Za-z0-9_\-]{10,}`),
	run("glft-", isTokenChar, 10, `glft-[A-Za-z0-9_\-]{10,}`),
	run("glimt-", isTokenChar, 10, `glimt-[A-Za-z0-9_\-]{10,}`),
	run("glagent-", isTokenChar, 10, `glagent-[A-Za-z0-9_\-]{10,}`),
	run("glsoat-", isTokenChar, 10, `glsoat-[A-Za-z0-9_\-]{10,}`),
	run("glffct-", isTokenChar, 10, `glffct-[A-Za-z0-9_\-]{10,}`),
	run("glwt-", isTokenChar, 10, `glwt-[A-Za-z0-9_\-]{10,}`),
	run("GR1348941", isTokenChar, 10, `GR1348941[A-Za-z0-9_\-]{10,}`),
	run("pk-lf-", isAlnumDash, 8, `pk-lf-[A-Za-z0-9\-]{8,}`),
}

var builtinsByFirst = indexBuiltins()

func indexBuiltins() [256][]int {
	var idx [256][]int
	for i, b := range builtins {
		idx[b.lit[0]] = append(idx[b.lit[0]], i)
	}
	return idx
}

func builtinLiterals() []string {
	out := make([]string, len(builtins))
	for i, b := range builtins {
		out[i] = literalPrefix(b.pattern)
	}
	return out
}

func lookaheadOK(text string, end int) bool {
	return end >= len(text) || !isTokenChar(text[end])
}

func runEnd(text string, q int, class func(byte) bool) int {
	for q < len(text) && class(text[q]) {
		q++
	}
	return q
}

func greedyRun(text string, q int, class func(byte) bool, min int) (int, bool) {
	r := runEnd(text, q, class) - q
	for k := r; k >= min; k-- {
		if lookaheadOK(text, q+k) {
			return q + k, true
		}
	}
	return 0, false
}

func (b builtinPattern) match(text string, p int) (int, bool) {
	if !strings.HasPrefix(text[p:], b.lit) {
		return 0, false
	}
	q := p + len(b.lit)
	switch b.kind {
	case bodyRun:
		return greedyRun(text, q, b.class, b.min)
	case bodyExact:
		if q+b.min > len(text) || runEnd(text[:q+b.min], q, b.class) != q+b.min {
			return 0, false
		}
		if lookaheadOK(text, q+b.min) {
			return q + b.min, true
		}
		return 0, false
	case bodyDotted:
		if q >= len(text) || !isTokenChar(text[q]) {
			return 0, false
		}
		i, units := q+1, 0
		for {
			if i < len(text) && isTokenChar(text[i]) {
				i++
			} else if i+1 < len(text) && text[i] == '.' && isTokenChar(text[i+1]) {
				i += 2
			} else {
				break
			}
			units++
		}
		if units < b.min {
			return 0, false
		}
		return i, true
	case bodyDigitsDash:
		i, digits := q, 0
		for i < len(text) {
			r, w := utf8.DecodeRuneInString(text[i:])
			if !unicode.Is(unicode.Nd, r) {
				break
			}
			i += w
			digits++
		}
		if digits == 0 || i >= len(text) || text[i] != '-' {
			return 0, false
		}
		return greedyRun(text, i+1, b.class, b.min)
	case bodyOneOfDash:
		if q+1 >= len(text) || !strings.ContainsRune("baprs", rune(text[q])) || text[q+1] != '-' {
			return 0, false
		}
		return greedyRun(text, q+2, b.class, b.min)
	case bodyOptionalOrg:
		if strings.HasPrefix(text[q:], "org_") {
			if end, ok := greedyRun(text, q+4, b.class, b.min); ok {
				return end, true
			}
		}
		return greedyRun(text, q, b.class, b.min)
	}
	return 0, false
}

func scanBuiltins(text string) [][2]int {
	var out [][2]int
	for p := 0; p < len(text); p++ {
		idx := builtinsByFirst[text[p]]
		if len(idx) == 0 || (p > 0 && isTokenChar(text[p-1])) {
			continue
		}
		for _, k := range idx {
			if end, ok := builtins[k].match(text, p); ok {
				out = append(out, [2]int{p, end})
				p = end - 1
				break
			}
		}
	}
	return out
}

type pluginSource struct {
	name     string
	patterns []string
}

type prefixSet struct {
	sources  []pluginSource
	literals []string
	combined *regexp2.Regexp
}

func (ps *prefixSet) pluginPatterns() []string {
	var out []string
	for _, s := range ps.sources {
		out = append(out, s.patterns...)
	}
	return out
}

func (ps *prefixSet) hasLiteral(text string) bool {
	for _, l := range ps.literals {
		if strings.Contains(text, l) {
			return true
		}
	}
	return false
}

func (ps *prefixSet) find(text string) ([][2]int, error) {
	if ps.combined == nil {
		return scanBuiltins(text), nil
	}
	runes := []rune(text)
	offsets := make([]int, 0, len(runes)+1)
	for i := 0; i < len(text); {
		offsets = append(offsets, i)
		_, w := utf8.DecodeRuneInString(text[i:])
		i += w
	}
	offsets = append(offsets, len(text))
	var out [][2]int
	m, err := ps.combined.FindRunesMatch(runes)
	for err == nil && m != nil {
		g := m.GroupByNumber(1)
		out = append(out, [2]int{offsets[g.RuneIndex], offsets[g.RuneIndex+g.RuneLength]})
		m, err = ps.combined.FindNextMatch(m)
	}
	return out, err
}

func (ps *prefixSet) replace(text string, mask func(string) string) (string, error) {
	found, err := ps.find(text)
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return text, nil
	}
	var b strings.Builder
	last := 0
	for _, f := range found {
		b.WriteString(text[last:f[0]])
		b.WriteString(mask(text[f[0]:f[1]]))
		last = f[1]
	}
	b.WriteString(text[last:])
	return b.String(), nil
}

func (ps *prefixSet) maskNonreusable(token string) string {
	if token != "" {
		for _, l := range ps.literals {
			if strings.HasPrefix(token, l) {
				return "«redacted:" + l + "…»"
			}
		}
	}
	return "«redacted-secret»"
}

func isSplitControl(r rune, w int) bool {
	if r == utf8.RuneError && w == 1 {
		return false
	}
	return r <= 0x1f || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x2028 && r <= 0x202f) || r == 0x2060 || r == 0xfeff
}

func spanIsTokenBody(span string) bool {
	for i := 0; i < len(span); {
		r, w := utf8.DecodeRuneInString(span[i:])
		if !(r < utf8.RuneSelf && (isTokenChar(byte(r)) || r == '.')) && !isSplitControl(r, w) {
			return false
		}
		i += w
	}
	return true
}

func (ps *prefixSet) maskControlSplit(text string, mask func(string) string) (string, error) {
	var stripped strings.Builder
	origAt := make([]int, 0, len(text))
	removed := false
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		if isSplitControl(r, w) {
			removed = true
			i += w
			continue
		}
		stripped.WriteString(text[i : i+w])
		for k := 0; k < w; k++ {
			origAt = append(origAt, i+k)
		}
		i += w
	}
	if !removed {
		return text, nil
	}
	s := stripped.String()
	found, err := ps.find(s)
	if err != nil {
		return "", err
	}
	type span struct {
		start, end int
		with       string
	}
	var spans []span
	for _, f := range found {
		start, end := origAt[f[0]], origAt[f[1]-1]+1
		piece := text[start:end]
		if strings.ContainsAny(piece, "\n\r") {
			hits, err := ps.find(piece)
			if err != nil {
				return "", err
			}
			if len(hits) > 0 {
				continue
			}
		}
		if spanIsTokenBody(piece) && (end >= len(text) || text[end] != '=') {
			spans = append(spans, span{start, end, mask(s[f[0]:f[1]])})
		}
	}
	if len(spans) == 0 {
		return text, nil
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(text[last:sp.start])
		b.WriteString(sp.with)
		last = sp.end
	}
	b.WriteString(text[last:])
	return b.String(), nil
}

func literalPrefix(pattern string) string {
	for i, ch := range pattern {
		if strings.ContainsRune(`[(\.?*+|{^$`, ch) {
			return pattern[:i]
		}
	}
	return pattern
}

func skipCharClass(pattern string, i int) int {
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

func unboundedQuantifierFollows(pattern string, j int) bool {
	if j >= len(pattern) {
		return false
	}
	switch pattern[j] {
	case '*', '+':
		return true
	case '{':
		k := strings.IndexByte(pattern[j:], '}')
		if k < 0 {
			return false
		}
		body := pattern[j+1 : j+k]
		if !strings.HasSuffix(body, ",") {
			return false
		}
		digits := body[:len(body)-1]
		if digits == "" {
			return false
		}
		for _, r := range digits {
			if !unicode.IsDigit(r) {
				return false
			}
		}
		return true
	}
	return false
}

func patternStructure(pattern string) (topLevelAlt, nested bool) {
	containsUnbounded := []bool{false}
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch {
		case ch == '\\':
			i++
		case ch == '[':
			i = skipCharClass(pattern, i)
		case ch == '(':
			containsUnbounded = append(containsUnbounded, false)
		case ch == ')':
			inner := false
			if len(containsUnbounded) > 1 {
				inner = containsUnbounded[len(containsUnbounded)-1]
				containsUnbounded = containsUnbounded[:len(containsUnbounded)-1]
			}
			if inner && unboundedQuantifierFollows(pattern, i+1) {
				nested = true
			}
			containsUnbounded[len(containsUnbounded)-1] = containsUnbounded[len(containsUnbounded)-1] || inner
		case ch == '|' && len(containsUnbounded) == 1:
			topLevelAlt = true
		case unboundedQuantifierFollows(pattern, i):
			containsUnbounded[len(containsUnbounded)-1] = true
			if ch == '{' {
				i += strings.IndexByte(pattern[i:], '}')
			}
		}
	}
	return topLevelAlt, nested
}

func rejectReason(pattern string) string {
	alt, nested := patternStructure(pattern)
	switch {
	case alt:
		return "%s: skipping redaction pattern %q — top-level alternation escapes the literal-prefix " +
			"guarantee (in 'ab|.*' the prefix binds only the first branch); wrap alternation in " +
			"a group after the prefix, e.g. 'ab(?:x|y)'"
	case nested:
		return "%s: skipping redaction pattern %q — nested unbounded quantifiers (e.g. '(a+)+') can " +
			"backtrack catastrophically, and registered patterns run on every log line and tool output"
	case utf8.RuneCountInString(literalPrefix(pattern)) < 2:
		return "%s: skipping redaction pattern %q — must start with at least 2 literal characters " +
			"(needed for the pre-screen substring gate)"
	}
	return ""
}

func isBuiltinPattern(pattern string) bool {
	for _, b := range builtins {
		if b.pattern == pattern {
			return true
		}
	}
	return false
}

func (ps *prefixSet) with(source string, accepted []string) (*prefixSet, error) {
	sources := make([]pluginSource, 0, len(ps.sources)+1)
	added := false
	for _, s := range ps.sources {
		if s.name == source {
			s = pluginSource{name: s.name, patterns: append(slices.Clone(s.patterns), accepted...)}
			added = true
		}
		sources = append(sources, s)
	}
	if !added {
		sources = append(sources, pluginSource{name: source, patterns: slices.Clone(accepted)})
	}
	next := &prefixSet{sources: sources}
	all := make([]string, 0, len(builtins))
	for _, b := range builtins {
		all = append(all, b.pattern)
	}
	all = append(all, next.pluginPatterns()...)
	combined, err := regexp2.Compile(`(?<![A-Za-z0-9_-])(` + strings.Join(all, "|") + `)(?![A-Za-z0-9_-])`)
	if err != nil {
		next.literals, next.combined = ps.literals, ps.combined
		return next, err
	}
	combined.MatchTimeout = pluginMatchTimeout
	next.combined = combined
	next.literals = make([]string, len(all))
	for i, p := range all {
		next.literals[i] = literalPrefix(p)
	}
	return next, nil
}

func (r *Redactor) RegisterPatterns(patterns []string, source string) (int, error) {
	r.patternsMu.Lock()
	defer r.patternsMu.Unlock()
	cur := r.prefixes.Load()
	existing := cur.pluginPatterns()
	var accepted []string
	for _, raw := range patterns {
		pattern := pyStrip(raw)
		if pattern == "" {
			slog.Warn(fmt.Sprintf("%s: skipping empty/non-string redaction pattern", source))
			continue
		}
		if _, err := regexp2.Compile(pattern); err != nil {
			slog.Warn(fmt.Sprintf("%s: skipping invalid redaction pattern %q (%v)", source, pattern, err))
			continue
		}
		if msg := rejectReason(pattern); msg != "" {
			slog.Warn(fmt.Sprintf(msg, source, pattern))
			continue
		}
		if isBuiltinPattern(pattern) || slices.Contains(existing, pattern) || slices.Contains(accepted, pattern) {
			slog.Debug(fmt.Sprintf("%s: redaction pattern %q already registered", source, pattern))
			continue
		}
		accepted = append(accepted, pattern)
	}
	if len(accepted) == 0 {
		return 0, nil
	}
	next, err := cur.with(source, accepted)
	r.prefixes.Store(next)
	if err != nil {
		return 0, err
	}
	slog.Info(fmt.Sprintf("%s: registered %d redaction pattern(s)", source, len(accepted)))
	return len(accepted), nil
}
