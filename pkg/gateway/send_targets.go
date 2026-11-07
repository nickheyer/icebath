package gateway

import (
	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type notExplicitMarker struct{}

// Sentinel a target syntax parser returns to skip the generic id rules and go to the channel directory
var NotExplicit any = &notExplicitMarker{}

// Chat id and optional thread id a target syntax parser recognised, nil chat id when the group did not match
type TargetRefParse struct {
	ChatID   *string
	ThreadID *string
}

// Parser explicit on a full match of the Python pattern, group one the chat id and group two the thread
func RegexGroupsParser(pattern string, threadGroup bool) func(ref string) any {
	full := fullMatchRegexp(pattern)
	return func(ref string) any {
		m, err := full.FindStringMatch(ref)
		if err != nil || m == nil {
			return nil
		}
		parsed := &TargetRefParse{ChatID: submatchGroup(m, 1)}
		if threadGroup {
			parsed.ThreadID = submatchGroup(m, 2)
		}
		return parsed
	}
}

// Parser explicit on a full match of the Python pattern that returns the whitespace stripped reference
func RegexStrippedParser(pattern string) func(ref string) any {
	full := fullMatchRegexp(pattern)
	return func(ref string) any {
		if ok, err := full.MatchString(ref); err != nil || !ok {
			return nil
		}
		stripped := pystr.Strip(ref)
		return &TargetRefParse{ChatID: &stripped}
	}
}

// Stripped reference as an explicit chat id whenever it is non empty
func ParseNonempty(ref string) any {
	stripped := pystr.Strip(ref)
	if stripped == "" {
		return nil
	}
	return &TargetRefParse{ChatID: &stripped}
}

// Routing rules for sending a platform through its standalone sender instead of the live adapter
type StandaloneSendPolicy struct {
	Label                  string
	Media                  bool
	AllSends               bool
	Captionable            bool
	EmptyListBetweenChunks bool
	ForwardForceDocument   bool
	Mentions               bool
	TextDirect             bool
}

// Python pattern compiled so a match must span the whole string like re.fullmatch, panicking like re.compile
func fullMatchRegexp(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Text of one submatch group, nil when the group took no part in the match
func submatchGroup(m *regexp2.Match, group int) *string {
	g := m.GroupByNumber(group)
	if g == nil || len(g.Captures) == 0 {
		return nil
	}
	v := g.String()
	return &v
}
