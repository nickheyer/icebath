package sessions

import (
	"regexp"
	"strings"
	"unicode"
)

// Text icebath puts into messages of its own: the summary compaction leaves, and a skill expanded into the turn that invoked it
const (
	SummaryPrefix       = "[CONTEXT COMPACTION"
	LegacySummaryPrefix = "[CONTEXT SUMMARY]:"
	SkillPrefix         = "[IMPORTANT: The user has invoked the "
	AutoSkillPrefix     = `[IMPORTANT: The "`
	singleSkillMarker   = "The full skill content is loaded below.]"
	skillInstruction    = "The user has provided the following instruction alongside the skill invocation: "
	runtimeNote         = "\n\n[Runtime note:"
	bundleMarker        = " skill bundle,"
	bundleInstruction   = "\nUser instruction: "
	bundleFirstSkill    = "\n\n[Loaded as part of the "
	skillDirNoteEnd     = "then run them with the terminal tool using the absolute path."
	supportingFilesEnd  = "/scripts/foo.js`)."
	// Joins the instruction of a skill turn to an excerpt after it
	skillExcerptJoint = "\x1e"
)

var (
	skillName         = regexp.MustCompile(`^` + regexp.QuoteMeta(SkillPrefix) + `"([^"]*)"`)
	autoSkillName     = regexp.MustCompile(`^` + regexp.QuoteMeta(AutoSkillPrefix) + `([^"]*)"`)
	trailingBlockEnds = [][2]string{{"[Skill config (", "\n]"}, {"[Skill setup note: ", "]\n\n"}, {"[This skill has supporting files", supportingFilesEnd}}
)

// Whether the text is the summary compaction left in place of earlier messages
func IsSummary(text string) bool {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	return strings.HasPrefix(trimmed, SummaryPrefix) || strings.HasPrefix(trimmed, LegacySummaryPrefix)
}

// Text of a skill turn after the marker, up to the stop marker
func cutAfter(message, marker, stop string, last bool) (string, bool) {
	at := strings.Index(message, marker)
	if last {
		at = strings.LastIndex(message, marker)
	}
	if at < 0 {
		return "", false
	}
	span, _, _ := strings.Cut(message[at+len(marker):], stop)
	span = strings.TrimSpace(span)
	return span, span != ""
}

// What the user typed alongside the skill a turn invoked, the whole text of a turn that invoked none
func SkillInstruction(content string) (string, bool) {
	switch {
	case !strings.HasPrefix(content, SkillPrefix):
		return content, true
	case strings.Contains(content, bundleMarker):
		return cutAfter(content, bundleInstruction, bundleFirstSkill, false)
	case strings.Contains(content, singleSkillMarker):
		return cutAfter(content, skillInstruction, runtimeNote, true)
	}
	return "", false
}

// A turn a skill was expanded into as the user typed it: the skill's command, then what they asked, joined by the separator. False for
// a turn that invoked no skill
func DescribeSkill(content, separator string) (string, bool) {
	if strings.HasPrefix(content, AutoSkillPrefix) {
		return describeAutoSkill(content)
	}
	if !strings.HasPrefix(content, SkillPrefix) {
		return "", false
	}
	name := ""
	if match := skillName.FindStringSubmatch(content); match != nil {
		name = strings.TrimSpace(match[1])
	}
	label := name
	if !strings.HasPrefix(name, "/") {
		label = "/" + name
	}
	if instruction, ok := SkillInstruction(content); ok && instruction != "" {
		head, _, _ := strings.Cut(instruction, skillExcerptJoint)
		if instruction = strings.Join(strings.Fields(head), " "); instruction != "" {
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

// A turn a skill was loaded into by itself: what the user wrote after the skill's notes, else the skill's command
func describeAutoSkill(content string) (string, bool) {
	name := ""
	if match := autoSkillName.FindStringSubmatch(content); match != nil {
		name = match[1]
	}
	bare := func() (string, bool) {
		if name != "" {
			return "/" + name, true
		}
		return "", false
	}
	end := strings.LastIndex(content, skillDirNoteEnd)
	if end < 0 {
		return bare()
	}
	tail := strings.TrimLeftFunc(content[end+len(skillDirNoteEnd):], unicode.IsSpace)
	for {
		blockEnd := ""
		for _, pair := range trailingBlockEnds {
			if strings.HasPrefix(tail, pair[0]) {
				blockEnd = pair[1]
				break
			}
		}
		if blockEnd == "" {
			break
		}
		if cut := strings.Index(tail, blockEnd); cut >= 0 {
			tail = strings.TrimLeftFunc(tail[cut+len(blockEnd):], unicode.IsSpace)
		} else {
			tail = ""
		}
	}
	if joined := strings.Join(strings.Fields(tail), " "); joined != "" {
		return joined, true
	}
	return bare()
}
