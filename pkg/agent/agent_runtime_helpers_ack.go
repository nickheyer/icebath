package agent

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	ackFutureRE        = mustPyRE(`\b(i['’]ll|i will|let me|i can do that|i can help with that)\b`)
	ackActionMarkers   = []string{"look into", "look at", "inspect", "scan", "check", "analyz", "review", "explore", "read", "open", "run", "test", "fix", "debug", "search", "find", "walkthrough", "report back", "summarize"}
	ackWorkspaceMarker = []string{"directory", "current directory", "current dir", "cwd", "repo", "repository", "codebase", "project", "folder", "filesystem", "file tree", "files", "path"}

	ackActionClauseSrc = `(?:^|[.!…—–]\s+|\n+\s*|,\s+then\s+)` +
		`(?:(?P<transition>then)\s+)?` +
		`(?P<action>(?:re)?launching|(?:re)?starting|creating|` +
		`checking|running|writing|opening|reading|inspecting|reviewing|` +
		`testing|debugging|searching|fixing)\b`
	ackActionClauseRE     = mustPyRE(ackActionClauseSrc)
	ackActionGroup        = mustPyGroup(ackActionClauseSrc, "action")
	ackListItemPrefixRE   = mustPyRE(`(?:^|\n)\s*(?:[-*+]|\d+[.)])\s*$`)
	ackNumberedItemRE     = mustPyRE(`(?:^|\s)(\d+)[.)](?=\s)`)
	ackStatusNumberSrc    = `\b(?P<label>exit\s+code|attempt|step|retry)\s+(?P<number>\d+)[.)](?=\s)`
	ackStatusNumberRE     = mustPyRE(ackStatusNumberSrc)
	ackLabelGroup         = mustPyGroup(ackStatusNumberSrc, "label")
	ackNumberGroup        = mustPyGroup(ackStatusNumberSrc, "number")
	ackQuestionRE         = mustPyRE(`\?(?=\s|$|["'’”)\\])`)
	ackSentenceBoundaryRE = mustPyRE(`(?:[.!…](?=\s|$)|\?(?=\s|$|["'’”)\\])|\n+)`)
	ackAllowedTrailingRE  = mustPyFullmatch(`\s*(?:will\s+report\s+back|` +
		`then\s+(?:launching|relaunching|starting|restarting|creating)\s+` +
		`(?:it|(?:the\s+)?(?:session|worker|agent|job|process))|` +
		`(?:checking|reading|opening|inspecting)\s+(?:the\s+)?(?:log|output))` +
		`\s*[.!…]?\s*$`)
	ackLaunchNowTailRE = mustPyFullmatch(`\s+(?:(?:it|(?:the\s+)?(?:session|worker|agent|job|process)|` +
		`(?:the\s+)?migration\s+file\s+in\s+the\s+repo)\s+)?` +
		`now(?:\s*\([^)]*\))?\s*$`)
	ackRunNowTailRE = mustPyFullmatch(`\s+(?:(?:it|(?:the\s+)?(?:(?:repo\s+)?suite|tests?|job|process|` +
		`server|service))\s+)?now(?:\s*\([^)]*\))?\s*$`)
	ackCheckNowTailRE = mustPyFullmatch(`\s+(?:the\s+)?(?:log|output|status|service|session|job)\s+` +
		`now(?:\s*\([^)]*\))?\s*$`)
	ackLaunchURLTailRE  = mustPyFullmatch(`\s+now\s*[—–-]\s*see\s+https?://\S+(?:\s+for\s+progress)?\s*$`)
	ackActualLogTailRE  = mustPyFullmatch(`\s+(?:the\s+)?actual\s+(?:log|output)\s*$`)
	ackHealthCheckRE    = mustPyFullmatch(`\s+whether\b.*\b(?:healthy|ready|running|available|reachable|working)\s*$`)
	ackLaunchWithTailRE = mustPyFullmatch(`\s+with\s+(?:(?:corrected|updated|new)\s+` +
		`(?:arguments?|args?|options?|flags?|parameters?)|` +
		`globals?\s+before\s+(?:the\s+)?agent\s+name)\s*$`)
	ackLaunchActions = map[string]bool{"launching": true, "relaunching": true, "starting": true, "restarting": true, "creating": true}
	ackLogActions    = map[string]bool{"checking": true, "reading": true, "opening": true, "inspecting": true}
)

// Group number of a named group in a Python pattern
func mustPyGroup(pattern, name string) int {
	idx, err := pyre.GroupIndex(pattern)
	if err != nil {
		panic(err)
	}
	return idx[name]
}

// Whether the pattern finds a match anywhere in the text
func pyReSearch(re *regexp2.Regexp, text string) bool {
	m, _ := re.FindStringMatch(text)
	return m != nil
}

// Every non overlapping match in order
func pyReFinditer(re *regexp2.Regexp, text string) []*regexp2.Match {
	out := []*regexp2.Match{}
	m, _ := re.FindStringMatch(text)
	for m != nil {
		out = append(out, m)
		m, _ = re.FindNextMatch(m)
	}
	return out
}

// Fullmatch pattern of a clause relaunched on an external process provider by name, alias or binary
func ackProviderTailRE(ctx context.Context) *regexp2.Regexp {
	seen := map[string]bool{}
	for _, p := range provider.ListProviders(ctx) {
		if p.AuthType != provider.AuthTypeExternalProcess {
			continue
		}
		for _, n := range append(append([]string{p.Name}, p.Aliases...), p.ProcessCommand) {
			if n != "" {
				seen[pystr.Lower(n)] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Strings(names)
	alt := strings.Join(names, "|")
	if alt == "" {
		alt = "(?!)"
	}
	return mustPyFullmatch(`\s+(?:it|(?:the\s+)?(?:session|worker|agent|job|process))\s+on\s+(?:` + alt + `)(?:\s+via\s+acpx)?\s*$`)
}

// Whether a pronounless action clause with an explicit transition cue ends the reply
func hasPronounlessAction(ctx context.Context, text string) bool {
	stepNumbers := map[string]bool{}
	for _, m := range pyReFinditer(ackStatusNumberRE, text) {
		if m.GroupByNumber(ackLabelGroup).String() == "step" {
			stepNumbers[m.GroupByNumber(ackNumberGroup).String()] = true
		}
	}
	numberingText, _ := ackStatusNumberRE.Replace(text, "", -1, -1)
	hasNumberedList := len(stepNumbers) >= 2 || len(pyReFinditer(ackNumberedItemRE, numberingText)) > 0
	if pyReSearch(ackQuestionRE, text) {
		return false
	}
	runes := []rune(text)
	var providerTail *regexp2.Regexp
	for _, m := range pyReFinditer(ackActionClauseRE, text) {
		action := m.GroupByNumber(ackActionGroup)
		if hasNumberedList || pyReSearch(ackListItemPrefixRE, string(runes[:action.RuneIndex])) {
			continue
		}
		rawTail := string(runes[m.RuneIndex+m.RuneLength:])
		clauseTail, remaining := rawTail, ""
		if b, _ := ackSentenceBoundaryRE.FindStringMatch(rawTail); b != nil {
			tailRunes := []rune(rawTail)
			clauseTail, remaining = string(tailRunes[:b.RuneIndex]), string(tailRunes[b.RuneIndex+b.RuneLength:])
		}
		if pystr.Strip(remaining) != "" && !pyReSearch(ackAllowedTrailingRE, remaining) {
			continue
		}
		word := action.String()
		launch := ackLaunchActions[word]
		cue := (launch && pyReSearch(ackLaunchNowTailRE, clauseTail)) ||
			(word == "running" && pyReSearch(ackRunNowTailRE, clauseTail)) ||
			(word == "checking" && pyReSearch(ackCheckNowTailRE, clauseTail)) ||
			(launch && pyReSearch(ackLaunchURLTailRE, clauseTail))
		if !cue && launch {
			if providerTail == nil {
				providerTail = ackProviderTailRE(ctx)
			}
			cue = pyReSearch(providerTail, clauseTail)
		}
		cue = cue || (ackLogActions[word] && pyReSearch(ackActualLogTailRE, clauseTail)) ||
			(word == "checking" && pyReSearch(ackHealthCheckRE, clauseTail)) ||
			(launch && pyReSearch(ackLaunchWithTailRE, clauseTail))
		if cue {
			return true
		}
	}
	return false
}

// Planning or acknowledgement reply that should continue the turn instead of ending it
func (a *AIAgent) looksLikeCodexIntermediateAck(userMessage any, assistantContent string, messages []*state.Message, requireWorkspace bool) bool {
	for _, msg := range messages {
		if msg != nil && msg.Role == "tool" {
			return false
		}
	}
	text := pystr.Lower(pystr.Strip(a.stripThinkBlocks(assistantContent)))
	if text == "" || pyLen(text) > 1200 {
		return false
	}
	pronounless := hasPronounlessAction(a.homeContext(), text)
	if !pyReSearch(ackFutureRE, text) && !pronounless {
		return false
	}
	if !pronounless && !containsAny(text, ackActionMarkers...) {
		return false
	}
	if !requireWorkspace {
		return true
	}
	userText := pystr.Lower(pystr.Strip(provider.SummarizeUserMessageForLog(userMessage, " ")))
	return containsAny(userText, ackWorkspaceMarker...) || strings.Contains(userText, "~/") || strings.Contains(userText, "/") ||
		containsAny(text, ackWorkspaceMarker...)
}
