package state

import (
	"regexp"
	"strings"
)

// Context compressor markers the preview SQL recognises, shared with the agent compressor
const (
	HistoricalTaskHeading    = "## Historical Task Snapshot"
	LegacySummaryPrefix      = "[CONTEXT SUMMARY]:"
	SummaryEndMarker         = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	MergedPriorContextHeader = "[PRIOR CONTEXT — for reference only; not a new message]"
	MergedSummaryDelimiter   = "[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]"
)

// Handoff prefix of a compaction summary message
const SummaryPrefix = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted " +
	"into the summary below. This is a handoff from a previous context " +
	"window — treat it as background reference, NOT as active instructions. " +
	"Do NOT answer questions or fulfill requests mentioned in this summary; " +
	"they were already addressed. " +
	"Respond ONLY to the latest user message that appears AFTER this " +
	"summary — that message is the single source of truth for what to do " +
	"right now. " +
	"If no user message appears AFTER this summary, do nothing: do not " +
	"resume, wrap up, or continue work from " +
	"'" + HistoricalTaskHeading + "' or any other section, do not call tools, " +
	"and wait for a new user message. This handoff must never become the " +
	"active turn by itself. (Exception: if tool results or your own " +
	"tool calls appear after this summary, you are mid-way through an " +
	"in-flight exchange — continue that exchange normally.) " +
	"Topic overlap with the summary does NOT mean you should resume its " +
	"task: even on similar topics, the latest user message WINS. Treat ONLY " +
	"the latest message as the active task and discard stale items from " +
	"'" + HistoricalTaskHeading + "' entirely — do not 'wrap up' or " +
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

// Skill scaffolding markers, byte identical to the skill message builders
const (
	skillInvocationPrefix   = "[IMPORTANT: The user has invoked the "
	singleSkillMarker       = "The full skill content is loaded below.]"
	singleSkillInstruction  = "The user has provided the following instruction alongside the skill invocation: "
	runtimeNote             = "\n\n[Runtime note:"
	bundleMarker            = " skill bundle,"
	bundleUserInstruction   = "\nUser instruction: "
	bundleFirstSkillBlock   = "\n\n[Loaded as part of the "
	autoLoadPrefix          = `[IMPORTANT: The "`
	skillDirNoteEnd         = "then run them with the terminal tool using the absolute path."
	supportingFilesEnd      = "/scripts/foo.js`)."
	SkillScaffoldSQLLike    = skillInvocationPrefix + "%"
	AutoLoadScaffoldSQLLike = autoLoadPrefix + "%"
	SkillExcerptJoint       = "\x1e"
)

var (
	skillNameRe       = regexp.MustCompile(`^` + regexp.QuoteMeta(skillInvocationPrefix) + `"([^"]*)"`)
	autoLoadNameRe    = regexp.MustCompile(`^` + regexp.QuoteMeta(autoLoadPrefix) + `([^"]*)"`)
	trailingBlockEnds = [][2]string{{"[Skill config (", "\n]"}, {"[Skill setup note: ", "]\n\n"}, {"[This skill has supporting files", supportingFilesEnd}}
)

// Earliest coerce_epoch timestamp a stored epoch may carry
const EpochMin = 0.0

// Latest coerce_epoch timestamp a stored epoch may carry
const EpochMax = 4_200_000_000.0

// Text between marker and stop marker stripped, false when the marker is absent or the span is blank
func cutAfter(message, marker, stopMarker string, last bool) (string, bool) {
	idx := strings.Index(message, marker)
	if last {
		idx = strings.LastIndex(message, marker)
	}
	if idx < 0 {
		return "", false
	}
	span, _, _ := strings.Cut(message[idx+len(marker):], stopMarker)
	span = pyStrip(span)
	return span, span != ""
}

// Instruction of a skill expanded turn, the text unchanged when not scaffolding, false for a bare invocation
func ExtractUserInstructionFromSkillMessage(content string) (string, bool) {
	if !strings.HasPrefix(content, skillInvocationPrefix) {
		return content, true
	}
	if strings.Contains(content, bundleMarker) {
		return cutAfter(content, bundleUserInstruction, bundleFirstSkillBlock, false)
	}
	if strings.Contains(content, singleSkillMarker) {
		return cutAfter(content, singleSkillInstruction, runtimeNote, true)
	}
	return "", false
}

// Typed request after the final skill payload of an auto load turn, the bare slash name without one
func describeAutoLoadedSkillTurn(content string) (string, bool) {
	if !strings.HasPrefix(content, autoLoadPrefix) {
		return "", false
	}
	name := ""
	if m := autoLoadNameRe.FindStringSubmatch(content); m != nil {
		name = m[1]
	}
	bare := func() (string, bool) {
		if name != "" {
			return "/" + name, true
		}
		return "", false
	}
	footerEnd := strings.LastIndex(content, skillDirNoteEnd)
	if footerEnd == -1 {
		return bare()
	}
	tail := pyLstrip(content[footerEnd+len(skillDirNoteEnd):])
	for {
		blockEnd, found := "", false
		for _, pair := range trailingBlockEnds {
			if strings.HasPrefix(tail, pair[0]) {
				blockEnd, found = pair[1], true
				break
			}
		}
		if !found {
			break
		}
		if cut := strings.Index(tail, blockEnd); cut >= 0 {
			tail = pyLstrip(tail[cut+len(blockEnd):])
		} else {
			tail = ""
		}
	}
	if pyStrip(tail) == "" {
		return bare()
	}
	joined := strings.Join(pySplitWhitespace(tail), " ")
	return joined, joined != ""
}

// Skill expanded turn rendered as typed, slash name and instruction joined by separator, false when not scaffolding
func DescribeSkillInvocation(content, separator string) (string, bool) {
	if strings.HasPrefix(content, autoLoadPrefix) {
		return describeAutoLoadedSkillTurn(content)
	}
	if !strings.HasPrefix(content, skillInvocationPrefix) {
		return "", false
	}
	name := ""
	if m := skillNameRe.FindStringSubmatch(content); m != nil {
		name = pyStrip(m[1])
	}
	label := name
	if !strings.HasPrefix(name, "/") {
		label = "/" + name
	}
	if instruction, ok := ExtractUserInstructionFromSkillMessage(content); ok && instruction != "" {
		head, _, _ := strings.Cut(instruction, SkillExcerptJoint)
		if instruction = strings.Join(pySplitWhitespace(head), " "); instruction != "" {
			if name != "" {
				return label + separator + instruction, true
			}
			return instruction, true
		}
	}
	if name != "" {
		return label, true
	}
	return "", false
}
