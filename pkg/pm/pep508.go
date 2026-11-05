package pm

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// InvalidRequirementError is a dependency specifier packaging refuses
type InvalidRequirementError struct{ Message string }

func (e *InvalidRequirementError) Error() string { return e.Message }

// ParserSyntaxError is a specifier syntax error pointing at the offending span of the source
type ParserSyntaxError struct {
	Message    string
	Source     string
	Start, End int
}

func (e *ParserSyntaxError) Error() string {
	marker := strings.Repeat(" ", max(0, e.Start)) + strings.Repeat("~", max(0, e.End-e.Start)) + "^"
	return fmt.Sprintf("%s\n    %s\n    %s", e.Message, e.Source, marker)
}

// Character class Python's case insensitive Unicode matching gives one ASCII letter
func pyFoldClass(c byte, unicodeFold bool) string {
	lower, upper := strings.ToLower(string(c)), strings.ToUpper(string(c))
	if lower == upper {
		return regexp.QuoteMeta(string(c))
	}
	extra := ""
	if unicodeFold {
		switch lower {
		case "i":
			extra = `\x{130}\x{131}`
		case "s":
			extra = `\x{17f}`
		case "k":
			extra = `\x{212a}`
		}
	}
	return "[" + lower + upper + extra + "]"
}

// Pattern matching a literal word case insensitively the way a Python IGNORECASE group does
func pyFold(word string, unicodeFold bool) string {
	var b strings.Builder
	for i := 0; i < len(word); i++ {
		b.WriteString(pyFoldClass(word[i], unicodeFold))
	}
	return b.String()
}

// Alternation of folded words in their given order
func pyFoldAlt(unicodeFold bool, words ...string) string {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = pyFold(w, unicodeFold)
	}
	return "(?:" + strings.Join(parts, "|") + ")"
}

// Packaging's specifier pattern, ASCII groups folding only ASCII and the compatible branch folding Unicode
func specifierPattern() string {
	ws := "[" + pyWhitespaceClass + "]*"
	pre := func(u bool) string {
		return `(?:[-_\.]?` + pyFoldAlt(u, "alpha", "beta", "preview", "pre", "a", "b", "c", "rc") + `[-_\.]?[0-9]*)?`
	}
	post := func(u bool) string {
		return `(?:(?:-[0-9]+)|(?:[-_\.]?` + pyFoldAlt(u, "post", "rev", "r") + `[-_\.]?[0-9]*))?`
	}
	dev := func(u bool) string { return `(?:[-_\.]?` + pyFold("dev", u) + `[-_\.]?[0-9]*)?` }
	local := `(?:\+[a-zA-Z0-9]+(?:[-_\.][a-zA-Z0-9]+)*)?`
	identity := `(?:===` + ws + `[^` + pyWhitespaceClass + `;)]*)`
	equality := `(?:(?:==|!=)` + ws + `[vV]?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*(?:\.\*|` + pre(false) + post(false) + dev(false) + local + `)?)`
	compatible := `(?:~=` + ws + `[vV]?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)+` + pre(true) + post(true) + dev(true) + `)`
	ordered := `(?:(?:<=|>=|<|>)` + ws + `[vV]?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*` + pre(false) + post(false) + dev(false) + `)`
	return `(?:` + identity + `|` + equality + `|` + compatible + `|` + ordered + `)`
}

var (
	specifierToken     = regexp.MustCompile(`^` + specifierPattern())
	specifierFull      = regexp.MustCompile(`^[` + pyWhitespaceClass + `]*` + specifierPattern() + `[` + pyWhitespaceClass + `]*$`)
	localSeparators    = regexp.MustCompile(`[\._-]`)
	versionLocalTrail  = regexp.MustCompile(`^\+[a-z0-9]+(?:[-_\.][a-z0-9]+)*`)
	pep440VersionRegex = regexp.MustCompile(`^[` + pyWhitespaceClass + `]*[vV]?(?:(?:(?P<epoch>[0-9]+)!)?(?P<release>[0-9]+(?:\.[0-9]+)*)` +
		`(?P<pre>[._-]?(?P<pre_l>` + pyFoldAlt(false, "alpha", "a", "beta", "b", "preview", "pre", "c", "rc") + `)[._-]?(?P<pre_n>[0-9]+)?)?` +
		`(?P<post>(?:-(?P<post_n1>[0-9]+))|(?:[._-]?(?P<post_l>` + pyFoldAlt(false, "post", "rev", "r") + `)[._-]?(?P<post_n2>[0-9]+)?))?` +
		`(?P<dev>[._-]?(?P<dev_l>` + pyFold("dev", false) + `)[._-]?(?P<dev_n>[0-9]+)?)?)` +
		`(?:\+(?P<local>[a-zA-Z0-9]+(?:[._-][a-zA-Z0-9]+)*))?[` + pyWhitespaceClass + `]*$`)
	letterNormalization = map[string]string{"alpha": "a", "beta": "b", "c": "rc", "pre": "rc", "preview": "rc", "rev": "post", "r": "post"}
	markerVariables     = []string{
		"python_version", "python_full_version", "os.name", "os_name", "sys.platform", "sys_platform",
		"platform_release", "platform_system", "platform.version", "platform_version", "platform.machine",
		"platform_machine", "platform.python_implementation", "platform_python_implementation",
		"python_implementation", "implementation_name", "implementation_version", "extras", "extra",
		"dependency_groups",
	}
)

// Decimal value of a digit run as Python int prints it, refusing runs past the conversion limit
func pyDecimal(digits string) (string, error) {
	if len(digits) > 4300 {
		return "", &PyError{Class: "ValueError", Message: fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", len(digits))}
	}
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0", nil
	}
	return trimmed, nil
}

// pep440Version is a parsed PEP 440 version with integers kept as normalized decimal text
type pep440Version struct {
	epoch   string
	release []string
	pre     []string
	post    string
	dev     string
	local   []string
	hasPost bool
	hasDev  bool
}

// Parses a version like packaging Version, ok false for an invalid one
func parsePEP440(text string) (*pep440Version, bool, error) {
	simple := true
	for _, r := range text {
		if r != '.' && (r < '0' || r > '9') {
			simple = false
			break
		}
	}
	if simple {
		v := &pep440Version{epoch: "0"}
		for _, part := range strings.Split(text, ".") {
			if part == "" {
				return nil, false, nil
			}
			n, err := pyDecimal(part)
			if err != nil {
				return nil, false, err
			}
			v.release = append(v.release, n)
		}
		return v, true, nil
	}
	m := pep440VersionRegex.FindStringSubmatch(text)
	if m == nil {
		return nil, false, nil
	}
	group := func(name string) string { return m[pep440VersionRegex.SubexpIndex(name)] }
	v := &pep440Version{epoch: "0"}
	var err error
	if e := group("epoch"); e != "" {
		if v.epoch, err = pyDecimal(e); err != nil {
			return nil, false, err
		}
	}
	for _, part := range strings.Split(group("release"), ".") {
		n, err := pyDecimal(part)
		if err != nil {
			return nil, false, err
		}
		v.release = append(v.release, n)
	}
	letterNumber := func(letter, number string) (string, string, bool, error) {
		if letter != "" {
			letter = strings.ToLower(letter)
			if norm, ok := letterNormalization[letter]; ok {
				letter = norm
			}
			if number == "" {
				number = "0"
			}
			n, err := pyDecimal(number)
			return letter, n, true, err
		}
		if number != "" {
			n, err := pyDecimal(number)
			return "post", n, true, err
		}
		return "", "", false, nil
	}
	if l, n, ok, err := letterNumber(group("pre_l"), group("pre_n")); err != nil {
		return nil, false, err
	} else if ok {
		v.pre = []string{l, n}
	}
	postNumber := group("post_n1")
	if postNumber == "" {
		postNumber = group("post_n2")
	}
	if _, n, ok, err := letterNumber(group("post_l"), postNumber); err != nil {
		return nil, false, err
	} else if ok {
		v.post, v.hasPost = n, true
	}
	if _, n, ok, err := letterNumber(group("dev_l"), group("dev_n")); err != nil {
		return nil, false, err
	} else if ok {
		v.dev, v.hasDev = n, true
	}
	if local := group("local"); local != "" {
		for _, part := range localSeparators.Split(local, -1) {
			if isASCIIDigits(part) {
				n, err := pyDecimal(part)
				if err != nil {
					return nil, false, err
				}
				v.local = append(v.local, n)
			} else {
				v.local = append(v.local, strings.ToLower(part))
			}
		}
	}
	return v, true, nil
}

// Reports a nonempty run of ASCII digits
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Normalized text of a version, its release cut to one trailing zero when trimmed
func (v *pep440Version) format(trimmed bool) string {
	release := v.release
	if trimmed {
		i := len(release)
		for i > 1 && release[i-1] == "0" {
			i--
		}
		release = release[:i]
	}
	out := strings.Join(release, ".")
	if v.epoch != "0" {
		out = v.epoch + "!" + out
	}
	if v.pre != nil {
		out += v.pre[0] + v.pre[1]
	}
	if v.hasPost {
		out += ".post" + v.post
	}
	if v.hasDev {
		out += ".dev" + v.dev
	}
	if v.local != nil {
		out += "+" + strings.Join(v.local, ".")
	}
	return out
}

// pySpecifier is one version clause, its operator and version text
type pySpecifier struct{ op, version string }

func (s pySpecifier) String() string { return s.op + s.version }

// Parses one clause like packaging Specifier
func parseSpecifier(spec string) (pySpecifier, error) {
	if !specifierFull.MatchString(spec) {
		return pySpecifier{}, &InvalidRequirementError{Message: "Invalid specifier: " + pyRepr(spec)}
	}
	spec = pyStrip(spec)
	switch {
	case strings.HasPrefix(spec, "==="):
		return pySpecifier{"===", pyStrip(spec[3:])}, nil
	case strings.HasPrefix(spec, "~="), strings.HasPrefix(spec, "=="), strings.HasPrefix(spec, "!="),
		strings.HasPrefix(spec, "<="), strings.HasPrefix(spec, ">="):
		return pySpecifier{spec[:2], pyStrip(spec[2:])}, nil
	}
	return pySpecifier{spec[:1], pyStrip(spec[1:])}, nil
}

// Identity two clauses compare equal by, trailing release zeros dropped except for the compatible operator
func (s pySpecifier) canonical() (string, error) {
	if s.op == "===" || strings.HasSuffix(s.version, ".*") {
		return s.op + "\x00" + s.version, nil
	}
	v, ok, err := parsePEP440(s.version)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &PyError{Class: "AssertionError"}
	}
	return s.op + "\x00" + v.format(s.op != "~="), nil
}

// Parses a comma separated specifier set like packaging SpecifierSet
func parseSpecifierSet(text string) ([]pySpecifier, error) {
	var specs []pySpecifier
	for _, part := range strings.Split(text, ",") {
		part = pyStrip(part)
		if part == "" {
			continue
		}
		s, err := parseSpecifier(part)
		if err != nil {
			return nil, err
		}
		specs = append(specs, s)
	}
	return specs, nil
}

// Text of a specifier set, clauses sorted and equal clauses kept once
func formatSpecifierSet(specs []pySpecifier) (string, error) {
	if len(specs) > 1 {
		sorted := append([]pySpecifier{}, specs...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
		seen := map[string]bool{}
		specs = specs[:0:0]
		for _, s := range sorted {
			key, err := s.canonical()
			if err != nil {
				return "", err
			}
			if !seen[key] {
				seen[key] = true
				specs = append(specs, s)
			}
		}
	}
	parts := make([]string, len(specs))
	for i, s := range specs {
		parts[i] = s.String()
	}
	return strings.Join(parts, ","), nil
}

// Normalizes a project or extra name like packaging canonicalize_name
func CanonicalizeName(name string) string {
	value := strings.ReplaceAll(strings.ReplaceAll(pyLower(name), "_", "-"), ".", "-")
	for strings.Contains(value, "--") {
		value = strings.ReplaceAll(value, "--", "-")
	}
	return value
}

// markerVar is a marker operand, an environment variable name or a string value
type markerVar struct {
	value    string
	variable bool
}

// Serialized operand, values quoted with whichever quote they do not contain
func (v markerVar) serialize() (string, error) {
	if v.variable {
		return v.value, nil
	}
	if !strings.Contains(v.value, `"`) {
		return `"` + v.value + `"`, nil
	}
	if !strings.Contains(v.value, "'") {
		return "'" + v.value + "'", nil
	}
	return "", &PyError{Class: "ValueError", Message: "Cannot serialize marker value containing both quote characters"}
}

// markerItem is one comparison of a marker expression
type markerItem struct {
	lhs markerVar
	op  string
	rhs markerVar
}

// markerList is a marker expression, atoms alternating with and or or, an atom being an item or a nested list
type markerList []any

// Formats a marker like packaging, nested groups parenthesized
func formatMarker(marker any, first bool) (string, error) {
	if list, ok := marker.(markerList); ok && len(list) == 1 {
		switch list[0].(type) {
		case markerList, markerItem:
			return formatMarker(list[0], first)
		}
	}
	switch m := marker.(type) {
	case markerList:
		parts := make([]string, len(m))
		for i, inner := range m {
			text, err := formatMarker(inner, false)
			if err != nil {
				return "", err
			}
			parts[i] = text
		}
		if first {
			return strings.Join(parts, " "), nil
		}
		return "(" + strings.Join(parts, " ") + ")", nil
	case markerItem:
		lhs, err := m.lhs.serialize()
		if err != nil {
			return "", err
		}
		rhs, err := m.rhs.serialize()
		if err != nil {
			return "", err
		}
		return lhs + " " + m.op + " " + rhs, nil
	case string:
		return m, nil
	}
	return "", &PyError{Class: "AssertionError"}
}

// Canonicalizes extra and set membership literals inside a marker
func normalizeExtras(marker any) any {
	switch m := marker.(type) {
	case markerList:
		out := make(markerList, len(m))
		for i, inner := range m {
			out[i] = normalizeExtras(inner)
		}
		return out
	case markerItem:
		switch {
		case m.lhs.variable && m.lhs.value == "extra" && !m.rhs.variable:
			m.rhs = markerVar{value: CanonicalizeName(m.rhs.value)}
		case m.rhs.variable && m.rhs.value == "extra" && !m.lhs.variable:
			m.lhs = markerVar{value: CanonicalizeName(m.lhs.value)}
		case m.rhs.variable && (m.rhs.value == "extras" || m.rhs.value == "dependency_groups") && !m.lhs.variable:
			m.lhs = markerVar{value: CanonicalizeName(m.lhs.value)}
		}
		return m
	}
	return marker
}

// pepTokenizer is packaging's context sensitive tokenizer over code points
type pepTokenizer struct {
	src     []rune
	source  string
	pos     int
	pending string
	length  int
}

// Reports a Python word boundary at a code point offset
func (t *pepTokenizer) boundary(at int) bool {
	before := at > 0 && pyIsWord(t.src[at-1])
	after := at < len(t.src) && pyIsWord(t.src[at])
	return before != after
}

// Length of a regexp match anchored at the current position, in code points
func (t *pepTokenizer) regexLength(re *regexp.Regexp) (int, bool) {
	rest := string(t.src[t.pos:])
	loc := re.FindStringIndex(rest)
	if loc == nil {
		return 0, false
	}
	return utf8.RuneCountInString(rest[:loc[1]]), true
}

// Length of a word bounded literal at the current position
func (t *pepTokenizer) wordLength(words ...string) (int, bool) {
	if !t.boundary(t.pos) {
		return 0, false
	}
	for _, w := range words {
		n := len([]rune(w))
		if t.pos+n <= len(t.src) && string(t.src[t.pos:t.pos+n]) == w && t.boundary(t.pos+n) {
			return n, true
		}
	}
	return 0, false
}

// Length of the named token at the current position, ok false when it does not match
func (t *pepTokenizer) match(name string) (int, bool) {
	src, pos := t.src, t.pos
	at := func(r rune) bool { return pos < len(src) && src[pos] == r }
	switch name {
	case "LEFT_PARENTHESIS":
		return 1, at('(')
	case "RIGHT_PARENTHESIS":
		return 1, at(')')
	case "LEFT_BRACKET":
		return 1, at('[')
	case "RIGHT_BRACKET":
		return 1, at(']')
	case "SEMICOLON":
		return 1, at(';')
	case "COMMA":
		return 1, at(',')
	case "AT":
		return 1, at('@')
	case "QUOTED_STRING":
		if pos < len(src) && (src[pos] == '\'' || src[pos] == '"') {
			for i := pos + 1; i < len(src); i++ {
				if src[i] == src[pos] {
					return i - pos + 1, true
				}
			}
		}
		return 0, false
	case "OP":
		for _, op := range []string{"===", "==", "~=", "!=", "<=", ">=", "<", ">"} {
			if strings.HasPrefix(string(src[pos:min(len(src), pos+3)]), op) {
				return len(op), true
			}
		}
		return 0, false
	case "BOOLOP":
		return t.wordLength("or", "and")
	case "IN":
		return t.wordLength("in")
	case "NOT":
		return t.wordLength("not")
	case "VARIABLE":
		return t.wordLength(markerVariables...)
	case "SPECIFIER":
		return t.regexLength(specifierToken)
	case "URL":
		n := 0
		for pos+n < len(src) && src[pos+n] != ' ' && src[pos+n] != '\t' {
			n++
		}
		return n, n > 0
	case "IDENTIFIER":
		alnum := func(r rune) bool {
			return r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
		}
		if !t.boundary(pos) || pos >= len(src) || !alnum(src[pos]) {
			return 0, false
		}
		end := pos + 1
		for end < len(src) && (alnum(src[end]) || src[end] == '.' || src[end] == '_' || src[end] == '-') {
			end++
		}
		for ; end > pos; end-- {
			if t.boundary(end) {
				return end - pos, true
			}
		}
		return 0, false
	case "VERSION_PREFIX_TRAIL":
		return 2, pos+1 < len(src) && src[pos] == '.' && src[pos+1] == '*'
	case "VERSION_LOCAL_LABEL_TRAIL":
		return t.regexLength(versionLocalTrail)
	case "WS":
		n := 0
		for pos+n < len(src) && (src[pos+n] == ' ' || src[pos+n] == '\t') {
			n++
		}
		return n, n > 0
	case "END":
		return 0, pos == len(src)
	}
	panic("unknown token name: " + name)
}

// Reports whether the named token comes next, loading it unless peeking
func (t *pepTokenizer) check(name string, peek bool) bool {
	n, ok := t.match(name)
	if ok && !peek {
		t.pending, t.length = name, n
	}
	return ok
}

// Skips the named token when it comes next
func (t *pepTokenizer) consume(name string) {
	if t.check(name, false) {
		t.read()
	}
}

// Consumes the loaded token and returns its text and position
func (t *pepTokenizer) read() (string, int) {
	start := t.pos
	t.pos += t.length
	t.pending, t.length = "", 0
	return string(t.src[start:t.pos]), start
}

// Reads the named token or fails naming what was expected
func (t *pepTokenizer) expect(name, expected string) (string, int, error) {
	if !t.check(name, false) {
		return "", 0, t.syntaxError("Expected "+expected, -1, -1)
	}
	text, at := t.read()
	return text, at, nil
}

// Syntax error at the current position or an explicit span
func (t *pepTokenizer) syntaxError(message string, start, end int) error {
	if start < 0 {
		start = t.pos
	}
	if end < 0 {
		end = t.pos
	}
	return &ParserSyntaxError{Message: message, Source: t.source, Start: start, End: end}
}

// Runs body inside an optional pair of enclosing tokens, requiring the close once the open was read
func (t *pepTokenizer) enclosing(open, closeToken, around string, body func() error) error {
	openPos := -1
	if t.check(open, false) {
		openPos = t.pos
		t.read()
	}
	if err := body(); err != nil {
		return err
	}
	if openPos < 0 {
		return nil
	}
	if !t.check(closeToken, false) {
		return t.syntaxError(fmt.Sprintf("Expected matching %s for %s, after %s", closeToken, open, around), openPos, -1)
	}
	t.read()
	return nil
}

// pyRequirement is a parsed dependency specifier
type pyRequirement struct {
	Name      string
	URL       string
	Extras    []string
	Specifier []pySpecifier
	Marker    markerList
}

// Parses a dependency specifier like packaging Requirement
func ParseRequirement(text string) (*pyRequirement, error) {
	t := &pepTokenizer{src: []rune(text), source: text}
	req, err := parseRequirementTokens(t)
	if err != nil {
		if syntax, ok := err.(*ParserSyntaxError); ok {
			return nil, &InvalidRequirementError{Message: syntax.Error()}
		}
		return nil, err
	}
	return req, nil
}

// Grammar of a whole requirement, name then extras then details then the end
func parseRequirementTokens(t *pepTokenizer) (*pyRequirement, error) {
	t.consume("WS")
	name, _, err := t.expect("IDENTIFIER", "package name at the start of dependency specifier")
	if err != nil {
		return nil, err
	}
	t.consume("WS")
	extras, err := parseExtras(t)
	if err != nil {
		return nil, err
	}
	t.consume("WS")
	url, specifier, marker, err := parseRequirementDetails(t)
	if err != nil {
		return nil, err
	}
	if _, _, err := t.expect("END", "end of dependency specifier"); err != nil {
		return nil, err
	}
	req := &pyRequirement{Name: name, URL: url}
	seen := map[string]bool{}
	for _, e := range extras {
		if !seen[e] {
			seen[e] = true
			req.Extras = append(req.Extras, e)
		}
	}
	if req.Specifier, err = parseSpecifierSet(specifier); err != nil {
		return nil, err
	}
	if marker != nil {
		req.Marker = normalizeExtras(marker).(markerList)
	}
	return req, nil
}

// Details after the name, a URL or a version specifier, each optionally followed by a marker
func parseRequirementDetails(t *pepTokenizer) (string, string, markerList, error) {
	if t.check("AT", false) {
		t.read()
		t.consume("WS")
		urlStart := t.pos
		url, _, err := t.expect("URL", "URL after @")
		if err != nil {
			return "", "", nil, err
		}
		if t.check("END", true) {
			return url, "", nil, nil
		}
		if _, _, err := t.expect("WS", "whitespace after URL"); err != nil {
			return "", "", nil, err
		}
		if t.check("END", true) {
			return url, "", nil, nil
		}
		marker, err := parseRequirementMarker(t, urlStart, "semicolon (after URL and whitespace)")
		return url, "", marker, err
	}
	specifierStart := t.pos
	specifier, err := parseSpecifierTokens(t)
	if err != nil {
		return "", "", nil, err
	}
	t.consume("WS")
	if t.check("END", true) {
		return "", specifier, nil, nil
	}
	expected := "semicolon (after name with no version specifier)"
	if specifier != "" {
		expected = "comma (within version specifier), semicolon (after version specifier)"
	}
	marker, err := parseRequirementMarker(t, specifierStart, expected)
	return "", specifier, marker, err
}

// A semicolon then a marker expression
func parseRequirementMarker(t *pepTokenizer, spanStart int, expected string) (markerList, error) {
	if !t.check("SEMICOLON", false) {
		return nil, t.syntaxError(fmt.Sprintf("Expected %s or end", expected), spanStart, -1)
	}
	t.read()
	marker, err := parseMarkerTokens(t)
	if err != nil {
		return nil, err
	}
	t.consume("WS")
	return marker, nil
}

// Optional bracketed extras list
func parseExtras(t *pepTokenizer) ([]string, error) {
	if !t.check("LEFT_BRACKET", true) {
		return nil, nil
	}
	var extras []string
	err := t.enclosing("LEFT_BRACKET", "RIGHT_BRACKET", "extras", func() error {
		t.consume("WS")
		var err error
		extras, err = parseExtrasList(t)
		t.consume("WS")
		return err
	})
	return extras, err
}

// Comma separated extra names
func parseExtrasList(t *pepTokenizer) ([]string, error) {
	var extras []string
	if !t.check("IDENTIFIER", false) {
		return extras, nil
	}
	first, _ := t.read()
	extras = append(extras, first)
	for {
		t.consume("WS")
		if t.check("IDENTIFIER", true) {
			return nil, t.syntaxError("Expected comma between extra names", -1, -1)
		} else if !t.check("COMMA", false) {
			break
		}
		t.read()
		t.consume("WS")
		extra, _, err := t.expect("IDENTIFIER", "extra name after comma")
		if err != nil {
			return nil, err
		}
		extras = append(extras, extra)
	}
	return extras, nil
}

// Version specifier, optionally parenthesized
func parseSpecifierTokens(t *pepTokenizer) (string, error) {
	var parsed string
	err := t.enclosing("LEFT_PARENTHESIS", "RIGHT_PARENTHESIS", "version specifier", func() error {
		t.consume("WS")
		var err error
		parsed, err = parseVersionMany(t)
		t.consume("WS")
		return err
	})
	return parsed, err
}

// Comma joined specifier clauses, refusing misplaced wildcard and local suffixes
func parseVersionMany(t *pepTokenizer) (string, error) {
	parsed := ""
	for t.check("SPECIFIER", false) {
		spanStart := t.pos
		specifier, _ := t.read()
		parsed += specifier
		if t.check("VERSION_PREFIX_TRAIL", true) {
			message := ".* suffix can only be used with `==` or `!=` operators"
			if strings.HasPrefix(specifier, "!=") || (strings.HasPrefix(specifier, "==") && !strings.HasPrefix(specifier, "===")) {
				message = ".* suffix cannot be used with pre-release, post-release, dev or local versions"
			}
			return "", t.syntaxError(message, spanStart, t.pos+1)
		}
		if t.check("VERSION_LOCAL_LABEL_TRAIL", true) {
			return "", t.syntaxError("Local version label can only be used with `==` or `!=` operators", spanStart, t.pos)
		}
		t.consume("WS")
		if !t.check("COMMA", false) {
			break
		}
		comma, _ := t.read()
		parsed += comma
		t.consume("WS")
	}
	return parsed, nil
}

// Marker atoms joined by boolean operators
func parseMarkerTokens(t *pepTokenizer) (markerList, error) {
	atom, err := parseMarkerAtom(t)
	if err != nil {
		return nil, err
	}
	expression := markerList{atom}
	for t.check("BOOLOP", false) {
		op, _ := t.read()
		right, err := parseMarkerAtom(t)
		if err != nil {
			return nil, err
		}
		expression = append(expression, op, right)
	}
	return expression, nil
}

// A parenthesized marker or one comparison
func parseMarkerAtom(t *pepTokenizer) (any, error) {
	t.consume("WS")
	var marker any
	if t.check("LEFT_PARENTHESIS", true) {
		err := t.enclosing("LEFT_PARENTHESIS", "RIGHT_PARENTHESIS", "marker expression", func() error {
			t.consume("WS")
			inner, err := parseMarkerTokens(t)
			marker = inner
			if err == nil {
				t.consume("WS")
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	} else {
		item, err := parseMarkerItem(t)
		if err != nil {
			return nil, err
		}
		marker = item
	}
	t.consume("WS")
	return marker, nil
}

// One comparison of two operands
func parseMarkerItem(t *pepTokenizer) (markerItem, error) {
	t.consume("WS")
	lhs, err := parseMarkerVar(t)
	if err != nil {
		return markerItem{}, err
	}
	t.consume("WS")
	op, err := parseMarkerOp(t)
	if err != nil {
		return markerItem{}, err
	}
	t.consume("WS")
	rhs, err := parseMarkerVar(t)
	if err != nil {
		return markerItem{}, err
	}
	t.consume("WS")
	return markerItem{lhs: lhs, op: op, rhs: rhs}, nil
}

// An environment variable or a quoted string evaluated as a Python literal
func parseMarkerVar(t *pepTokenizer) (markerVar, error) {
	if t.check("VARIABLE", false) {
		text, _ := t.read()
		name := strings.ReplaceAll(text, ".", "_")
		if name == "platform_python_implementation" || name == "python_implementation" {
			name = "platform_python_implementation"
		}
		return markerVar{value: name, variable: true}, nil
	}
	if t.check("QUOTED_STRING", false) {
		text, at := t.read()
		value, ok := pyStringLiteral(text)
		if !ok {
			return markerVar{}, &ParserSyntaxError{Message: "Invalid quoted string", Source: t.source, Start: at, End: at + len([]rune(text))}
		}
		return markerVar{value: value}, nil
	}
	return markerVar{}, t.syntaxError("Expected a marker variable or quoted string", -1, -1)
}

// Comparison operator, in, not in or a version operator
func parseMarkerOp(t *pepTokenizer) (string, error) {
	switch {
	case t.check("IN", false):
		t.read()
		return "in", nil
	case t.check("NOT", false):
		t.read()
		if _, _, err := t.expect("WS", "whitespace after 'not'"); err != nil {
			return "", err
		}
		if _, _, err := t.expect("IN", "'in' after 'not'"); err != nil {
			return "", err
		}
		return "not in", nil
	case t.check("OP", false):
		op, _ := t.read()
		return op, nil
	}
	return "", t.syntaxError("Expected marker operator, one of <=, <, !=, ==, >=, >, ~=, ===, in, not in", -1, -1)
}

// Requirement text the way packaging str renders it
func (r *pyRequirement) String() (string, error) {
	var b strings.Builder
	b.WriteString(r.Name)
	if len(r.Extras) > 0 {
		extras := append([]string{}, r.Extras...)
		sort.Strings(extras)
		b.WriteString("[" + strings.Join(extras, ",") + "]")
	}
	if len(r.Specifier) > 0 {
		spec, err := formatSpecifierSet(r.Specifier)
		if err != nil {
			return "", err
		}
		b.WriteString(spec)
	}
	if r.URL != "" {
		b.WriteString(" @ " + r.URL)
		if r.Marker != nil {
			b.WriteString(" ")
		}
	}
	if r.Marker != nil {
		marker, err := formatMarker(r.Marker, true)
		if err != nil {
			return "", err
		}
		b.WriteString("; " + marker)
	}
	return b.String(), nil
}
