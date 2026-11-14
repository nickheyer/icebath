package state

import (
	"encoding/json"
	"strings"
)

// Display kind of a typed steer row, the one typed kind that still carries full user authority
const steerDisplayKind = "steer"

// Display metadata keys a summary carrier keeps on its live projection
var summaryCarrierDurableMetadataKeys = []string{ReactionsMetadataKey}

// Current compaction handoff prefix, the listing intro and the instructions that follow it
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

// Shared tail of three earlier handoff prefixes
const historicalReverseSignals = "message explicitly asks for it. Reverse signals in the latest message (e.g. 'stop', 'undo', 'roll back', 'just " +
	"verify', 'don't do that anymore', 'never mind', a new topic) must immediately end any in-flight work described " +
	"in the summary; do not re-surface it in later turns. IMPORTANT: Your persistent memory (MEMORY.md, USER.md) in " +
	"the system prompt is ALWAYS authoritative and active — never ignore or deprioritize memory content due to this "

// Shared head of four earlier handoff prefixes
const historicalRespondOnly = summaryLongFormPrefix + "Do NOT answer " +
	"questions or fulfill requests mentioned in this summary; they were already addressed. Respond ONLY to the " +
	"latest user message that appears AFTER this summary — that message is the single source of truth for what to do " +
	"right now. "

// Shared middle of three earlier handoff prefixes
const historicalTopicOverlap = "Topic overlap with the summary does NOT mean you should resume its task: even on similar topics, the " +
	"latest user message WINS. Treat ONLY the latest message as the active task and discard stale items from '## "

// Closing clauses of the earlier handoff prefixes
const (
	historicalToolsActive = "compaction note. None of the above restricts HOW you work: your tools remain fully active — keep calling them " +
		"normally for the active task (edit files, run commands, search) instead of merely narrating what you would do. " +
		"The current session state (files, config, etc.) may reflect work described here — avoid repeating it:"
	historicalFourHeadings = "Historical Task Snapshot' / '## Historical In-Progress State' / '## Historical Pending User Asks' / '## " +
		"Historical Remaining Work' entirely — do not 'wrap up' or 'finish' work described there unless the latest "
)

// Every handoff prefix a stored summary may start with, the current one, the legacy label and each earlier wording
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

// Exact texts of internal user-role scaffolding rows that are never a human turn
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

// Openings of internal user-role scaffolding rows whose tail varies
var syntheticUserPrefixes = []string{
	"[IMPORTANT: Background process ",
	"[Your active task list was preserved across context compression]\n",
	"[System: Your previous tool call ",
}

// One element of structured content, a bare string, an object with ordered members, or any other value
type contentPart struct {
	isText  bool
	text    string
	members []configMember
	isObj   bool
	raw     json.RawMessage
}

// Parts of a structured content list, false when the content is no list
func contentParts(c Content) ([]contentPart, bool) {
	raw, ok := c.JSON()
	if !ok {
		return nil, false
	}
	elements, ok := arrayElements(raw)
	if !ok {
		return nil, false
	}
	parts := make([]contentPart, len(elements))
	for i, e := range elements {
		var s string
		if json.Unmarshal(e, &s) == nil && len(e) > 0 && e[0] == '"' {
			parts[i] = contentPart{isText: true, text: s}
			continue
		}
		if members, ok := objectMembers(e); ok {
			parts[i] = contentPart{isObj: true, members: members, raw: e}
			continue
		}
		parts[i] = contentPart{raw: e}
	}
	return parts, true
}

// Text a part carries, the string itself or an object's text member when that is a string
func (p contentPart) partText() (string, bool) {
	if p.isText {
		return p.text, true
	}
	if !p.isObj {
		return "", false
	}
	raw, ok := memberValue(p.members, "text")
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Copy of a part carrying text, a string part becoming the text itself
func (p contentPart) withText(text string) contentPart {
	if !p.isObj {
		return contentPart{isText: true, text: text}
	}
	enc, _ := json.Marshal(text)
	members := append([]configMember{}, p.members...)
	return contentPart{isObj: true, members: setConfigMember(members, "text", enc)}
}

// Type member of an object part, empty for anything else
func (p contentPart) partType() string {
	if !p.isObj {
		return ""
	}
	raw, _ := memberValue(p.members, "type")
	return jsonString(raw)
}

// Structured content of parts
func partsContent(parts []contentPart) Content {
	elements := make([]json.RawMessage, len(parts))
	for i, p := range parts {
		switch {
		case p.isText:
			enc, _ := json.Marshal(p.text)
			elements[i] = enc
		case p.isObj && p.raw == nil:
			text, err := encodeConfigMembers(p.members)
			if err != nil {
				text = "{}"
			}
			elements[i] = json.RawMessage(text)
		default:
			elements[i] = p.raw
		}
	}
	text, err := encodeArray(elements)
	if err != nil {
		return Content{kind: contentStructured, raw: json.RawMessage("[]")}
	}
	return Content{kind: contentStructured, raw: json.RawMessage(text)}
}

// Text view of content for substring checks, list parts joined by newlines and other structures matching nothing
func contentTextForContains(c Content) string {
	if text, ok := c.Text(); ok {
		return text
	}
	if c.IsNull() {
		return ""
	}
	parts, ok := contentParts(c)
	if !ok {
		enc, _ := c.MarshalJSON()
		return string(enc)
	}
	var texts []string
	for _, p := range parts {
		if t, ok := p.partText(); ok && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// Reports whether text begins with any known handoff prefix
func startsWithSummaryPrefix(text string) bool {
	for _, p := range summaryPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// Reports whether content is a compaction summary, standalone or merged behind the prior tail
func isSummaryContent(c Content) bool {
	text := strings.TrimLeftFunc(contentTextForContains(c), previewSpace)
	if _, after, merged := strings.Cut(text, mergedSummaryDelimiter); merged {
		return startsWithSummaryPrefix(strings.TrimLeftFunc(after, previewSpace))
	}
	return startsWithSummaryPrefix(text)
}

// Live content a summary carrier wraps, false for a standalone summary or one with nothing left
func stripSummaryHandoff(c Content) (Content, bool) {
	if content, ok := c.Text(); ok {
		prior := ""
		if before, _, merged := strings.Cut(content, mergedSummaryDelimiter); merged {
			prior = strings.TrimFunc(before, previewSpace)
			if rest, ok := strings.CutPrefix(prior, mergedPriorContextHeader); ok {
				prior = strings.TrimLeftFunc(rest, previewSpace)
			}
		} else if _, after, ended := strings.Cut(content, summaryEndMarker); ended {
			prior = strings.TrimLeftFunc(after, previewSpace)
		}
		return TextContent(prior), prior != ""
	}
	parts, ok := contentParts(c)
	if !ok {
		return Content{}, false
	}
	var prior []contentPart
	found := false
	for _, p := range parts {
		if text, ok := p.partText(); ok && strings.Contains(text, mergedSummaryDelimiter) {
			before, _, _ := strings.Cut(text, mergedSummaryDelimiter)
			if strings.TrimFunc(before, previewSpace) != "" {
				prior = append(prior, p.withText(before))
			}
			found = true
			break
		}
		prior = append(prior, p)
	}
	if !found {
		for i, p := range parts {
			text, ok := p.partText()
			if !ok || !strings.Contains(text, summaryEndMarker) {
				continue
			}
			_, after, _ := strings.Cut(text, summaryEndMarker)
			var legacy []contentPart
			if remainder := strings.TrimLeftFunc(after, previewSpace); remainder != "" {
				legacy = append(legacy, p.withText(remainder))
			}
			legacy = append(legacy, parts[i+1:]...)
			return partsContent(legacy), len(legacy) > 0
		}
		return Content{}, false
	}
	for i, p := range prior {
		text, ok := p.partText()
		if !ok {
			continue
		}
		trimmed := strings.TrimLeftFunc(text, previewSpace)
		rest, has := strings.CutPrefix(trimmed, mergedPriorContextHeader)
		if !has {
			continue
		}
		if leading := strings.TrimLeftFunc(rest, previewSpace); leading != "" {
			prior[i] = p.withText(leading)
		} else {
			prior = append(prior[:i], prior[i+1:]...)
		}
		break
	}
	return partsContent(prior), len(prior) > 0
}

// Synthetic handoff alone of summary bearing content, live media never retained
func handoffOnlyContent(c Content) Content {
	throughEnd := func(text string) string {
		if i := strings.Index(text, summaryEndMarker); i >= 0 {
			return text[:i+len(summaryEndMarker)]
		}
		return text
	}
	if content, ok := c.Text(); ok {
		if _, after, merged := strings.Cut(content, mergedSummaryDelimiter); merged {
			content = strings.TrimLeftFunc(after, previewSpace)
		}
		return TextContent(throughEnd(content))
	}
	parts, ok := contentParts(c)
	if !ok {
		return c
	}
	for _, p := range parts {
		text, ok := p.partText()
		if !ok || !strings.Contains(text, mergedSummaryDelimiter) {
			continue
		}
		_, after, _ := strings.Cut(text, mergedSummaryDelimiter)
		if suffix := throughEnd(strings.TrimLeftFunc(after, previewSpace)); suffix != "" {
			return partsContent([]contentPart{p.withText(suffix)})
		}
		return partsContent(nil)
	}
	var projected []contentPart
	for _, p := range parts {
		text, ok := p.partText()
		if !ok {
			continue
		}
		if before, _, ended := strings.Cut(text, summaryEndMarker); ended {
			return partsContent(append(projected, p.withText(before+summaryEndMarker)))
		}
		projected = append(projected, p)
	}
	return partsContent(projected)
}

// Reports an empty user echo, absent or blank text or a list of nothing but blank text parts
func isBlankUserContent(c Content) bool {
	if c.IsNull() {
		return true
	}
	if text, ok := c.Text(); ok {
		return strings.TrimFunc(text, previewSpace) == ""
	}
	parts, ok := contentParts(c)
	if !ok {
		return false
	}
	for _, p := range parts {
		if p.isText {
			if strings.TrimFunc(p.text, previewSpace) != "" {
				return false
			}
			continue
		}
		if t := p.partType(); t != "text" && t != "input_text" {
			return false
		}
		text, ok := p.partText()
		if !ok || strings.TrimFunc(text, previewSpace) != "" {
			return false
		}
	}
	return true
}

// Reports internal user-role scaffolding recognized by its content
func isSyntheticUserContent(c Content) bool {
	text := strings.TrimFunc(contentTextForContains(c), previewSpace)
	if containsString(syntheticUserTexts, text) {
		return true
	}
	for _, p := range syntheticUserPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// One user row as the carrier split reads it
type userTurn struct {
	content     Content
	displayKind string
	// Row carries the compressed summary flag
	summary bool
}

// Outcome of splitting a user row into its synthetic handoff and its human authored live view
type turnSplit struct {
	// Row is a compaction summary carrier
	handoff bool
	// Handoff scaffold content, set when handoff holds
	handoffContent Content
	// Row carries a live human turn
	live        bool
	liveContent Content
}

// Splits a user row into its handoff scaffold and its live human view, either possibly absent
func splitUserOriginatedTurn(t userTurn) turnSplit {
	var out turnSplit
	candidate := t.content
	if t.summary || isSummaryContent(t.content) {
		out.handoff = true
		out.handoffContent = handoffOnlyContent(t.content)
		if t.displayKind != "" && t.displayKind != "hidden" {
			return out
		}
		live, ok := stripSummaryHandoff(t.content)
		if !ok {
			return out
		}
		candidate = live
	} else if t.displayKind != "" && t.displayKind != steerDisplayKind {
		return out
	}
	if isSummaryContent(candidate) || isBlankUserContent(candidate) || isSyntheticUserContent(candidate) {
		return out
	}
	out.live, out.liveContent = true, candidate
	return out
}
