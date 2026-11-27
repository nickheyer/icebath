package skills

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// How ready a skill is to use, as a view reports it
const (
	Available   = "available"
	SetupNeeded = "setup_needed"
	Unsupported = "unsupported"
)

// Start of every refusal to guess between skills of one name
const AmbiguousPrefix = "Ambiguous skill name "

const lookupHint = "Use a skill name or relative path within the skills directory."

// Lower case phrases a skill is warned about holding
var injectionPhrases = []string{
	"ignore previous instructions", "ignore all previous", "you are now", "disregard your", "forget your instructions",
	"new instructions:", "system prompt:", "<system>", "]]>",
}

// Result of a skill operation as the model reads it, success and error or what was asked for
type Result map[string]any

func failure(message string, extra Result) Result {
	out := Result{"success": false, "error": message}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// Whether the result is a success
func (r Result) OK() bool {
	ok, _ := r["success"].(bool)
	return ok
}

// Text of a field, empty when it is not text
func (r Result) Text(key string) string {
	return str(r[key])
}

// Why a lookup name could leave the skill folders it is joined onto, empty when it can't
func lookupError(name string) string {
	candidate := strings.TrimSpace(name)
	runes := []rune(candidate)
	if strings.HasPrefix(candidate, "/") || strings.HasPrefix(strings.ReplaceAll(candidate, "/", `\`), `\\`) || len(runes) >= 2 && runes[1] == ':' {
		return "Skill name must be a relative path within the skills directory."
	}
	if traversal(candidate) {
		return "Skill name cannot contain '..' path traversal components."
	}
	return ""
}

// Whether a path has a .. part
func traversal(p string) bool {
	return slices.Contains(strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }), "..")
}

// Why a path joined onto a folder lands outside it, empty when it stays inside
func outside(target, dir string) string {
	if !within(resolve(target), resolve(dir)) {
		return "Path escapes the skill directory."
	}
	return ""
}

// Whether the skill is disabled by any of the names: globally or on the surface
func (s *Scope) off(names ...string) bool {
	disabled := s.Disabled()
	return slices.ContainsFunc(names, func(name string) bool { return disabled[name] })
}

// Where a skill was found: its folder, none for a flat markdown skill, and its markdown file
type location struct {
	dir string
	md  string
}

// Path with its last extension replaced
func withExt(p, ext string) string {
	base := filepath.Base(p)
	if old := filepath.Ext(base); old != "" && old != base {
		return p[:len(p)-len(old)] + ext
	}
	return p + ext
}

// Whether a flat markdown file belongs to a skill folder above it
func packageOwned(p, searchRoot string) bool {
	rel, err := filepath.Rel(searchRoot, p)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for depth := 1; depth < len(parts); depth++ {
		if fsx.IsFile(filepath.Join(searchRoot, filepath.FromSlash(strings.Join(parts[:depth], "/")), "SKILL.md")) {
			return true
		}
	}
	return false
}

// Every place the name can mean across the folders: a folder or flat file at the path, a skill folder or declared name, a flat file
// of the name; one per file
func candidates(name, category string, dirs []string) []location {
	var out []location
	seen := map[string]bool{}
	record := func(dir, md string) {
		if key := resolve(md); !seen[key] {
			seen[key] = true
			out = append(out, location{dir, md})
		}
	}
	for _, searchDir := range dirs {
		for _, direct := range []string{name, category} {
			if direct == "" {
				continue
			}
			p := filepath.Join(searchDir, filepath.FromSlash(direct))
			flat := withExt(p, ".md")
			if !supportPath(p) && fsx.IsDir(p) && fsx.Exists(filepath.Join(p, "SKILL.md")) {
				record(p, filepath.Join(p, "SKILL.md"))
			} else if fsx.Exists(flat) && !supportPath(flat) && !packageOwned(flat, searchDir) {
				record("", flat)
			}
		}
		for _, skillMD := range skillFiles(searchDir) {
			if filepath.Base(filepath.Dir(skillMD)) == name || declaredName(skillMD) == name {
				record(filepath.Dir(skillMD), skillMD)
			}
		}
		filepath.WalkDir(searchDir, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil
			case d.IsDir() && p != searchDir && excluded[d.Name()]:
				return filepath.SkipDir
			case !d.IsDir() && d.Name() == name+".md" && !supportPath(p) && !packageOwned(p, searchDir):
				record("", p)
			}
			return nil
		})
	}
	return out
}

// Name a SKILL.md declares, empty when it declares none or the file can't be read
func declaredName(skillMD string) string {
	content, err := readText(skillMD)
	if err != nil {
		return ""
	}
	fm, _ := Parse(content)
	return str(fm["name"])
}

// Most specific folder holding the file
func owner(md string, dirs []string) string {
	best, depth := "", -1
	for _, dir := range dirs {
		if within(md, dir) {
			if n := len(strings.Split(filepath.ToSlash(filepath.Clean(dir)), "/")); n > depth {
				best, depth = dir, n
			}
		}
	}
	return best
}

// Exact name a location loads by: its path relative to the folder holding it
func relative(loc location, dirs []string) string {
	target := loc.dir
	if target == "" {
		target = withExt(loc.md, "")
	}
	root := owner(loc.md, dirs)
	if root == "" {
		return target
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

// The one skill the name means in the roots, else the refusal: none, several of one precedence, or held back
func (s *Scope) locate(name, category string, roots []root) (location, Result) {
	if len(roots) == 0 {
		return location{}, failure("Skills directory does not exist yet. It will be created on first install.", nil)
	}
	dirs := make([]string, len(roots))
	tiers := map[string]int{}
	for i, r := range roots {
		dirs[i] = r.dir
		if _, ok := tiers[r.dir]; !ok {
			tiers[r.dir] = r.tier
		}
	}
	found := candidates(name, category, dirs)
	if len(found) > 1 {
		ranked := make([]candidate, len(found))
		for i, loc := range found {
			rootDir := owner(loc.md, dirs)
			ranked[i] = candidate{tiers[rootDir], rootDir, rank(loc.md, rootDir), loc.md}
		}
		won, contenders := pick(ranked)
		if won >= 0 {
			found = []location{found[won]}
		} else {
			kept := make([]location, len(contenders))
			for i, j := range contenders {
				kept[i] = found[j]
			}
			found = kept
		}
	}
	if len(found) > 1 {
		var paths, loadNames []string
		for _, loc := range found {
			paths = append(paths, loc.md)
			if rel := relative(loc, dirs); rel != name && !slices.Contains(loadNames, rel) {
				loadNames = append(loadNames, rel)
			}
		}
		slices.Sort(loadNames)
		use := ""
		if len(loadNames) > 0 {
			use = "use one of " + strings.Join(loadNames, ", ") + ". "
		}
		return location{}, failure(fmt.Sprintf("%s'%s': %s%d different skills share this name in the same skills directory tier; refusing to guess.", AmbiguousPrefix, name, use, len(found)), Result{
			"matches":    paths,
			"load_names": loadNames,
			"hint":       "Pass the exact relative path instead of the bare name, or rename one of the colliding skills so each name is unique.",
		})
	}
	if len(found) == 1 && tiers[owner(found[0].md, dirs)] == TierProject && s.quarantined(found[0].md) {
		return location{}, failure(fmt.Sprintf("Project skill '%s' is quarantined: the security scan flagged its content as dangerous. It will not load until the repo's skill content changes and passes a re-scan.", name), Result{
			"hint": "Inspect the skill in the repo checkout, or untrust the repo by removing it from skills.trusted_project_dirs.",
		})
	}
	if len(found) == 0 || !fsx.Exists(found[0].md) {
		available := []string{}
		for _, e := range s.Loadable(false) {
			if len(available) == 20 {
				break
			}
			available = append(available, e.Load)
		}
		return location{}, failure(fmt.Sprintf("Skill '%s' not found.", name), Result{"available_skills": available, "hint": "Use skills_list to see all available skills"})
	}
	return found[0], nil
}

// Loads a skill's SKILL.md, or one of its files, with what it needs to be used; preprocess applies the skill variables and shell
// snippets settings
func (s *Scope) View(ctx context.Context, name, file string, preprocess bool) Result {
	if problem := lookupError(name); problem != "" {
		return failure(problem, Result{"hint": lookupHint})
	}
	category := ""
	if strings.Contains(name, ":") {
		served, local := s.viewPlugin(ctx, name, file, preprocess)
		if served != nil {
			return served
		}
		category = local
		if problem := lookupError(category); category != "" && problem != "" {
			return failure(problem, Result{"hint": lookupHint})
		}
	}
	roots := s.searched()
	var dirs []string
	for _, r := range roots {
		dirs = append(dirs, r.dir)
	}
	loc, refused := s.locate(name, category, roots)
	if refused != nil {
		return refused
	}
	content, err := readText(loc.md)
	if err != nil {
		return failure(fmt.Sprintf("Failed to read skill '%s': %s", name, pathError(err)), nil)
	}
	s.warn(name, loc.md, content, dirs)
	fm, _ := Parse(content)
	if !platformMatches(fm["platforms"]) {
		return failure(fmt.Sprintf("Skill '%s' is not supported on this platform.", name), Result{"readiness_status": Unsupported})
	}
	declared := fm.Name(filepath.Base(filepath.Dir(loc.md)))
	rel := relative(loc, dirs)
	if s.off(declared, rel) && (s.off(declared) || s.loadsAs(loc.md, rel)) {
		return failure(fmt.Sprintf("Skill '%s' is disabled. Enable it with `icebath skills config` or inspect the files directly on disk.", declared), nil)
	}
	if file != "" && loc.dir != "" {
		return s.serveFile(ctx, loc.dir, file, name, "Use a relative path within the skill directory", true, false)
	}
	meta := fm.Meta()
	field := func(key string) any {
		if config.Truthy(meta[key]) {
			return meta[key]
		}
		return fm[key]
	}
	path, err := filepath.Rel(s.Dir(), loc.md)
	if err != nil || !filepath.IsLocal(path) {
		path, _ = filepath.Rel(filepath.Dir(filepath.Dir(loc.md)), loc.md)
	}
	skillName := filepath.Base(loc.dir)
	if loc.dir == "" {
		skillName = filepath.Base(withExt(loc.md, ""))
	}
	skillName = fm.Name(skillName)
	rendered := content
	if preprocess {
		rendered = s.preprocess(ctx, content, loc.dir)
	}
	description := any("")
	if v, ok := fm["description"]; ok {
		description = v
	}
	result := Result{
		"success":        true,
		"name":           skillName,
		"description":    description,
		"tags":           tags(field("tags")),
		"related_skills": tags(field("related_skills")),
		"content":        rendered,
		"path":           filepath.ToSlash(path),
		"skill_dir":      nil,
		"linked_files":   nil,
		"usage_hint":     nil,
	}
	if loc.dir != "" {
		result["skill_dir"] = loc.dir
	}
	if linked := linkedFiles(loc.dir); len(linked) > 0 {
		result["linked_files"] = linked
		result["usage_hint"] = "To view linked files, call skill_view(name, file_path) where file_path is e.g. 'references/api.md' or 'assets/config.yaml'"
	}
	for key, value := range s.readiness(ctx, fm, skillName) {
		result[key] = value
	}
	if note := s.depsNote(ctx, fm); note != "" {
		result["deps_note"] = note
	}
	markRead(ctx, loc.md)
	if config.Truthy(fm["compatibility"]) {
		result["compatibility"] = fm["compatibility"]
	}
	if metadata := mapping(fm["metadata"]); metadata != nil {
		result["metadata"] = metadata
	}
	return result
}

// Reason of a file error followed by the path it is about
func pathError(err error) string {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err.Error() + ": " + pe.Path
	}
	return err.Error()
}

// Warns when a loaded skill lies outside every skill folder, or holds phrases a prompt injection uses
func (s *Scope) warn(name, md, content string, dirs []string) {
	trusted := append([]string{resolve(s.Dir())}, dirs...)
	var warnings []string
	if !slices.ContainsFunc(trusted, func(dir string) bool { return within(resolve(md), resolve(dir)) }) {
		warnings = append(warnings, "skill file is outside the trusted skills directory: "+md)
	}
	if injection(content) {
		warnings = append(warnings, "skill content contains patterns that may indicate prompt injection")
	}
	if len(warnings) > 0 && s.registry.first("warned\x00"+md) {
		slog.Warn("skills: skill security warning", "skill", name, "warnings", strings.Join(warnings, "; "))
	}
}

func injection(content string) bool {
	lower := strings.ToLower(content)
	return slices.ContainsFunc(injectionPhrases, func(phrase string) bool { return strings.Contains(lower, phrase) })
}

// Tool programs a skill declares it needs: the package manager's own installed when missing and the profile allows it, when commands run
// on this machine; any other looked for on the path. Those that can't be had are named with how to get them
func (s *Scope) depsNote(ctx context.Context, fm Frontmatter) string {
	if !config.Truthy(fm["deps"]) {
		return ""
	}
	pm := s.registry.shared().packages
	var failed, managed, missing, other []string
	for _, dep := range list(fm["deps"]) {
		if dep = strings.TrimSpace(dep); dep == "" {
			continue
		}
		if pm != nil && s.Remote == "" {
			if _, err := pm.Package(dep); err == nil {
				managed = append(managed, dep)
				if _, err := pm.Ensure(ctx, s.Config, dep, packages.Ensuring{}); err != nil {
					failed = append(failed, dep+": "+err.Error())
				}
				continue
			}
		}
		other = append(other, dep)
		if _, err := exec.LookPath(dep); err != nil {
			missing = append(missing, dep)
		}
	}
	var notes []string
	if len(failed) > 0 {
		notes = append(notes, "Tool dependencies could not be installed — "+strings.Join(failed, "; ")+". Run `icebath pm install "+strings.Join(managed, " ")+
			"` and reload.")
	}
	if len(missing) > 0 {
		notes = append(notes, "Tool dependencies could not be found — "+strings.Join(missing, ", ")+". Install "+strings.Join(other, " ")+
			" on this machine and reload.")
	}
	return strings.Join(notes, " ")
}

// Supporting files of a skill folder by group, empty groups left out
func linkedFiles(dir string) map[string][]string {
	out := map[string][]string{}
	if dir == "" {
		return out
	}
	groups := []struct {
		sub       string
		exts      []string
		recursive bool
	}{
		{"references", []string{".md"}, false},
		{"templates", []string{".md", ".py", ".yaml", ".yml", ".json", ".tex", ".sh"}, true},
		{"assets", nil, true},
		{"scripts", []string{".py", ".sh", ".bash", ".js", ".ts", ".rb"}, false},
	}
	for _, group := range groups {
		base := filepath.Join(dir, group.sub)
		if !fsx.Exists(base) {
			continue
		}
		var found []string
		add := func(p string, d fs.DirEntry) {
			if group.exts == nil && !fsx.IsFile(p) || group.exts != nil && !slices.Contains(group.exts, filepath.Ext(d.Name())) {
				return
			}
			if rel, err := filepath.Rel(dir, p); err == nil {
				found = append(found, filepath.ToSlash(rel))
			}
		}
		if group.recursive {
			filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err == nil && p != base && (group.exts == nil || !d.IsDir()) {
					add(p, d)
				}
				return nil
			})
		} else if entries, err := os.ReadDir(base); err == nil {
			for _, entry := range entries {
				add(filepath.Join(base, entry.Name()), entry)
			}
		}
		if len(found) > 0 {
			out[group.sub] = found
		}
	}
	return out
}

// Files of a skill folder besides SKILL.md, by support folder, other for source files elsewhere
func availableFiles(dir string) map[string][]string {
	sources := []string{".md", ".py", ".yaml", ".yml", ".json", ".tex", ".sh"}
	out := map[string][]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir || !fsx.IsFile(p) || d.Name() == "SKILL.md" {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		top, _, nested := strings.Cut(rel, "/")
		switch {
		case nested && slices.Contains(supportDirs, top):
			out[top] = append(out[top], rel)
		case slices.Contains(sources, filepath.Ext(p)):
			out["other"] = append(out["other"], rel)
		}
		return nil
	})
	return out
}

// One file of a skill folder as a view
func (s *Scope) serveFile(ctx context.Context, dir, file, label, hint string, listAvailable, readPrefix bool) Result {
	extra := Result{}
	if hint != "" {
		extra["hint"] = hint
	}
	if traversal(file) {
		return failure("Path traversal ('..') is not allowed.", extra)
	}
	target := filepath.Join(dir, filepath.FromSlash(file))
	if problem := outside(target, dir); problem != "" {
		return failure(problem, extra)
	}
	if !fsx.IsFile(target) {
		listing := Result{}
		if listAvailable {
			listing = Result{"available_files": availableFiles(dir), "hint": "Use one of the available file paths listed above"}
		}
		return failure(fmt.Sprintf("File '%s' not found in skill '%s'.", file, label), listing)
	}
	content, err := readText(target)
	if err != nil {
		if readPrefix {
			return failure(fmt.Sprintf("Failed to read '%s': %s", file, pathError(err)), nil)
		}
		return failure(pathError(err), nil)
	}
	if !readPrefix {
		markRead(ctx, target)
	}
	ext := filepath.Ext(target)
	if ext == filepath.Base(target) {
		ext = ""
	}
	return Result{"success": true, "name": label, "file": file, "content": content, "file_type": ext}
}

// A namespaced skill as a view, or the on disk name name:skill stands for when no namespace of the name serves it
func (s *Scope) viewPlugin(ctx context.Context, name, file string, preprocess bool) (Result, string) {
	namespace, bare, _ := strings.Cut(name, ":")
	if namespace == "plugin" && strings.Contains(bare, ":") {
		name = bare
		namespace, bare, _ = strings.Cut(name, ":")
	}
	if !namespaceName.MatchString(namespace) {
		return failure(fmt.Sprintf("Invalid namespace '%s' in '%s'. Namespaces must match [a-zA-Z0-9_-]+.", namespace, name), nil), ""
	}
	if p, ok := s.plugin(name); ok {
		if !fsx.Exists(p.Path) {
			return failure(fmt.Sprintf("Skill '%s' file no longer exists at %s. Try again after icebath restarts.", name, p.Path), nil), ""
		}
		return s.servePlugin(ctx, p, file, preprocess), ""
	}
	if siblings := s.namespace(namespace); len(siblings) > 0 {
		qualified := make([]string, len(siblings))
		for i, sibling := range siblings {
			qualified[i] = namespace + ":" + sibling
		}
		return failure(fmt.Sprintf("Skill '%s' not found in plugin '%s'.", bare, namespace), Result{
			"available_skills": qualified,
			"hint":             fmt.Sprintf("The '%s' plugin provides %d skill(s).", namespace, len(siblings)),
		}), ""
	}
	if bare != "" {
		return nil, namespace + "/" + bare
	}
	return nil, ""
}

// A namespaced skill as a view, read only, with the other skills of its namespace named
func (s *Scope) servePlugin(ctx context.Context, p Plugin, file string, preprocess bool) Result {
	qualified := p.Qualified()
	content, err := readText(p.Path)
	if err != nil {
		return failure(fmt.Sprintf("Failed to read skill '%s': %s", qualified, pathError(err)), nil)
	}
	fm, _ := Parse(content)
	if s.off(qualified) {
		return failure(fmt.Sprintf("Skill '%s' is disabled.", qualified), nil)
	}
	if !platformMatches(fm["platforms"]) {
		return failure(fmt.Sprintf("Skill '%s' is not supported on this platform.", qualified), Result{"readiness_status": Unsupported})
	}
	dir := filepath.Dir(p.Path)
	if file != "" {
		return s.serveFile(ctx, dir, file, qualified, "", false, true)
	}
	if injection(content) && s.registry.first("warned\x00"+p.Path) {
		slog.Warn("skills: plugin skill contains patterns that may indicate prompt injection", "skill", qualified)
	}
	var siblings []string
	for _, sibling := range s.namespace(p.Namespace) {
		if sibling != p.Name {
			siblings = append(siblings, sibling)
		}
	}
	banner := "[Bundle context: This skill is part of the '" + p.Namespace + "' plugin."
	if len(siblings) > 0 {
		banner += "\nSibling skills: " + strings.Join(siblings, ", ") + ".\nUse qualified form to invoke siblings (e.g. " + p.Namespace + ":" + siblings[0] + ")."
	}
	banner += "]\n\n"
	rendered := content
	if preprocess {
		rendered = s.preprocess(ctx, content, dir)
	}
	var linked any
	if files := pluginFiles(dir); len(files) > 0 {
		linked = files
	}
	return Result{
		"success":          true,
		"name":             qualified,
		"content":          banner + rendered,
		"description":      capDescription(text(fm["description"])),
		"linked_files":     linked,
		"readiness_status": Available,
		"skill_dir":        dir,
	}
}

// Every file in the support folders of a namespaced skill, sorted, kept inside it
func pluginFiles(dir string) map[string][]string {
	out := map[string][]string{}
	for _, sub := range supportDirs {
		var files []string
		filepath.WalkDir(filepath.Join(dir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil && fsx.IsFile(p) && outside(p, dir) == "" {
				if rel, err := filepath.Rel(dir, p); err == nil {
					files = append(files, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		slices.Sort(files)
		if len(files) > 0 {
			out[sub] = files
		}
	}
	return out
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// An environment variable a skill needs
type Variable struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Where to get a value
	Help string `json:"help,omitempty"`
	// What it is needed for
	For      string `json:"required_for,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// Text of a value when it is non blank text
func clean(v any) string {
	return strings.TrimSpace(str(v))
}

// First of the values that counts as set
func firstSet(values ...any) any {
	for _, v := range values {
		if config.Truthy(v) {
			return v
		}
	}
	return nil
}

// A mapping or a list as a list
func entries(v any) []any {
	switch t := v.(type) {
	case map[string]any:
		return []any{t}
	case []any:
		return t
	}
	return nil
}

// Variables a skill needs, from required_environment_variables, setup.collect_secrets and prerequisites.env_vars, each once
func Variables(fm Frontmatter) []Variable {
	setup := mapping(fm["setup"])
	setupHelp := clean(setup["help"])
	var raw []map[string]any
	for _, item := range entries(fm["required_environment_variables"]) {
		switch t := item.(type) {
		case string:
			raw = append(raw, map[string]any{"name": t})
		case map[string]any:
			raw = append(raw, t)
		}
	}
	for _, item := range entries(setup["collect_secrets"]) {
		if m := mapping(item); m != nil {
			entry := map[string]any{"name": m["env_var"], "prompt": m["prompt"]}
			if url := strings.TrimSpace(text(firstSet(m["provider_url"], m["url"]))); url != "" {
				entry["url"] = url
			}
			raw = append(raw, entry)
		}
	}
	if legacy := mapping(fm["prerequisites"])["env_vars"]; config.Truthy(legacy) {
		switch t := legacy.(type) {
		case map[string]any:
			for name := range t {
				raw = append(raw, map[string]any{"name": name})
			}
		default:
			for _, name := range list(t) {
				raw = append(raw, map[string]any{"name": name})
			}
		}
	}
	var out []Variable
	for _, entry := range raw {
		name := strings.TrimSpace(text(firstSet(entry["name"], entry["env_var"])))
		if name == "" || !variableName.MatchString(name) || slices.ContainsFunc(out, func(v Variable) bool { return v.Name == name }) {
			continue
		}
		v := Variable{Name: name, Prompt: "Enter value for " + name, For: clean(entry["required_for"]), Optional: config.Truthy(entry["optional"])}
		if prompt := firstSet(entry["prompt"]); prompt != nil {
			v.Prompt = strings.TrimSpace(text(prompt))
		}
		help := firstSet(entry["help"], entry["provider_url"], entry["url"])
		if help == nil && setupHelp != "" {
			help = setupHelp
		}
		v.Help = clean(help)
		out = append(out, v)
	}
	return out
}

// Key of the profile setting that keeps a variable's value
func variableKey(name string) string {
	return skillsconfig.Variables + "." + strings.ToLower(name)
}

// Value of a variable: the profile's environment's, else the one kept for the profile
func envValue(cfg *config.Config, name string) string {
	if value := cfg.Getenv(name); value != "" {
		return value
	}
	return cfg.GetString(variableKey(name))
}

// Settings the user keeps the values of skills' variables in, one secret each; the process's own variable of the name wins
func (s *Scope) VariableSettings() []config.Setting {
	var out []config.Setting
	for _, e := range s.declaring() {
		for _, v := range Variables(e.Frontmatter) {
			key := variableKey(v.Name)
			if slices.ContainsFunc(out, func(o config.Setting) bool { return o.Key == key }) {
				continue
			}
			about := v.Prompt + " (skill " + e.Name + ")"
			if v.Help != "" {
				about += ". " + v.Help
			}
			out = append(out, config.Setting{Key: key, Title: v.Name, About: about, Default: "", Secret: true})
		}
	}
	return out
}

// What the skills a session loaded need from the commands it runs
type needs struct {
	variables map[string]bool
	files     map[string]bool
}

// Records what a skill the session loaded needs from its commands
func (s *Scope) need(variables, files []string) {
	if s.Session == "" || len(variables)+len(files) == 0 {
		return
	}
	st := s.registry.shared()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.needs == nil {
		st.needs = map[string]*needs{}
	}
	n := st.needs[s.Session]
	if n == nil {
		n = &needs{variables: map[string]bool{}, files: map[string]bool{}}
		st.needs[s.Session] = n
	}
	for _, name := range variables {
		n.variables[name] = true
	}
	for _, file := range files {
		n.files[file] = true
	}
}

// What the skills the session loaded need from the commands it runs: their environment variables with values, the process's own or
// the ones kept for the profile, and the credential files of the profile's folder they read, by path in it
func (r *Registry) Needs(cfg *config.Config, session string) (map[string]string, []string) {
	st := r.shared()
	st.mu.Lock()
	n := st.needs[session]
	var variables, files []string
	if n != nil {
		variables, files = sorted(n.variables), sorted(n.files)
	}
	st.mu.Unlock()
	values := map[string]string{}
	for _, name := range variables {
		if value := envValue(cfg, name); value != "" {
			values[name] = value
		}
	}
	return values, files
}

// Forgets what the skills a session loaded need, the session being closed or deleted
func (r *Registry) Release(_ context.Context, session string) {
	st := r.shared()
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.needs, session)
}

// Whether the skill's variables and credential files are there, asking the user for missing variables when someone can be asked
func (s *Scope) readiness(ctx context.Context, fm Frontmatter, skill string) Result {
	required := Variables(fm)
	missing := func() []string {
		var out []string
		for _, v := range required {
			if !v.Optional && envValue(s.Config, v.Name) == "" {
				out = append(out, v.Name)
			}
		}
		return out
	}
	skipped := false
	if absent := missing(); len(absent) > 0 && s.Ask != nil {
		for _, v := range required {
			if !slices.Contains(absent, v.Name) {
				continue
			}
			value, err := s.Ask(ctx, skill, v)
			if err == nil && value != "" {
				err = s.Config.Set(variableKey(v.Name), value)
			}
			if err != nil || value == "" {
				skipped = true
			}
		}
	}
	remaining := missing()
	var present []string
	for _, v := range required {
		if !slices.Contains(remaining, v.Name) {
			present = append(present, v.Name)
		}
	}
	var files, missingFiles []string
	for _, item := range entries(fm["required_credential_files"]) {
		rel := ""
		switch t := item.(type) {
		case map[string]any:
			rel = clean(firstSet(t["path"], t["name"]))
		case string:
			rel = strings.TrimSpace(t)
		}
		if rel == "" {
			continue
		}
		if filepath.IsAbs(rel) || traversal(rel) || !fsx.IsFile(filepath.Join(s.Home(), filepath.FromSlash(rel))) {
			missingFiles = append(missingFiles, rel)
			continue
		}
		files = append(files, rel)
	}
	s.need(present, files)
	needed := len(remaining) > 0 || len(missingFiles) > 0
	status := Available
	if needed {
		status = SetupNeeded
	}
	out := Result{
		"required_environment_variables":         required,
		"missing_required_environment_variables": strs.List(remaining),
		"missing_credential_files":               strs.List(missingFiles),
		"setup_needed":                           needed,
		"setup_skipped":                          skipped,
		"readiness_status":                       status,
	}
	if out["required_environment_variables"] == nil {
		out["required_environment_variables"] = []Variable{}
	}
	help := ""
	for _, v := range required {
		if v.Help != "" {
			help = v.Help
			break
		}
	}
	if help != "" {
		out["setup_help"] = help
	}
	if needed {
		var items []string
		for _, name := range remaining {
			items = append(items, "env $"+name)
		}
		for _, file := range missingFiles {
			items = append(items, "file "+file)
		}
		note := "Setup needed before using this skill: missing " + strings.Join(items, ", ") + "."
		if help != "" {
			note += " " + help
		}
		if s.Remote != "" {
			note += " " + strings.ToUpper(s.Remote) + "-backed skills need these requirements available inside the remote environment as well."
		}
		out["setup_note"] = note
	}
	return out
}
