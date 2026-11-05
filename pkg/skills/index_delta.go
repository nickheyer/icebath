package skills

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	indexNotePrefix    = "[System: The skills available in this conversation changed after its system prompt was built."
	indexAddedHead     = "Now available (load with skill_view(name) when relevant, like the skills listed in the system prompt):"
	indexRemovedHead   = "No longer available:"
	indexAccurateAgain = " The skills listed in the system prompt are accurate again; earlier skill-change notes are superseded."
	indexRow4          = "    - "
)

var (
	indexBlockRe     = regexp.MustCompile(`(?s)<available_skills>\n?(.*?)</available_skills>`)
	indexNamesOnlyRe = regexp.MustCompile(`\[names only\]:[` + pySpaceClass + `]*(.+)$`)
)

// Names a note added and removed
type indexDelta struct{ added, removed map[string]bool }

// Per agent record of the skills index difference already delivered and the note staged for the next turn
type SkillsIndexNotes struct {
	toldKey   string
	toldDelta *indexDelta
	note      string
}

// Skill name of an index row, splitting on a colon and space so qualified plugin names stay whole
func indexRowName(line string) string {
	stripped := pystr.Strip(line)
	if len(stripped) >= 2 {
		stripped = stripped[2:]
	} else {
		stripped = ""
	}
	return pystr.Strip(strings.SplitN(stripped, ": ", 2)[0])
}

// Index row text by skill name for every skill in the prompt available_skills block, names only rows mapping to empty
func IndexEntries(prompt any) map[string]string {
	entries := map[string]string{}
	text, ok := prompt.(string)
	if !ok {
		return entries
	}
	m := indexBlockRe.FindStringSubmatch(text)
	if m == nil {
		return entries
	}
	for _, line := range pystr.Splitlines(m[1], false) {
		if strings.HasPrefix(pystr.Strip(line), "- ") {
			if name := indexRowName(line); name != "" {
				if _, seen := entries[name]; !seen {
					entries[name] = pystr.Rstrip(line)
				}
				continue
			}
		}
		if names := indexNamesOnlyRe.FindStringSubmatch(line); names != nil {
			for _, n := range strings.Split(names[1], ",") {
				if n = pystr.Strip(n); n != "" {
					if _, seen := entries[n]; !seen {
						entries[n] = ""
					}
				}
			}
		}
	}
	return entries
}

// Sorted keys of a string set
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Note text stating the full difference against the prompt index
func renderIndexNote(added map[string]string, removed map[string]bool) string {
	if len(added) == 0 && len(removed) == 0 {
		return indexNotePrefix + indexAccurateAgain + "]"
	}
	parts := []string{indexNotePrefix}
	if len(added) > 0 {
		parts = append(parts, indexAddedHead)
		for _, name := range sortedNames(added) {
			row := pystr.Lstrip(added[name])
			if row == "" {
				row = "- " + name
			}
			parts = append(parts, indexRow4+row[2:])
		}
	}
	if len(removed) > 0 {
		parts = append(parts, indexRemovedHead+" "+strings.Join(sortedNames(removed), ", "))
	}
	return strings.Join(parts, "\n") + "]"
}

// Added and removed names read back from the text after the note prefix
func parseIndexNote(note string) indexDelta {
	lines := strings.Split(note, "\n")
	d := indexDelta{added: map[string]bool{}, removed: map[string]bool{}}
	if len(lines) > 1 && lines[1] == indexAddedHead {
		for _, line := range lines[2:] {
			if !strings.HasPrefix(line, indexRow4) {
				break
			}
			d.added[indexRowName(strings.TrimSuffix(line, "]"))] = true
		}
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, indexRemovedHead) {
			tail := strings.TrimSuffix(line[len(indexRemovedHead):], "]")
			for _, n := range strings.Split(tail, ",") {
				if n = pystr.Strip(n); n != "" {
					d.removed[n] = true
				}
			}
			break
		}
	}
	return d
}

// Difference named by the newest skills note anywhere in the transcript
func announcedIndexDelta(history []map[string]any) indexDelta {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg == nil || msg["role"] != "user" {
			continue
		}
		sidecar, _ := msg["api_content"].(string)
		text := sidecar + "\n" + state.FlattenMessageText(msg["content"], "\n")
		if j := strings.LastIndex(text, indexNotePrefix); j >= 0 {
			return parseIndexNote(text[j+len(indexNotePrefix):])
		}
	}
	return indexDelta{added: map[string]bool{}, removed: map[string]bool{}}
}

// Pops the staged skills index note, empty when none is pending
func (n *SkillsIndexNotes) Consume() string {
	note := n.note
	n.note = ""
	return note
}

// Stages a one shot note when the loadable skills differ from the prompt index by something the model was not told
func (n *SkillsIndexNotes) Stage(provider, apiMode, sessionID string, current func() (string, error), prompt any, history []map[string]any) bool {
	if provider == "moa" || apiMode == "codex_app_server" {
		return false
	}
	stored := IndexEntries(prompt)
	if len(stored) == 0 {
		return false
	}
	currentPrompt, err := current()
	if err != nil {
		slog.Debug("skills index delta: current index unavailable", "error", err)
		return false
	}
	now := IndexEntries(currentPrompt)
	key := prompt.(string)
	told := announcedIndexDelta(history)
	if n.toldDelta != nil && n.toldKey == key {
		told = *n.toldDelta
	}
	believed := map[string]bool{}
	for name := range stored {
		believed[name] = true
	}
	for name := range told.added {
		believed[name] = true
	}
	for name := range told.removed {
		delete(believed, name)
	}
	same := len(believed) == len(now)
	for name := range now {
		same = same && believed[name]
	}
	if same {
		n.toldKey, n.toldDelta = key, &told
		return false
	}
	added := map[string]string{}
	delta := indexDelta{added: map[string]bool{}, removed: map[string]bool{}}
	for name, row := range now {
		if _, ok := stored[name]; !ok {
			added[name] = row
			delta.added[name] = true
		}
	}
	for name := range stored {
		if _, ok := now[name]; !ok {
			delta.removed[name] = true
		}
	}
	n.toldKey, n.toldDelta = key, &delta
	n.note = renderIndexNote(added, delta.removed)
	slog.Default().With("logger", "run_agent").Info(fmt.Sprintf(
		"Session %s: skills changed (+%d/-%d) since the system prompt was built; delivering the "+
			"change as a turn note (prefix cache preserved).", sessionID, len(added), len(delta.removed)))
	return true
}
