package prompt

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	surfaceSwitchNotePrefix = "[System: This conversation is now being answered on a different interface: "
	surfaceNameEnd          = " — any earlier interface guidance"
	surfaceNoteScanTail     = 200
	moaProvider             = "moa"

	skillsNotePrefix    = "[System: The skills available in this conversation changed after its system prompt was built."
	skillsAddedHead     = "Now available (load with skill_view(name) when relevant, like the skills listed in the system prompt):"
	skillsRemovedHead   = "No longer available:"
	skillsAccurateAgain = " The skills listed in the system prompt are accurate again; earlier skill-change notes are superseded."
	skillsRow           = "    - "

	SectionAfterMemory     = "after_memory"
	MaxSectionChars        = 4_000
	pluginSectionsStart    = "<!-- icebath-plugin-sections:start -->"
	pluginSectionsEnd      = "<!-- icebath-plugin-sections:end -->"
	pluginSectionHeading   = "## Plugin Context: "
	persistedSectionPlugin = "persisted-prompt"
)

var (
	skillsBlock       = regexp.MustCompile(`(?s)<available_skills>\n?(.*?)</available_skills>`)
	skillsNamesOnly   = regexp.MustCompile(`\[names only\]:\s*(.+)$`)
	pluginSectionMark = regexp.MustCompile(`(?m)^## Plugin Context: ([a-z0-9][a-z0-9._-]{0,127})\n<!-- icebath-plugin-section-chars:([0-9]{1,4}) -->\n\n`)
)

type Section struct {
	ID       string
	Content  string
	Position string
	Plugin   string
}

func formatSections(sections []Section) string {
	if len(sections) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(sections))
	for _, s := range sections {
		blocks = append(blocks, pluginSectionHeading+s.ID+"\n"+
			"<!-- icebath-plugin-section-chars:"+strconv.Itoa(utf8.RuneCountInString(s.Content))+" -->\n\n"+s.Content)
	}
	return pluginSectionsStart + "\n" + strings.Join(blocks, "\n\n") + "\n" + pluginSectionsEnd
}

func sectionBlock(sections []Section, position string) string {
	var at []Section
	for _, s := range sections {
		if s.Position == position {
			at = append(at, s)
		}
	}
	return formatSections(at)
}

func RestoreSections(prompt string) []Section {
	start := strings.LastIndex(prompt, pluginSectionsStart)
	if start < 0 {
		return nil
	}
	rel := strings.Index(prompt[start+len(pluginSectionsStart):], pluginSectionsEnd)
	if rel < 0 {
		return nil
	}
	afterEnd := start + len(pluginSectionsStart) + rel + len(pluginSectionsEnd)
	if !strings.HasPrefix(prompt[afterEnd:], "\n\nConversation started:") {
		return nil
	}
	framed := prompt[start:afterEnd]
	var restored []Section
	for _, m := range pluginSectionMark.FindAllStringSubmatchIndex(framed, -1) {
		want, err := strconv.Atoi(framed[m[4]:m[5]])
		if err != nil {
			continue
		}
		rest := []rune(framed[m[1]:])
		if want > MaxSectionChars || len(rest) < want {
			continue
		}
		restored = append(restored, Section{ID: framed[m[2]:m[3]], Content: string(rest[:want]),
			Position: SectionAfterMemory, Plugin: persistedSectionPlugin})
	}
	if formatSections(restored) != framed {
		return nil
	}
	return restored
}

func SplitRuntimeBoundary(prompt string) (identity, runtime string, ok bool) {
	if !strings.HasSuffix(prompt, runtimeEnvironmentEnd) {
		return prompt, "", false
	}
	sep := "\n\n" + runtimeEnvironmentHeading + "\n\n"
	i := strings.LastIndex(prompt, sep)
	if i < 0 {
		return "", prompt, false
	}
	return prompt[:i], prompt[i+len(sep):], true
}

func RuntimeHostValue(prompt, label string) string {
	_, runtime, ok := SplitRuntimeBoundary(prompt)
	prefix := label + ":"
	body := prompt
	if ok {
		body, _, _ = strings.Cut(runtime, "\n\n")
	}
	lines := splitLines(body)
	for i, line := range lines {
		if !strings.HasPrefix(line, "User home directory:") {
			continue
		}
		for _, candidate := range lines[min(i+1, len(lines)):min(i+4, len(lines))] {
			if strings.HasPrefix(candidate, prefix) {
				return trim(candidate[len(prefix):])
			}
		}
	}
	return ""
}

func IdentityLineValue(prompt, label string) string {
	prefix := label + ":"
	identity, _, _ := SplitRuntimeBoundary(prompt)
	trailer := identity
	if i := strings.LastIndex(identity, "\n\n"); i >= 0 {
		trailer = identity[i+2:]
	}
	value := ""
	for _, line := range splitLines(trailer) {
		if strings.HasPrefix(line, prefix) {
			value = trim(line[len(prefix):])
		}
	}
	return value
}

func RewriteModelIdentity(prompt, model, provider string) string {
	if prompt == "" {
		return prompt
	}
	for _, f := range [][2]string{{"Model", model}, {"Provider", provider}} {
		label, value := f[0], f[1]
		if value == "" {
			continue
		}
		matches := regexp.MustCompile(`(?m)^`+label+`: .*$`).FindAllStringIndex(prompt, -1)
		if len(matches) == 0 {
			continue
		}
		last := matches[len(matches)-1]
		prompt = prompt[:last[0]] + label + ": " + value + prompt[last[1]:]
	}
	return prompt
}

func notesSkipped(in Inputs) bool {
	return in.Provider == moaProvider || in.Mode == wire.ModeCodexAppServer
}

func lastAnnouncedSurface(history []wire.Message) string {
	tail := history
	if len(tail) > surfaceNoteScanTail {
		tail = tail[len(tail)-surfaceNoteScanTail:]
	}
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i].Role != wire.RoleUser {
			continue
		}
		text := tail[i].FlatText("\n")
		at := strings.LastIndex(text, surfaceSwitchNotePrefix)
		if at < 0 {
			continue
		}
		name, _, _ := strings.Cut(text[at+len(surfaceSwitchNotePrefix):], surfaceNameEnd)
		return trim(name)
	}
	return ""
}

func (b *Builder) SurfaceSwitch(ctx context.Context, in Inputs, prompt string, history []wire.Message) (string, bool, error) {
	if notesSkipped(in) {
		return "", false, nil
	}
	current := trim(in.Platform)
	if current == "" {
		return "", false, nil
	}
	described := IdentityLineValue(prompt, "Platform")
	told := lastAnnouncedSurface(history)
	if told == "" {
		told = described
	}
	if told == "" || told == current {
		return "", false, nil
	}
	hint, where := "", "the interface section in the system prompt above"
	if described != current {
		var err error
		hint, err = b.PlatformHint(ctx, in)
		if err != nil {
			return "", false, err
		}
	}
	if hint != "" {
		where = "the guidance below"
	}
	note := surfaceSwitchNotePrefix + current + surfaceNameEnd + " in this conversation is " +
		"superseded — follow " + where + " for formatting, file delivery and any interface-specific " +
		"capability.]"
	if hint != "" {
		note += "\n" + hint
	}
	slog.Info(fmt.Sprintf("Session %s switched surface %s -> %s; keeping the stored system prompt and delivering "+
		"the new surface guidance as a turn note (prefix cache preserved).", in.SessionID, told, current))
	return note, true, nil
}

func InertToolsNote(note string, pinned, built []string) string {
	var inert []string
	for _, name := range pinned {
		if !slices.Contains(built, name) {
			inert = append(inert, name)
		}
	}
	if len(inert) == 0 || note == "" {
		return note
	}
	return note + "\n[System: These tools stay listed for this conversation (dropping them would discard " +
		"the cached request prefix) but were not loaded for this interface — expect a call to " +
		"one of them to fail: " + strings.Join(inert, ", ") + ".]"
}

func dropRunes(s string, n int) string {
	for ; n > 0 && s != ""; n-- {
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
	}
	return s
}

func skillRowName(line string) string {
	name, _, _ := strings.Cut(dropRunes(trim(line), 2), ": ")
	return trim(name)
}

func indexEntries(prompt string) map[string]string {
	entries := map[string]string{}
	m := skillsBlock.FindStringSubmatch(prompt)
	if m == nil {
		return entries
	}
	for _, line := range splitLines(m[1]) {
		if name := skillRowName(line); strings.HasPrefix(trim(line), "- ") && name != "" {
			if _, ok := entries[name]; !ok {
				entries[name] = strings.TrimRightFunc(line, isSpace)
			}
			continue
		}
		only := skillsNamesOnly.FindStringSubmatch(line)
		if only == nil {
			continue
		}
		for _, name := range strings.Split(only[1], ",") {
			if name = trim(name); name != "" {
				if _, ok := entries[name]; !ok {
					entries[name] = ""
				}
			}
		}
	}
	return entries
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func renderSkillsNote(added map[string]string, removed map[string]bool) string {
	if len(added) == 0 && len(removed) == 0 {
		return skillsNotePrefix + skillsAccurateAgain + "]"
	}
	parts := []string{skillsNotePrefix}
	if len(added) > 0 {
		parts = append(parts, skillsAddedHead)
		for _, name := range sortedKeys(added) {
			row := strings.TrimLeftFunc(added[name], isSpace)
			if row == "" {
				row = "- " + name
			}
			parts = append(parts, skillsRow+dropRunes(row, 2))
		}
	}
	if len(removed) > 0 {
		parts = append(parts, skillsRemovedHead+" "+strings.Join(sortedKeys(removed), ", "))
	}
	return strings.Join(parts, "\n") + "]"
}

func parseSkillsNote(note string) (added, removed map[string]bool) {
	added, removed = map[string]bool{}, map[string]bool{}
	lines := strings.Split(note, "\n")
	if len(lines) > 1 && lines[1] == skillsAddedHead {
		for _, line := range lines[2:] {
			if !strings.HasPrefix(line, skillsRow) {
				break
			}
			added[skillRowName(strings.TrimSuffix(line, "]"))] = true
		}
	}
	for _, line := range lines[min(1, len(lines)):] {
		if !strings.HasPrefix(line, skillsRemovedHead) {
			continue
		}
		tail := strings.TrimSuffix(line[len(skillsRemovedHead):], "]")
		for _, n := range strings.Split(tail, ",") {
			if n = trim(n); n != "" {
				removed[n] = true
			}
		}
		break
	}
	return added, removed
}

func announcedSkills(history []wire.Message) (added, removed map[string]bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != wire.RoleUser {
			continue
		}
		text := history[i].FlatText("\n")
		if at := strings.LastIndex(text, skillsNotePrefix); at >= 0 {
			return parseSkillsNote(text[at+len(skillsNotePrefix):])
		}
	}
	return map[string]bool{}, map[string]bool{}
}

type SkillsTold struct {
	prompt  string
	known   bool
	added   map[string]bool
	removed map[string]bool
}

func SkillsDelta(in Inputs, prompt string, history []wire.Message, told *SkillsTold) (string, bool) {
	if notesSkipped(in) {
		return "", false
	}
	stored := indexEntries(prompt)
	if len(stored) == 0 {
		return "", false
	}
	currentIndex := ""
	if skillTools(in.ToolNames) {
		currentIndex = in.Skills.Text
	}
	current := indexEntries(currentIndex)
	var toldAdded, toldRemoved map[string]bool
	if told != nil && told.known && told.prompt == prompt {
		toldAdded, toldRemoved = told.added, told.removed
	} else {
		toldAdded, toldRemoved = announcedSkills(history)
	}
	believed := map[string]bool{}
	for name := range stored {
		believed[name] = true
	}
	for name := range toldAdded {
		believed[name] = true
	}
	for name := range toldRemoved {
		delete(believed, name)
	}
	same := len(believed) == len(current)
	if same {
		for name := range current {
			if !believed[name] {
				same = false
				break
			}
		}
	}
	if same {
		if told != nil {
			*told = SkillsTold{prompt: prompt, known: true, added: toldAdded, removed: toldRemoved}
		}
		return "", false
	}
	added := map[string]string{}
	addedNames := map[string]bool{}
	for name, row := range current {
		if _, ok := stored[name]; !ok {
			added[name] = row
			addedNames[name] = true
		}
	}
	removed := map[string]bool{}
	for name := range stored {
		if _, ok := current[name]; !ok {
			removed[name] = true
		}
	}
	if told != nil {
		*told = SkillsTold{prompt: prompt, known: true, added: addedNames, removed: removed}
	}
	slog.Info(fmt.Sprintf("Session %s: skills changed (+%d/-%d) since the system prompt was built; delivering the "+
		"change as a turn note (prefix cache preserved).", in.SessionID, len(added), len(removed)))
	return renderSkillsNote(added, removed), true
}
