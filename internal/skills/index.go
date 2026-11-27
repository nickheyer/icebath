package skills

import (
	"cmp"
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Categories outside coding, which a session in a code project with the coding focus posture lists by name only
var NonCoding = []string{"apple", "communication", "cooking", "creative", "email", "finance", "gaming", "gifs", "health", "media", "music", "note-taking", "productivity", "shopping", "smart-home", "social-media", "travel", "yuanbao"}

// Tools of skills, whose presence offers the index
var Tools = []string{"skills_list", "skill_view", "skill_manage"}

// What the index is built for besides the scope: categories listed by name only, and whether the session answers one query and ends
type IndexOptions struct {
	Demoted map[string]bool
	OneShot bool
}

const oneShotGuidance = "## Skills\n" +
	"Scan the skills below and load one with skill_view(name) only when it carries domain knowledge you lack " +
	"for THIS task (an API, a tool's commands, a project's conventions). Do not load general process skills " +
	"(testing, debugging, review methodology) for work you already know how to do.\n"

// One row of the index
type row struct{ name, description string }

// Category of a skill in the index: the folders above its own within its root, general for one at the root
func indexCategory(rel string) string {
	parts := strings.Split(rel, "/")
	switch {
	case len(parts) > 2:
		return strings.Join(parts[:len(parts)-2], "/")
	case len(parts) == 2:
		return parts[0]
	}
	return "general"
}

// Entries of the index: every skill of every root resolved as a lookup resolves it, visible when it can be offered here, its
// conditions let it show and it is not disabled by its folder's name
func (s *Scope) indexEntries(disabled map[string]bool) []Entry {
	var scanned []Entry
	for _, r := range s.roots(true) {
		if !fsx.Exists(r.dir) {
			continue
		}
		files := skillFiles(r.dir)
		if r.tier == TierProject {
			files = s.unquarantined(files)
		}
		for _, skillMD := range files {
			rel, err := filepath.Rel(r.dir, skillMD)
			if err != nil || !filepath.IsLocal(rel) {
				continue
			}
			rel = filepath.ToSlash(rel)
			fm := Frontmatter{}
			description := ""
			compatible := true
			if content, err := readStrict(skillMD); err == nil {
				fm, _ = Parse(content)
				description = clip(fm.Description())
				compatible = s.compatible(fm)
			}
			folder := filepath.Base(filepath.Dir(skillMD))
			name := folder
			if v, ok := fm["name"]; ok && config.Truthy(v) {
				name = text(v)
			}
			scanned = append(scanned, Entry{
				Name:        name,
				Description: description,
				Category:    indexCategory(rel),
				Tier:        r.tier,
				Root:        r.dir,
				Path:        skillMD,
				Rel:         filepath.ToSlash(filepath.Dir(rel)),
				Visible:     compatible && !disabled[folder] && s.shows(fm.conditions()),
				Frontmatter: fm,
			})
		}
	}
	return s.resolveEntries(scanned)
}

// Namespaced skills the index lists: enabled, offerable here, their conditions letting them show
func (s *Scope) pluginRows(disabled map[string]bool) []row {
	var out []row
	for _, p := range s.Plugins() {
		if disabled[p.Qualified()] || !s.compatible(p.Frontmatter) || !s.shows(p.Frontmatter.conditions()) {
			continue
		}
		description := clip(strings.TrimSpace(p.Description))
		out = append(out, row{p.Qualified(), description})
	}
	return out
}

// The skills index of the system prompt, none when no skill can be listed
func (s *Scope) Index(ctx context.Context, opts IndexOptions) string {
	disabled := s.Disabled()
	byCategory := map[string][]row{}
	listed := map[string]bool{}
	var unloadable []string
	for _, e := range s.indexEntries(disabled) {
		if !e.Visible || e.Status == Shadowed || disabled[e.Name] || e.Load != "" && disabled[e.Load] {
			continue
		}
		if e.Load == "" {
			if !slices.Contains(unloadable, e.Name) {
				unloadable = append(unloadable, e.Name)
			}
			continue
		}
		description := e.Description
		if e.Tier == TierProject {
			description = strings.TrimSpace("[project] " + description)
		}
		category := cmp.Or(e.Category, "general")
		byCategory[category] = append(byCategory[category], row{e.Load, description})
		listed[e.Load] = true
	}
	for _, r := range s.pluginRows(disabled) {
		if !listed[r.name] {
			listed[r.name] = true
			byCategory["plugin"] = append(byCategory["plugin"], r)
		}
	}
	slices.Sort(unloadable)
	return s.render(byCategory, opts, unloadable)
}

// The index text: each category's skills with their descriptions, a demoted category's by name only
func (s *Scope) render(byCategory map[string][]row, opts IndexOptions, unloadable []string) string {
	if len(byCategory) == 0 {
		return ""
	}
	demoted := map[string]bool{}
	for category := range byCategory {
		top, _, _ := strings.Cut(category, "/")
		if opts.Demoted[top] {
			demoted[category] = true
		}
	}
	note := ""
	if len(demoted) > 0 {
		note = "\n(Categories marked [names only] are outside the current coding context, so their descriptions are omitted — the skills work normally and load with skill_view(name) as usual.)"
	}
	if len(unloadable) > 0 {
		note += "\n(A copy of " + strings.Join(unloadable, ", ") + " is not listed: it shares both its name and its path with a different skill in the same skills directory tier (e.g. another external_dirs entry), so skill_view cannot load it — rename one.)"
	}
	basic := "web_search or terminal"
	if s.Tools != nil && !s.Tools["web_search"] {
		basic = "terminal"
	}
	var lines []string
	for _, category := range sorted(setOf(byCategory)) {
		rows := byCategory[category]
		if demoted[category] {
			names := map[string]bool{}
			for _, r := range rows {
				names[r.name] = true
			}
			lines = append(lines, "  "+category+" [names only]: "+strings.Join(sorted(names), ", "))
			continue
		}
		slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(a.name, b.name) })
		rows = slices.CompactFunc(rows, func(a, b row) bool { return a.name == b.name })
		indent := "    "
		if len(rows) == 1 {
			indent = "  "
		} else {
			lines = append(lines, "  "+category+":")
		}
		for _, r := range rows {
			if r.description != "" {
				lines = append(lines, indent+"- "+r.name+": "+r.description)
			} else {
				lines = append(lines, indent+"- "+r.name)
			}
		}
	}
	block := "<available_skills>\n" + strings.Join(lines, "\n") + "\n</available_skills>"
	if opts.OneShot {
		return oneShotGuidance + "\n" + block + note
	}
	return "## Skills\n" +
		"Before replying, scan the skills below. If a skill matches or is even partially relevant to your " +
		"task, you MUST load it with skill_view(name) and follow its instructions. Err on the side of " +
		"loading — it is always better to have context you don't need than to miss critical steps, pitfalls, " +
		"or established workflows. Skills contain specialized knowledge — API endpoints, tool-specific " +
		"commands, and proven workflows that outperform general-purpose approaches. Load the skill " +
		"even if you think you could handle the task with basic tools like " + basic + ". " +
		"Skills also encode the user's preferred approach, conventions, and quality standards for tasks like " +
		"code review, planning, and testing — load them even for tasks you already know how to do, because " +
		"the skill defines how it should be done here.\n" +
		"If a skill has issues, fix it with skill_manage(action='patch').\n" +
		"After difficult/iterative tasks, offer to save as a skill. If a skill you loaded was missing steps, " +
		"had wrong commands, or needed pitfalls you discovered, update it before finishing.\n\n" +
		block + "\n\nOnly proceed without loading a skill if genuinely none are relevant to the task." + note
}

// Keys of a map as a set
func setOf[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for key := range m {
		out[key] = true
	}
	return out
}

const (
	notePrefix  = "[System: The skills available in this conversation changed after its system prompt was built."
	addedHead   = "Now available (load with skill_view(name) when relevant, like the skills listed in the system prompt):"
	removedHead = "No longer available:"
	accurate    = " The skills listed in the system prompt are accurate again; earlier skill-change notes are superseded."
	addedIndent = "    - "
)

var (
	indexBlock = regexp.MustCompile(`(?s)<available_skills>\n?(.*?)</available_skills>`)
	namesOnly  = regexp.MustCompile(`\[names only\]:[` + spaceClass + `]*(.+)$`)
)

// Name of an index row, the part before ": " so a qualified name stays whole
func rowName(line string) string {
	stripped := strings.TrimSpace(line)
	if len(stripped) < 2 {
		return ""
	}
	name, _, _ := strings.Cut(stripped[2:], ": ")
	return strings.TrimSpace(name)
}

// Row of every skill the index in a prompt lists, by name; one listed by name only maps to nothing
func listing(prompt string) map[string]string {
	out := map[string]string{}
	m := indexBlock.FindStringSubmatch(prompt)
	if m == nil {
		return out
	}
	for _, line := range strings.Split(m[1], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			if name := rowName(line); name != "" {
				if _, seen := out[name]; !seen {
					out[name] = strings.TrimRightFunc(line, unicode.IsSpace)
				}
				continue
			}
		}
		if names := namesOnly.FindStringSubmatch(line); names != nil {
			for _, name := range strings.Split(names[1], ",") {
				if name = strings.TrimSpace(name); name != "" {
					if _, seen := out[name]; !seen {
						out[name] = ""
					}
				}
			}
		}
	}
	return out
}

// Names a change note added and removed
type delta struct{ added, removed map[string]bool }

// Note telling the model how the skills differ from those its prompt lists
func changeNote(added map[string]string, removed map[string]bool) string {
	if len(added) == 0 && len(removed) == 0 {
		return notePrefix + accurate + "]"
	}
	parts := []string{notePrefix}
	if len(added) > 0 {
		parts = append(parts, addedHead)
		for _, name := range sorted(setOf(added)) {
			line := strings.TrimLeftFunc(added[name], unicode.IsSpace)
			if line == "" {
				line = "- " + name
			}
			parts = append(parts, addedIndent+line[2:])
		}
	}
	if len(removed) > 0 {
		parts = append(parts, removedHead+" "+strings.Join(sorted(removed), ", "))
	}
	return strings.Join(parts, "\n") + "]"
}

// Names a note added and removed, read back from its text after the prefix
func readNote(note string) delta {
	lines := strings.Split(note, "\n")
	d := delta{added: map[string]bool{}, removed: map[string]bool{}}
	if len(lines) > 1 && lines[1] == addedHead {
		for _, line := range lines[2:] {
			if !strings.HasPrefix(line, addedIndent) {
				break
			}
			d.added[rowName(strings.TrimSuffix(line, "]"))] = true
		}
	}
	for _, line := range lines[1:] {
		if tail, ok := strings.CutPrefix(line, removedHead); ok {
			for _, name := range strings.Split(strings.TrimSuffix(tail, "]"), ",") {
				if name = strings.TrimSpace(name); name != "" {
					d.removed[name] = true
				}
			}
			break
		}
	}
	return d
}

// Text of a message without the note of skill changes a turn added to it
func Unnoted(text string) string {
	if at := strings.LastIndex(text, "\n\n"+notePrefix); at >= 0 {
		return text[:at]
	}
	return text
}

// A note for the next turn when the skills the scope can load differ from those the session's prompt lists by something the model
// was not told yet, in the newest note of its user messages; none when there is nothing new to tell. The prompt keeps the index it was
// built with, so its cache holds
func (s *Scope) Changes(ctx context.Context, prompt string, opts IndexOptions, history []string) string {
	stored := listing(prompt)
	if len(stored) == 0 {
		return ""
	}
	now := listing(s.Index(ctx, opts))
	told := delta{added: map[string]bool{}, removed: map[string]bool{}}
	for _, text := range slices.Backward(history) {
		if at := strings.LastIndex(text, notePrefix); at >= 0 {
			told = readNote(text[at+len(notePrefix):])
			break
		}
	}
	believed := setOf(stored)
	for name := range told.added {
		believed[name] = true
	}
	for name := range told.removed {
		delete(believed, name)
	}
	if len(believed) == len(now) && !slices.ContainsFunc(sorted(setOf(now)), func(name string) bool { return !believed[name] }) {
		return ""
	}
	added, removed := map[string]string{}, map[string]bool{}
	for name, line := range now {
		if _, ok := stored[name]; !ok {
			added[name] = line
		}
	}
	for name := range stored {
		if _, ok := now[name]; !ok {
			removed[name] = true
		}
	}
	return changeNote(added, removed)
}
