package skills

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Markers of the messages skills are loaded by, which Describe reads back
const (
	invokedPrefix     = "[IMPORTANT: The user has invoked the "
	invokedMarker     = "The full skill content is loaded below.]"
	instructionPrefix = "The user has provided the following instruction alongside the skill invocation: "
	runtimePrefix     = "\n\n[Runtime note:"
	bundleMarker      = " skill bundle,"
	bundleInstruction = "\nUser instruction: "
	bundleFirstBlock  = "\n\n[Loaded as part of the "
	autoLoadPrefix    = `[IMPORTANT: The "`
	skillDirNote      = "Resolve any relative paths in this skill (e.g. `scripts/foo.js`, `templates/config.yaml`) against that directory, then run them with the terminal tool using the absolute path."
	skillDirNoteEnd   = "then run them with the terminal tool using the absolute path."
	supportingEnd     = "/scripts/foo.js`)."
	setupSkipped      = "Required environment setup was skipped. Continue loading the skill and explain any reduced functionality if it matters."
)

// Most skills one slash line stacks
const maxStacked = 5

var (
	slugInvalid   = regexp.MustCompile(`[^\p{L}\p{N}_-]`)
	slugHyphens   = regexp.MustCompile(`-{2,}`)
	invokedName   = regexp.MustCompile(`^` + regexp.QuoteMeta(invokedPrefix) + `"([^"]*)"`)
	autoLoadName  = regexp.MustCompile(`^` + regexp.QuoteMeta(autoLoadPrefix) + `([^"]*)"`)
	trailingBlock = [][2]string{{"[Skill config (", "\n]"}, {"[Skill setup note: ", "]\n\n"}, {"[This skill has supporting files", supportingEnd}}
)

// Slash command name of a skill: lower case, spaces and underscores as hyphens, other characters dropped
func Slug(name string) string {
	slug := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), " ", "-"), "_", "-")
	slug = slugInvalid.ReplaceAllString(slug, "")
	return strings.Trim(slugHyphens.ReplaceAllString(slug, "-"), "-")
}

// A skill or a bundle a slash command loads
type Command struct {
	// Slash command, without the slash
	Key  string
	Name string
	// What it does, for listings
	Description string
	// Name it loads by: a namespaced skill's qualified name or a skill's folder; the bundle's skills for a bundle
	Identifier string
	// Skills of a bundle, none for a skill
	Skills []string
}

// Whether a slash command of the name belongs to icebath itself, so a skill can't take it
type Taken func(name string) bool

// Slash commands of the skills of the scope: every loadable skill by its slug, then each namespaced one by its qualified name, a
// skill whose slug a command of icebath's own has left out
func (s *Scope) Commands(taken Taken) map[string]Command {
	commands := map[string]Command{}
	disabled := s.Disabled()
	seen := map[string]bool{}
	for _, e := range s.catalog(false, false) {
		if e.Status != Unique {
			continue
		}
		content, err := readStrict(e.Path)
		if err != nil {
			continue
		}
		fm, body := Parse(content)
		if !s.compatible(fm) {
			continue
		}
		name := filepath.Base(e.Dir())
		if v, ok := fm["name"]; ok {
			if name, ok = v.(string); !ok {
				continue
			}
		}
		if seen[name] || disabled[name] {
			continue
		}
		seen[name] = true
		description := fm["description"]
		if !config.Truthy(description) {
			description = strs.Head(firstLine(body), 80)
		}
		slug := Slug(name)
		if slug == "" {
			continue
		}
		if taken != nil && taken(slug) {
			if s.registry.first("collides\x00" + slug) {
				slog.Warn("skills: a skill's slash command collides with one of icebath's own, so it is not registered", "skill", name, "command", "/"+slug)
			}
			continue
		}
		if prior, ok := commands[slug]; ok {
			if s.registry.first("claimed\x00" + slug + "\x00" + name) {
				slog.Warn("skills: a skill's slash command is claimed by another skill; the first is kept", "skill", name, "command", "/"+slug, "kept", prior.Name)
			}
			continue
		}
		about := strings.TrimSpace(text(description))
		if about == "" {
			about = "Invoke the " + name + " skill"
		}
		commands[slug] = Command{Key: slug, Name: name, Description: about, Identifier: e.Dir()}
	}
	for _, p := range s.Plugins() {
		qualified := p.Qualified()
		key := strings.ToLower(qualified)
		if disabled[qualified] || disabled[p.Name] || !s.compatible(p.Frontmatter) {
			continue
		}
		if _, claimed := commands[key]; claimed || taken != nil && taken(key) {
			continue
		}
		about := cmp.Or(p.Description, "Invoke the "+qualified+" plugin skill")
		commands[key] = Command{Key: key, Name: qualified, Description: about, Identifier: qualified}
	}
	return commands
}

// Key of a typed command in the table, underscores tried as hyphens
func resolveKey[V any](command string, table map[string]V) (string, bool) {
	if command == "" {
		return "", false
	}
	key := strings.ToLower(command)
	if _, ok := table[key]; ok {
		return key, true
	}
	key = strings.ToLower(strings.ReplaceAll(command, "_", "-"))
	_, ok := table[key]
	return key, ok
}

// A skill loaded for a message: what its view returned, its folder and its name
type loaded struct {
	view Result
	dir  string
	name string
}

// The name a lookup takes for an identifier: an absolute path in a skill folder made relative to it
func (s *Scope) lookupName(identifier string) string {
	raw := strings.TrimSpace(identifier)
	p := fsx.ExpandHome(s.expandVars(raw))
	if !filepath.IsAbs(p) {
		return strings.TrimLeft(raw, "/")
	}
	for _, root := range append(append([]string{s.Dir()}, s.ProjectDirs()...), s.External()...) {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsLocal(rel) {
			return rel
		}
	}
	if rel, err := filepath.Rel(resolve(s.Dir()), resolve(p)); err == nil && filepath.IsLocal(rel) {
		return rel
	}
	return raw
}

// Loads a skill for a message, false when it can't be served
func (s *Scope) load(ctx context.Context, identifier string) (loaded, bool) {
	if strings.TrimSpace(identifier) == "" {
		return loaded{}, false
	}
	name := s.lookupName(identifier)
	view := s.View(ctx, name, "", false)
	if !view.OK() {
		return loaded{}, false
	}
	dir := view.Text("skill_dir")
	if dir == "" && view.Text("path") != "" {
		dir = filepath.Join(s.Dir(), filepath.Dir(filepath.FromSlash(view.Text("path"))))
	}
	return loaded{view: view, dir: dir, name: cmp.Or(view.Text("name"), name)}, true
}

// Name of a missing skill, or the refusal naming the exact names of skills sharing it
func (s *Scope) missingLabel(ctx context.Context, identifier string) string {
	view := s.View(ctx, s.lookupName(identifier), "", false)
	if names, ok := view["load_names"].([]string); ok && len(names) > 0 {
		return fmt.Sprintf("%s%s: use one of %s", AmbiguousPrefix, identifier, strings.Join(names, ", "))
	}
	return identifier
}

// Skills that could not be loaded as the user reads them: the ambiguous ones each, then the unknown ones in a line
func Missing(missing []string) string {
	var clauses, unknown []string
	for _, m := range missing {
		rest, ambiguous := strings.CutPrefix(m, AmbiguousPrefix)
		if !ambiguous {
			unknown = append(unknown, m)
			continue
		}
		if name, names, ok := strings.Cut(rest, ": use one of "); ok {
			clauses = append(clauses, "the skill name "+name+" could be "+strings.Join(strings.Split(names, ", "), " or "))
		} else {
			clauses = append(clauses, "the skill name "+rest+" matches more than one skill")
		}
	}
	switch len(unknown) {
	case 0:
	case 1:
		clauses = append(clauses, "there is no skill named "+unknown[0])
	default:
		clauses = append(clauses, "there are no skills named "+strings.Join(unknown[:len(unknown)-1], ", ")+" and "+unknown[len(unknown)-1])
	}
	return strings.Join(clauses, ", and ")
}

// Supporting files of a loaded skill, from its view or from its support folders
func supportingFiles(view Result, dir string) []string {
	var out []string
	if linked, ok := view["linked_files"].(map[string][]string); ok {
		keys := slices.Sorted(maps.Keys(linked))
		slices.SortStableFunc(keys, func(a, b string) int {
			return cmp.Compare(supportIndex(a), supportIndex(b))
		})
		for _, key := range keys {
			for _, f := range linked[key] {
				out = append(out, filepath.ToSlash(filepath.Clean(f)))
			}
		}
	}
	if len(out) > 0 || dir == "" {
		return out
	}
	for _, sub := range supportDirs {
		for _, rel := range files(filepath.Join(dir, sub)) {
			if p := filepath.Join(dir, sub, filepath.FromSlash(rel)); !redirect(p) {
				out = append(out, sub+"/"+rel)
			}
		}
	}
	return out
}

// Order of a support folder, unknown ones after
func supportIndex(name string) int {
	if i := slices.Index(supportDirs, name); i >= 0 {
		return i
	}
	return len(supportDirs)
}

// The message that loads one skill: the note, its content, its folder, its values, its setup, its files, the user's instruction
func (s *Scope) message(ctx context.Context, l loaded, note, instruction, runtime string) string {
	content := s.preprocess(ctx, l.view.Text("content"), l.dir)
	parts := []string{note, "", strings.TrimSpace(content)}
	if l.dir != "" {
		parts = append(parts, "", fmt.Sprintf("[Skill directory: %s]", l.dir), skillDirNote)
	}
	fm, _ := Parse(cmp.Or(l.view.Text("raw_content"), l.view.Text("content")))
	if vars := configVars(fm); len(vars) > 0 {
		resolved := s.resolved(vars)
		parts = append(parts, "", fmt.Sprintf("[Skill config (from %s/config.yaml):", display(s.Home())))
		for _, v := range vars {
			shown := "(not set)"
			if value := resolved[v.Key]; config.Truthy(value) {
				shown = fmt.Sprint(value)
			}
			parts = append(parts, fmt.Sprintf("  %s = %s", v.Key, shown))
		}
		parts = append(parts, "]")
	}
	if note := setupNote(l.view); note != "" {
		parts = append(parts, "", fmt.Sprintf("[Skill setup note: %s]", note))
	}
	if files := supportingFiles(l.view, l.dir); len(files) > 0 && l.dir != "" {
		target := cmp.Or(l.view.Text("name"), filepath.Base(l.dir))
		if rel, err := filepath.Rel(filepath.Clean(s.Dir()), filepath.Clean(l.dir)); err == nil && filepath.IsLocal(rel) {
			target = rel
		}
		parts = append(parts, "", "[This skill has supporting files (paths relative to the skill directory above):]")
		for _, f := range files {
			parts = append(parts, "- "+f)
		}
		parts = append(parts, fmt.Sprintf("\nLoad any of these with skill_view(name=\"%s\", file_path=\"<path>\"), or run scripts directly by absolute path (e.g. `node %s%s", target, l.dir, supportingEnd))
	}
	if instruction != "" {
		parts = append(parts, "", instructionPrefix+instruction)
	}
	if runtime != "" {
		parts = append(parts, "", fmt.Sprintf("[Runtime note: %s]", runtime))
	}
	return strings.Join(parts, "\n")
}

// What the message says of a skill's setup, none when nothing needs saying
func setupNote(view Result) string {
	if skipped, _ := view["setup_skipped"].(bool); skipped {
		return setupSkipped
	}
	if needed, _ := view["setup_needed"].(bool); needed {
		return view.Text("setup_note")
	}
	return ""
}

// Counts a use of the skill, then makes its message
func (s *Scope) block(ctx context.Context, l loaded, note, instruction, runtime string) string {
	s.Used(ctx, l.name)
	return s.message(ctx, l, note, instruction, runtime)
}

// Options of a load of several skills
type blockOptions struct {
	label    func(id string) string
	disabled map[string]bool
	// Whether a disabled skill counts as missing rather than disabled
	disabledMissing bool
	// Skills loaded already, counted as loaded without a block
	already map[string]bool
}

// Loads each identifier once: the names loaded, those missing, those disabled, and the messages
func (s *Scope) blocks(ctx context.Context, ids []string, load func(id string) (loaded, bool), note func(name string) string, opts blockOptions) ([]string, []string, []string, []string) {
	var names, missing, disabled, out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		l, ok := load(id)
		if !ok {
			label := id
			if opts.label != nil {
				label = opts.label(id)
			}
			missing = append(missing, label)
			continue
		}
		bare := l.name
		if _, after, ok := strings.Cut(l.name, ":"); ok {
			bare = after
		}
		if opts.disabled[l.name] || opts.disabled[id] || strings.Contains(l.name, ":") && opts.disabled[bare] {
			switch {
			case opts.disabledMissing:
				missing = append(missing, id)
			default:
				disabled = append(disabled, cmp.Or(l.name, id))
			}
			continue
		}
		names = append(names, l.name)
		if !opts.already[l.name] {
			out = append(out, s.block(ctx, l, note(l.name), "", ""))
		}
	}
	return names, missing, disabled, out
}

// Header of a message loading several skills together
func scaffold(subject string, names, lead, missing, disabled []string, extra, instruction string) string {
	out := []string{fmt.Sprintf("[IMPORTANT: The user has invoked the %s, loading %d skills together. Treat every skill below as active guidance for this turn.]", subject, len(names)), ""}
	out = append(out, lead...)
	out = append(out, "Skills loaded: "+strings.Join(names, ", "))
	if len(missing) > 0 {
		out = append(out, "Skills missing (skipped): "+strings.Join(missing, ", "))
	}
	if len(disabled) > 0 {
		out = append(out, "Skills disabled for this platform (skipped): "+strings.Join(disabled, ", "))
	}
	if extra != "" {
		out = append(out, "", "Bundle instruction: "+extra)
	}
	if instruction != "" {
		out = append(out, "", "User instruction: "+instruction)
	}
	return strings.Join(out, "\n")
}

// What a slash line that loads skills turns into: the message the model is sent, the skills it loaded and those it could not
type Invocation struct {
	Message string
	Loaded  []string
	Missing []string
}

// The message a slash line loading skills becomes: one skill's command with an instruction, up to five stacked, or a bundle; false
// when the line names none
func (s *Scope) Invoke(ctx context.Context, line string, taken Taken) (Invocation, bool, error) {
	command, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	name, ok := strings.CutPrefix(command, "/")
	if !ok || name == "" {
		return Invocation{}, false, nil
	}
	rest = strings.TrimSpace(rest)
	commands := s.Commands(taken)
	key, ok := resolveKey(name, commands)
	if !ok {
		return s.invokeBundle(ctx, name, rest)
	}
	keys := []string{key}
	remaining := rest
	for len(keys) < maxStacked {
		stripped := strings.TrimLeftFunc(remaining, unicode.IsSpace)
		next, tail, _ := strings.Cut(stripped, " ")
		word, isCommand := strings.CutPrefix(next, "/")
		if !isCommand {
			break
		}
		key, ok := resolveKey(word, commands)
		if !ok || slices.Contains(keys, key) {
			break
		}
		keys = append(keys, key)
		remaining = tail
	}
	if len(keys) > 1 {
		instruction := strings.TrimSpace(remaining)
		names, missing, _, blocks := s.blocks(ctx, keys, func(key string) (loaded, bool) {
			return s.load(ctx, commands[key].Identifier)
		}, func(name string) string {
			return fmt.Sprintf("[Loaded as part of the stacked skill invocation \"%s\".]", name)
		}, blockOptions{})
		if len(blocks) > 0 {
			var slashed []string
			for _, k := range keys {
				slashed = append(slashed, "/"+k)
			}
			header := scaffold(fmt.Sprintf("\"%s\" stacked skill bundle", strings.Join(slashed, " ")), names, nil, missing, nil, "", instruction)
			return Invocation{Message: strings.Join(append([]string{header}, blocks...), "\n\n"), Loaded: names, Missing: missing}, true, nil
		}
	}
	l, ok := s.load(ctx, commands[key].Identifier)
	if !ok {
		return Invocation{}, false, fmt.Errorf("The skill of /%s could not be loaded.", key)
	}
	note := fmt.Sprintf("[IMPORTANT: The user has invoked the \"%s\" skill, indicating they want you to follow its instructions. The full skill content is loaded below.]", l.name)
	return Invocation{Message: s.block(ctx, l, note, rest, ""), Loaded: []string{l.name}}, true, nil
}

// Skills the session starts with, by -s: their messages for the system prompt, the names loaded and the identifiers missing, a
// disabled skill counted as missing
func (s *Scope) Preload(ctx context.Context, identifiers []string, already map[string]bool) (string, []string, []string) {
	ids := make([]string, len(identifiers))
	for i, id := range identifiers {
		ids[i] = strings.TrimSpace(id)
	}
	names, missing, _, parts := s.blocks(ctx, ids, func(id string) (loaded, bool) { return s.load(ctx, id) }, func(name string) string {
		return fmt.Sprintf("[IMPORTANT: The user launched this CLI session with the \"%s\" skill preloaded. Treat its instructions as active guidance for the duration of this session unless the user overrides them.]", name)
	}, blockOptions{label: func(id string) string { return s.missingLabel(ctx, id) }, disabled: s.Disabled(), disabledMissing: true, already: already})
	return strings.Join(parts, "\n\n"), names, missing
}

// Names of skills.auto_load, each once
func (s *Scope) autoLoad() []string {
	var out []string
	for _, name := range s.Config.GetStringSlice(skillsconfig.AutoLoad) {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// Skills of skills.auto_load as messages for the system prompt, the names loaded and those missing or disabled
func (s *Scope) AutoLoaded(ctx context.Context) (string, []string, []string) {
	auto := s.autoLoad()
	if len(auto) == 0 {
		return "", nil, nil
	}
	names, missing, _, parts := s.blocks(ctx, auto, func(id string) (loaded, bool) { return s.load(ctx, id) }, func(name string) string {
		return fmt.Sprintf("[IMPORTANT: The \"%s\" skill is auto-loaded via config (skills.auto_load). Treat its instructions as active guidance for the duration of this session unless the user overrides them.]", name)
	}, blockOptions{disabled: s.Disabled(), disabledMissing: true})
	return strings.Join(parts, "\n\n"), names, missing
}

// A message that loads skills as the user typed it: the slash command and its instruction joined by the separator; false for any other
// text. A session's title and transcript show this in place of the message
func Describe(content, separator string) (string, bool) {
	if strings.HasPrefix(content, autoLoadPrefix) {
		return describeAutoLoaded(content)
	}
	if !strings.HasPrefix(content, invokedPrefix) {
		return "", false
	}
	name := ""
	if m := invokedName.FindStringSubmatch(content); m != nil {
		name = strings.TrimSpace(m[1])
	}
	label := name
	if !strings.HasPrefix(name, "/") {
		label = "/" + name
	}
	if instruction, ok := Instruction(content); ok && instruction != "" {
		if instruction = strings.Join(strings.Fields(instruction), " "); instruction != "" {
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

// The user's instruction of a message that loads skills, the text itself for any other message; false for one with no instruction
func Instruction(content string) (string, bool) {
	if !strings.HasPrefix(content, invokedPrefix) {
		return content, true
	}
	if strings.Contains(content, bundleMarker) {
		return between(content, bundleInstruction, bundleFirstBlock, false)
	}
	if strings.Contains(content, invokedMarker) {
		return between(content, instructionPrefix, runtimePrefix, true)
	}
	return "", false
}

// Text after a marker up to a stop, stripped; false when the marker is not there or the span is blank
func between(content, marker, stop string, last bool) (string, bool) {
	at := strings.Index(content, marker)
	if last {
		at = strings.LastIndex(content, marker)
	}
	if at < 0 {
		return "", false
	}
	span, _, _ := strings.Cut(content[at+len(marker):], stop)
	span = strings.TrimSpace(span)
	return span, span != ""
}

// What the user typed after the last skill an auto loaded message carries, the bare slash name without it
func describeAutoLoaded(content string) (string, bool) {
	name := ""
	if m := autoLoadName.FindStringSubmatch(content); m != nil {
		name = m[1]
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
		closing := ""
		for _, pair := range trailingBlock {
			if strings.HasPrefix(tail, pair[0]) {
				closing = pair[1]
				break
			}
		}
		if closing == "" {
			break
		}
		if cut := strings.Index(tail, closing); cut >= 0 {
			tail = strings.TrimLeftFunc(tail[cut+len(closing):], unicode.IsSpace)
		} else {
			tail = ""
		}
	}
	if joined := strings.Join(strings.Fields(tail), " "); joined != "" {
		return joined, true
	}
	return bare()
}

// The prompt /learn sends: learn a skill from what the user names, the workflow just done when they name nothing
func Learn(request string) string {
	request = strings.TrimSpace(request)
	if request == "" {
		request = learnDefault
	}
	return "[/learn] The user wants you to learn a reusable skill from the request below, and save it.\n\nTHE REQUEST:\n" + request + "\n\n" + learnBody
}

// Slash commands of the skills and the bundles of the scope by key, a skill's winning over a bundle's of the same key
func (s *Scope) Slash(taken Taken) (map[string]Command, error) {
	commands := s.Commands(taken)
	bundles, err := s.Bundles()
	if err != nil {
		return nil, err
	}
	for slug, bundle := range bundles {
		if _, claimed := commands[slug]; !claimed && (taken == nil || !taken(slug)) {
			commands[slug] = Command{Key: slug, Name: bundle.Name, Description: bundle.Description, Skills: bundle.Skills}
		}
	}
	return commands, nil
}

// What a reload of the skills changed among the slash commands
type Reloaded struct {
	Added, Removed []Command
	// Slash commands of skills and bundles now
	Total int
}

// Scans the skills again and says which slash commands came and went
func (s *Scope) Reload(taken Taken) (Reloaded, error) {
	before, err := s.Slash(taken)
	if err != nil {
		return Reloaded{}, err
	}
	s.registry.Forget()
	after, err := s.Slash(taken)
	if err != nil {
		return Reloaded{}, err
	}
	out := Reloaded{Total: len(after)}
	for _, key := range slices.Sorted(maps.Keys(after)) {
		if _, had := before[key]; !had {
			out.Added = append(out.Added, after[key])
		}
	}
	for _, key := range slices.Sorted(maps.Keys(before)) {
		if _, has := after[key]; !has {
			out.Removed = append(out.Removed, before[key])
		}
	}
	return out, nil
}
