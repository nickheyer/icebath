// Searches stored session messages through the full-text, trigram and CJK indexes with LIKE fallbacks
package search

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/state"
)

const fts5Special = `+{}():"^@/#&|~[]<>,;!?$=\'`

var (
	quotedPhraseRE = regexp.MustCompile(`"[^"]*"`)
	starRunRE      = regexp.MustCompile(`\*+`)
	leadingStarRE  = regexp.MustCompile(`(^|[\t\n\v\f\r\x{85}\p{Z}])\*`)
	dottedTermRE   = regexp.MustCompile(`[\p{L}\p{N}_]+(?:[._-][\p{L}\p{N}_]+)+`)
	likeTokenRE    = regexp.MustCompile(`"[^"]+"|[^\t\n\v\f\r\x{85}\p{Z}]+`)
)

var cjkRanges = [][2]rune{
	{0x4E00, 0x9FFF}, {0x3400, 0x4DBF}, {0x20000, 0x2A6DF}, {0x3000, 0x303F}, {0x3040, 0x309F},
	{0x30A0, 0x30FF}, {0xAC00, 0xD7AF},
}

func isCJK(r rune) bool {
	for _, rg := range cjkRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

func containsCJK(s string) bool {
	return strings.IndexFunc(s, isCJK) >= 0
}

func countCJK(s string) int {
	n := 0
	for _, r := range s {
		if isCJK(r) {
			n++
		}
	}
	return n
}

func hasLoneCJKRun(q string) bool {
	run := 0
	for _, r := range q {
		if isCJK(r) {
			run++
			continue
		}
		if run == 1 {
			return true
		}
		run = 0
	}
	return run == 1
}

func isOperator(tok string) bool {
	switch strings.ToUpper(tok) {
	case "AND", "OR", "NOT":
		return true
	}
	return false
}

func nonOperatorTokens(q string) []string {
	var out []string
	for _, t := range strings.Fields(q) {
		if !isOperator(t) {
			out = append(out, t)
		}
	}
	return out
}

func quoteFTSTokens(q string) string {
	toks := strings.Fields(q)
	for i, t := range toks {
		if !isOperator(t) {
			toks[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
	}
	return strings.Join(toks, " ")
}

func likeParams(term string) []any {
	p := "%" + state.EscapeLike(term) + "%"
	return []any{p, p, p}
}

func unquoted(q string) string { return strings.TrimSpace(strings.Trim(q, `"`)) }

func stripCJKWildcards(q string) string {
	if !strings.Contains(q, "*") {
		return q
	}
	toks := strings.Fields(q)
	for i, t := range toks {
		if !isOperator(t) {
			if bare := strings.TrimRight(t, "*"); bare != "" {
				toks[i] = bare
			}
		}
	}
	if joined := strings.Join(toks, " "); joined != "" {
		return joined
	}
	return q
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

var booleanOperators = []string{"AND", "OR", "NOT"}

func stripLeadingOperator(s string) string {
	for _, op := range booleanOperators {
		if len(s) < len(op) || !strings.EqualFold(s[:len(op)], op) {
			continue
		}
		rest := s[len(op):]
		if r, _ := utf8.DecodeRuneInString(rest); rest == "" || !isWordRune(r) {
			return strings.TrimLeftFunc(rest, unicode.IsSpace)
		}
	}
	return s
}

func stripTrailingOperator(s string) string {
	for _, op := range booleanOperators {
		if len(s) <= len(op) || !strings.EqualFold(s[len(s)-len(op):], op) {
			continue
		}
		head := s[:len(s)-len(op)]
		if r, _ := utf8.DecodeLastRuneInString(head); unicode.IsSpace(r) {
			return strings.TrimRightFunc(head, unicode.IsSpace)
		}
	}
	return s
}

func sanitizeQuery(query string) string {
	query = truncateRunes(query, state.MaxFTS5QueryChars)
	var held []string
	sanitized := quotedPhraseRE.ReplaceAllStringFunc(query, func(m string) string {
		held = append(held, m)
		return "\x00Q" + strconv.Itoa(len(held)-1) + "\x00"
	})
	sanitized = strings.ReplaceAll(sanitized, `"`, " ")
	sanitized = strings.Map(func(r rune) rune {
		if strings.ContainsRune(fts5Special, r) {
			return ' '
		}
		return r
	}, sanitized)
	if strings.Contains(sanitized, "%") && !containsCJK(sanitized) {
		sanitized = strings.ReplaceAll(sanitized, "%", " ")
	}
	sanitized = starRunRE.ReplaceAllString(sanitized, "*")
	sanitized = leadingStarRE.ReplaceAllString(sanitized, "${1}")
	sanitized = stripLeadingOperator(strings.TrimSpace(sanitized))
	sanitized = stripTrailingOperator(strings.TrimSpace(sanitized))
	sanitized = dottedTermRE.ReplaceAllString(sanitized, `"${0}"`)
	for i, q := range held {
		sanitized = strings.ReplaceAll(sanitized, "\x00Q"+strconv.Itoa(i)+"\x00", q)
	}
	return strings.TrimSpace(sanitized)
}

func orRelaxedQuery(q string) (string, bool) {
	var units []string
	for _, tok := range likeTokenRE.FindAllString(q, -1) {
		switch strings.ToUpper(tok) {
		case "OR", "NOT":
			return "", false
		case "AND":
		default:
			units = append(units, tok)
		}
	}
	if len(units) < 2 {
		return "", false
	}
	return strings.Join(units, " OR "), true
}

func trigramEligibleTokens(q string) bool {
	toks := nonOperatorTokens(unquoted(q))
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if utf8.RuneCountInString(t) < 3 {
			return false
		}
	}
	return true
}

func hasShortCJKToken(raw string) bool {
	for _, t := range nonOperatorTokens(raw) {
		if containsCJK(t) && countCJK(t) < 3 {
			return true
		}
	}
	return false
}

const (
	likeAnyColumnSQL       = `(m.content LIKE ? ESCAPE '\' OR m.tool_name LIKE ? ESCAPE '\' OR m.tool_calls LIKE ? ESCAPE '\')`
	likeCoalescedColumnSQL = `(COALESCE(m.content, '') LIKE ? ESCAPE '\' OR ` +
		`COALESCE(m.tool_name, '') LIKE ? ESCAPE '\' OR ` +
		`COALESCE(m.tool_calls, '') LIKE ? ESCAPE '\')`
)

type likeTerm struct {
	text    string
	negated bool
}

func compileLikeBooleanQuery(q string) (string, []any, string) {
	groups := [][]likeTerm{nil}
	negateNext := false
	for _, tok := range likeTokenRE.FindAllString(q, -1) {
		switch strings.ToUpper(tok) {
		case "OR":
			if len(groups[len(groups)-1]) > 0 {
				groups = append(groups, nil)
			}
			negateNext = false
			continue
		case "AND", "NEAR":
			continue
		case "NOT":
			negateNext = true
			continue
		}
		term := strings.TrimSpace(strings.Trim(strings.Trim(tok, `"`), "*"))
		if term != "" {
			groups[len(groups)-1] = append(groups[len(groups)-1], likeTerm{term, negateNext})
			negateNext = false
		}
	}
	var compiled []string
	var args []any
	snippet := ""
	for _, group := range groups {
		positive := false
		for _, t := range group {
			positive = positive || !t.negated
		}
		if !positive {
			continue
		}
		var clauses []string
		for _, t := range group {
			if t.negated {
				clauses = append(clauses, "NOT "+likeCoalescedColumnSQL)
			} else {
				clauses = append(clauses, likeCoalescedColumnSQL)
				if snippet == "" {
					snippet = t.text
				}
			}
			args = append(args, likeParams(t.text)...)
		}
		compiled = append(compiled, "("+strings.Join(clauses, " AND ")+")")
	}
	return strings.Join(compiled, " OR "), args, snippet
}

func gapTerms(ftsQuery string) []string {
	var out []string
	for _, tok := range likeTokenRE.FindAllString(ftsQuery, -1) {
		t := strings.TrimSpace(strings.Trim(strings.Trim(tok, `"`), "*"))
		switch strings.ToUpper(t) {
		case "", "AND", "OR", "NOT", "NEAR":
			continue
		}
		out = append(out, t)
	}
	return out
}
