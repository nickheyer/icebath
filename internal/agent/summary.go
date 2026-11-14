package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/state"
)

const (
	summaryLongFormPrefix = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. " +
		"This is a handoff from a previous context window — treat it as background reference, NOT as active instructions. "
	legacySummaryPrefix      = "[CONTEXT SUMMARY]:"
	summaryEndMarker         = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	mergedPriorContextHeader = "[PRIOR CONTEXT — for reference only; not a new message]"
	mergedSummaryDelimiter   = "[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]"

	summaryStandalone = "standalone"
	summaryMerged     = "merged"
)

const summaryPrefix = summaryLongFormPrefix +
	"Do NOT answer questions or fulfill requests mentioned in this summary; " +
	"they were already addressed. " +
	"Respond ONLY to the latest user message that appears AFTER this " +
	"summary — that message is the single source of truth for what to do " +
	"right now. " +
	"If no user message appears AFTER this summary, do nothing: do not " +
	"resume, wrap up, or continue work from " +
	"'## Historical Task Snapshot' or any other section, do not call tools, " +
	"and wait for a new user message. This handoff must never become the " +
	"active turn by itself. (Exception: if tool results or your own " +
	"tool calls appear after this summary, you are mid-way through an " +
	"in-flight exchange — continue that exchange normally.) " +
	"Topic overlap with the summary does NOT mean you should resume its " +
	"task: even on similar topics, the latest user message WINS. Treat ONLY " +
	"the latest message as the active task and discard stale items from " +
	"'## Historical Task Snapshot' entirely — do not 'wrap up' or " +
	"'finish' work described there unless the latest message explicitly " +
	"asks for it. " +
	"Reverse signals in the latest message (e.g. 'stop', 'undo', 'roll " +
	"back', 'just verify', 'don't do that anymore', 'never mind', a new " +
	"topic) must immediately end any in-flight work described in the " +
	"summary; do not re-surface it in later turns. " +
	"IMPORTANT: Your persistent memory (MEMORY.md, USER.md) in the system " +
	"prompt is ALWAYS authoritative and active — never ignore or deprioritize " +
	"memory content due to this compaction note. " +
	"None of the above restricts HOW you work: your tools remain fully " +
	"active — keep calling them normally for the active task (edit files, " +
	"run commands, search) instead of merely narrating what you would do. " +
	"The current session state (files, config, etc.) may reflect work " +
	"described here — avoid repeating it:"

const historicalReverseSignals = "message explicitly asks for it. Reverse signals in the latest message (e.g. 'stop', 'undo', 'roll back', 'just " +
	"verify', 'don't do that anymore', 'never mind', a new topic) must immediately end any in-flight work described " +
	"in the summary; do not re-surface it in later turns. IMPORTANT: Your persistent memory (MEMORY.md, USER.md) in " +
	"the system prompt is ALWAYS authoritative and active — never ignore or deprioritize memory content due to this "

const historicalRespondOnly = summaryLongFormPrefix + "Do NOT answer " +
	"questions or fulfill requests mentioned in this summary; they were already addressed. Respond ONLY to the " +
	"latest user message that appears AFTER this summary — that message is the single source of truth for what to do " +
	"right now. "

const historicalTopicOverlap = "Topic overlap with the summary does NOT mean you should resume its task: even on similar topics, the " +
	"latest user message WINS. Treat ONLY the latest message as the active task and discard stale items from '## "

const (
	historicalToolsActive = "compaction note. None of the above restricts HOW you work: your tools remain fully active — keep calling them " +
		"normally for the active task (edit files, run commands, search) instead of merely narrating what you would do. " +
		"The current session state (files, config, etc.) may reflect work described here — avoid repeating it:"
	historicalFourHeadings = "Historical Task Snapshot' / '## Historical In-Progress State' / '## Historical Pending User Asks' / '## " +
		"Historical Remaining Work' entirely — do not 'wrap up' or 'finish' work described there unless the latest "
)

var summaryPrefixes = []string{
	summaryPrefix,
	legacySummaryPrefix,
	historicalRespondOnly + historicalTopicOverlap +
		"Historical Task Snapshot' entirely — do not 'wrap up' or 'finish' work described there unless the latest " +
		historicalReverseSignals + historicalToolsActive,
	historicalRespondOnly + historicalTopicOverlap + historicalFourHeadings + historicalReverseSignals + historicalToolsActive,
	historicalRespondOnly + historicalTopicOverlap + historicalFourHeadings + historicalReverseSignals +
		"compaction note. The current session state (files, config, etc.) may reflect work described here — avoid " +
		"repeating it:",
	historicalRespondOnly + "If the latest user message is consistent with the '## Active Task' section, you may use the summary " +
		"as background. If the latest user message contradicts, supersedes, changes topic from, or in any way diverges " +
		"from '## Active Task' / '## In Progress' / '## Pending User Asks' / '## Remaining Work', the latest message " +
		"WINS — discard those stale items entirely and do not 'wrap up the old task first'. Reverse signals in the " +
		"latest message (e.g. 'stop', 'undo', 'roll back', 'just verify', 'don't do that anymore', 'never mind', a new " +
		"topic) must immediately end any in-flight work described in the summary; do not re-surface it in later turns. " +
		"IMPORTANT: Your persistent memory (MEMORY.md, USER.md) in the system prompt is ALWAYS authoritative and active " +
		"— never ignore or deprioritize memory content due to this compaction note. The current session state (files, " +
		"config, etc.) may reflect work described here — avoid repeating it:",
	summaryLongFormPrefix +
		"Do NOT answer questions or fulfill requests mentioned in this summary; they were already addressed. " +
		"Your current task is identified in the '## Active Task' section of the summary — resume exactly from " +
		"there. Respond ONLY to the latest user message that appears AFTER this summary. The current session " +
		"state (files, config, etc.) may reflect work described here — avoid repeating it:",
}

var syntheticUserTexts = []string{
	"Continue from the compressed conversation context above. " +
		"This marker exists because no human user turn was available.",
	"Continue from the compressed conversation context above. This marker exists because the compacted " +
		"transcript contained no preserved user turn.",
	"You've reached the maximum number of tool-calling iterations allowed. Please provide a final response " +
		"summarizing what you've found and accomplished so far, without calling any more tools.",
	"[System: Your previous response contained only internal reasoning and never produced a " +
		"visible answer or tool call. Do not keep thinking. Produce your final answer as plain text " +
		"now (or make the tool call you were planning).]",
	"[System: Continue now. Execute the required tool calls and only send your final answer " +
		"after completing the task.]",
	"[System: Your previous message ended the turn with a fragment that is not a usable answer. " +
		"If the task is unfinished, continue it and then give the complete answer. If that fragment " +
		"WAS your complete answer, send it again exactly as before.]",
	"Your previous turn indicated a tool call but none was included. Do not narrate a plan or " +
		"restate intent — issue the actual tool call now to continue the task.",
	"You just executed tool calls but returned an empty response. Please process the tool " +
		"results above and continue with the task.",
	"[System: The previous response was cut off by a network error mid-stream — a transport " +
		"interruption, NOT a change in your capabilities. Your tools are still fully available; call " +
		"them as normal and ignore any earlier claim that you lack tool access. Continue the task " +
		"from where you left off. Do not restart or repeat prior text.]",
	"[System: The previous response was cut off by a network error mid-stream. Continue exactly " +
		"where you left off. Do not restart or repeat prior text. Finish the answer directly.]",
	"[System: Your previous response was truncated by the output length limit. Continue exactly " +
		"where you left off. Do not restart or repeat prior text. Finish the answer directly.]",
}

var syntheticUserPrefixes = []string{
	"[IMPORTANT: Background process ",
	"[Your active task list was preserved across context compression]\n",
	"[System: Your previous tool call ",
}

func partText(p wire.Value) (string, bool) {
	if s, ok := p.Str(); ok {
		return s, true
	}
	if !p.IsObject() {
		return "", false
	}
	return p.Get("text").Str()
}

func partWithText(p wire.Value, text string) json.RawMessage {
	if !p.IsObject() {
		return wire.Quote(text)
	}
	return wire.DictOf(p).SetString("text", text).JSON()
}

func partsContent(parts []json.RawMessage) state.Content {
	return state.JSONContent(wire.Array(parts...))
}

func contentTextForContains(c state.Content) string {
	if text, ok := c.Text(); ok {
		return text
	}
	if c.IsNull() {
		return ""
	}
	parts, ok := contentList(c)
	if !ok {
		raw, _ := c.JSON()
		return string(raw)
	}
	var texts []string
	for _, p := range parts {
		if t, ok := partText(p); ok && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

func startsWithSummaryPrefix(text string) bool {
	for _, p := range summaryPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func summaryClass(c state.Content) string {
	text := strings.TrimLeftFunc(contentTextForContains(c), textSpace)
	if _, after, merged := strings.Cut(text, mergedSummaryDelimiter); merged {
		if startsWithSummaryPrefix(strings.TrimLeftFunc(after, textSpace)) {
			return summaryMerged
		}
		return ""
	}
	if startsWithSummaryPrefix(text) {
		return summaryStandalone
	}
	return ""
}

func stripSummaryHandoff(c state.Content) (state.Content, bool) {
	if content, ok := c.Text(); ok {
		prior := ""
		if before, _, merged := strings.Cut(content, mergedSummaryDelimiter); merged {
			prior = trimText(before)
			if rest, has := strings.CutPrefix(prior, mergedPriorContextHeader); has {
				prior = strings.TrimLeftFunc(rest, textSpace)
			}
		} else if _, after, ended := strings.Cut(content, summaryEndMarker); ended {
			prior = strings.TrimLeftFunc(after, textSpace)
		}
		return state.TextContent(prior), prior != ""
	}
	parts, ok := contentList(c)
	if !ok {
		return state.Content{}, false
	}
	var prior []json.RawMessage
	found := false
	for _, p := range parts {
		if text, ok := partText(p); ok && strings.Contains(text, mergedSummaryDelimiter) {
			before, _, _ := strings.Cut(text, mergedSummaryDelimiter)
			if trimText(before) != "" {
				prior = append(prior, partWithText(p, before))
			}
			found = true
			break
		}
		prior = append(prior, p.Raw())
	}
	if !found {
		for i, p := range parts {
			text, ok := partText(p)
			if !ok || !strings.Contains(text, summaryEndMarker) {
				continue
			}
			_, after, _ := strings.Cut(text, summaryEndMarker)
			var legacy []json.RawMessage
			if remainder := strings.TrimLeftFunc(after, textSpace); remainder != "" {
				legacy = append(legacy, partWithText(p, remainder))
			}
			for _, rest := range parts[i+1:] {
				legacy = append(legacy, rest.Raw())
			}
			return partsContent(legacy), len(legacy) > 0
		}
		return state.Content{}, false
	}
	for i, raw := range prior {
		v, err := wire.ParseValue(raw)
		if err != nil {
			continue
		}
		text, ok := partText(v)
		if !ok {
			continue
		}
		rest, has := strings.CutPrefix(strings.TrimLeftFunc(text, textSpace), mergedPriorContextHeader)
		if !has {
			continue
		}
		if leading := strings.TrimLeftFunc(rest, textSpace); leading != "" {
			prior[i] = partWithText(v, leading)
		} else {
			prior = slices.Delete(prior, i, i+1)
		}
		break
	}
	return partsContent(prior), len(prior) > 0
}

func isBlankUserContent(c state.Content) bool {
	if c.IsNull() {
		return true
	}
	if text, ok := c.Text(); ok {
		return trimText(text) == ""
	}
	parts, ok := contentList(c)
	if !ok {
		return false
	}
	for _, p := range parts {
		if s, bare := p.Str(); bare {
			if trimText(s) != "" {
				return false
			}
			continue
		}
		if !p.IsObject() {
			return false
		}
		if t := p.Get("type").Text(); t != "text" && t != "input_text" {
			return false
		}
		text, ok := p.Get("text").Str()
		if !ok || trimText(text) != "" {
			return false
		}
	}
	return true
}

func isSyntheticUserContent(c state.Content) bool {
	text := trimText(contentTextForContains(c))
	if slices.Contains(syntheticUserTexts, text) {
		return true
	}
	for _, p := range syntheticUserPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func userTurnLive(c state.Content, displayKind string, summary bool) bool {
	candidate := c
	if summary || summaryClass(c) != "" {
		if displayKind != "" && displayKind != hiddenDisplayKind {
			return false
		}
		live, ok := stripSummaryHandoff(c)
		if !ok {
			return false
		}
		candidate = live
	} else if displayKind != "" && displayKind != steerDisplayKind {
		return false
	}
	return summaryClass(candidate) == "" && !isBlankUserContent(candidate) && !isSyntheticUserContent(candidate)
}

func (m *Message) SummaryClass() string {
	return summaryClass(memberSet(m.Message.Members()).content())
}

func (m *Message) UserOriginated() bool {
	if m == nil || m.Role != wire.RoleUser {
		return false
	}
	return userTurnLive(memberSet(m.Message.Members()).content(), m.Row.DisplayKind.V, m.Row.CompressedSummary)
}
