package regexutil

import (
	"iter"
	"regexp"
	"strings"

	"github.com/dlclark/regexp2/v2"
)

// Whether re matches somewhere in s, false when the match runs out of time, which only a Compile pattern can do: a decision that must not read a give-up as no match calls re.MatchString and handles its error
func Match(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return ok && err == nil
}

// Leftmost match of re in s, nil when there is none or the match runs out of time, which only a Compile pattern can do
func Find(re *regexp2.Regexp, s string) *regexp2.Match {
	m, err := re.FindStringMatch(s)
	if err != nil {
		return nil
	}
	return m
}

// Successive non-overlapping matches of re in s from left to right, ending early and silently when a match runs out of time, which only a Compile pattern can do
func All(re *regexp2.Regexp, s string) iter.Seq[*regexp2.Match] {
	return func(yield func(*regexp2.Match) bool) {
		m, err := re.FindStringMatch(s)
		for m != nil && err == nil {
			if !yield(m) {
				return
			}
			m, err = re.FindNextMatch(m)
		}
	}
}

// s with every match of re replaced by template, in which $1 or ${1}, ${name}, $0 and $$ stand for a group, a named group, the whole match and a dollar sign, a reference to a group the pattern lacks staying literal text and a group that took no part giving nothing; s unchanged when the template is malformed or a match runs out of time, which only a Compile pattern can do
func Replace(re *regexp2.Regexp, s, template string) string {
	out, err := re.Replace(s, template, -1, -1)
	if err != nil {
		return s
	}
	return out
}

// s with every match of re replaced by what fn returns for it, s unchanged when a match runs out of time, which only a Compile pattern can do
func ReplaceFunc(re *regexp2.Regexp, s string, fn func(regexp2.Match) string) string {
	out, err := re.ReplaceFunc(s, fn, -1, -1)
	if err != nil {
		return s
	}
	return out
}

// Text of group n of m, empty when m is nil, the group does not exist or it took no part in the match
func Group(m *regexp2.Match, n int) string {
	if m == nil {
		return ""
	}
	return groupText(m.GroupByNumber(n))
}

// Text of the group called name in m, empty when m is nil, the pattern has no such group or it took no part in the match
func NamedGroup(m *regexp2.Match, name string) string {
	if m == nil {
		return ""
	}
	return groupText(m.GroupByName(name))
}

func groupText(g *regexp2.Group) string {
	if g == nil || len(g.Captures) == 0 {
		return ""
	}
	return g.String()
}

// s with every match of the standard library pattern re replaced by what fn returns for its groups, groups[0] being the whole match and a group that took no part being empty
func ReplaceSubmatchFunc(re *regexp.Regexp, s string, fn func(groups []string) string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, loc := range locs {
		b.WriteString(s[last:loc[0]])
		groups := make([]string, len(loc)/2)
		for i := range groups {
			if loc[2*i] >= 0 {
				groups[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(fn(groups))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
