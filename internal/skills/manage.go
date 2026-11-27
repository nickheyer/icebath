package skills

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	skillsconfig "github.com/nickheyer/icebath/internal/skills/config"
	"github.com/nickheyer/icebath/internal/tools/fuzzy"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Most characters of a SKILL.md or a supporting file text, and bytes of a supporting file
const (
	MaxContent = 100_000
	MaxFile    = 1 << 20
)

// Most operations of one skill_manage call
const maxOps = 20

// Kind of risky action a skill write asks approval for when skills.write_approval is on
const RiskWrite = "writes skills"

const nameRule = "Use lowercase letters, numbers, hyphens, dots, and underscores."

var (
	validName    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	supportFirst = []string{"assets", "references", "scripts", "templates"}
)

const (
	needsOld  = "old_string is required for 'patch' and must be the EXACT text currently in the file. Read the target file first (read_file on the skill's SKILL.md, or the file named by file_path) and copy the snippet verbatim, then retry 'patch'. Do NOT fall back to action='write_file' — that rewrites the entire file and destroys unrelated content."
	needsNew  = "new_string is required for 'patch'. Use an empty string to delete matched text."
	eitherOr  = "Pass EITHER content (full SKILL.md rewrite) OR old_string/new_string (targeted replacement), not both."
	lintHint  = "The write succeeded. These are advisory authoring-convention findings (not blockers) — fix them with skill_manage(action='patch') to match Icebath skill standards."
	createHow = "To add reference files, templates, or scripts, use skill_manage(action='write_file', name='%s', file_path='references/example.md', file_content='...')"
)

// One operation of skill_manage, as the model sends it
type Op struct {
	Action string `json:"action"`
	Name   string `json:"name"`
	// Full SKILL.md text, of a create or a full rewrite
	Content *string `json:"content"`
	// Folder of a new skill
	Category    any     `json:"category"`
	FilePath    *string `json:"file_path"`
	FileContent *string `json:"file_content"`
	OldString   *string `json:"old_string"`
	NewString   *string `json:"new_string"`
	ReplaceAll  bool    `json:"replace_all"`
	// Skill a deleted one was merged into, which the curator names
	AbsorbedInto *string `json:"absorbed_into"`
}

func set(p *string) bool { return p != nil && *p != "" }

func identifierError(value, label, invalid string) string {
	if utf8.RuneCountInString(value) > MaxName {
		return fmt.Sprintf("%s exceeds %d characters.", label, MaxName)
	}
	if !validName.MatchString(value) {
		return invalid
	}
	return ""
}

func nameError(name string) string {
	if name == "" {
		return "Skill name is required."
	}
	return identifierError(name, "Skill name", fmt.Sprintf("Invalid skill name '%s'. %s Must start with a letter or digit.", name, nameRule))
}

func categoryError(category any) string {
	if category == nil {
		return ""
	}
	s, ok := category.(string)
	if !ok {
		return "Category must be a string."
	}
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	invalid := fmt.Sprintf("Invalid category '%s'. %s Categories must be a single directory name.", s, nameRule)
	if strings.ContainsAny(s, `/\`) {
		return invalid
	}
	return identifierError(s, "Category", invalid)
}

// Why SKILL.md text is not a skill: no frontmatter, no name or description, a description too long, or no body
func frontmatterError(content string) string {
	if strings.TrimSpace(content) == "" {
		return "Content cannot be empty."
	}
	for strings.HasPrefix(content, "\uFEFF") {
		content = strings.TrimPrefix(content, "\uFEFF")
	}
	if !strings.HasPrefix(content, "---") {
		return "SKILL.md must start with YAML frontmatter (---). See existing skills for format."
	}
	end := frontmatterEnd.FindStringIndex(content[3:])
	if end == nil {
		return "SKILL.md frontmatter is not closed. Ensure you have a closing '---' line."
	}
	parsed, err := decodeYAML(content[3 : end[0]+3])
	if err != nil {
		return "YAML frontmatter parse error: " + err.Error()
	}
	fm, ok := parsed.(map[string]any)
	if !ok {
		return "Frontmatter must be a YAML mapping (key: value pairs)."
	}
	for _, field := range []string{"name", "description"} {
		if _, ok := fm[field]; !ok {
			return fmt.Sprintf("Frontmatter must include '%s' field.", field)
		}
	}
	if utf8.RuneCountInString(text(fm["description"])) > MaxDescription {
		return fmt.Sprintf("Description exceeds %d characters.", MaxDescription)
	}
	if strings.TrimSpace(content[end[1]+3:]) == "" {
		return "SKILL.md must have content after the frontmatter (instructions, procedures, etc.)."
	}
	return ""
}

// Description a write sets, false when it sets none
func descriptionOf(content string) (string, bool) {
	fm, _ := Parse(content)
	v, ok := fm["description"]
	return strings.Trim(strings.TrimSpace(text(v)), `'"`), ok
}

// Why a description this write sets is too wide for the index; one the file already had is let be
func budgetError(content string, before *string) string {
	description, _ := descriptionOf(content)
	n := utf8.RuneCountInString(description)
	if n <= IndexDescription {
		return ""
	}
	if before != nil {
		if previous, ok := descriptionOf(*before); ok && previous == description {
			return ""
		}
	}
	return fmt.Sprintf("Description is %d chars; a skill description must fit the %d-char skill index (one sentence, trigger first, ends with a period, without repeating the category name). The index clips anything longer. Move detail into the skill body.", n, IndexDescription)
}

func sizeError(content, label string) string {
	if n := utf8.RuneCountInString(content); n > MaxContent {
		return fmt.Sprintf("%s content is %s characters (limit: %s). Consider splitting into a smaller SKILL.md with supporting files in references/ or templates/.", label, strs.Thousands(n), strs.Thousands(MaxContent))
	}
	return ""
}

// First 120 characters of the description a SKILL.md text sets
func preview(content string) string {
	if len(content) < 3 {
		return ""
	}
	end := frontmatterEnd.FindStringIndex(content[3:])
	if end == nil {
		return ""
	}
	parsed, err := decodeYAML(content[3 : end[0]+3])
	if fm, ok := parsed.(map[string]any); err == nil && ok {
		if v, ok := fm["description"]; ok {
			return strs.Head(text(v), 120)
		}
	}
	return ""
}

// Folder a new skill is written to
func (s *Scope) newDir(name, category string) string {
	base := s.CreateDir()
	if base == "" {
		base = s.Dir()
	}
	return filepath.Join(base, category, name)
}

// Skill folders under a root, links not followed
func skillDirs(root string) []string {
	var out []string
	filepath.WalkDir(root+string(filepath.Separator), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "SKILL.md" && !excludedPath(p) {
			out = append(out, filepath.Dir(p))
		}
		return nil
	})
	return out
}

// Name a SKILL.md declares in its first 4000 characters, cut to the longest a name may be
func shortName(skillMD string) string {
	content, err := readText(skillMD)
	if err != nil {
		return ""
	}
	fm, _ := Parse(strs.Head(content, 4000))
	return strs.Head(str(fm["name"]), MaxName)
}

// Folder of the skill a write names: by its folder's name, its path in the profile's folder, or the one skill declaring the name
func (s *Scope) Find(name string) (string, bool) {
	local := ""
	if strings.ContainsAny(name, `/\`) {
		local = resolve(s.Dir())
	}
	var declared []string
	for _, root := range s.Dirs() {
		if !fsx.Exists(root) {
			continue
		}
		for _, dir := range skillDirs(root) {
			if filepath.Base(dir) == name {
				return dir, true
			}
			if local != "" {
				if rel, err := filepath.Rel(local, resolve(dir)); err == nil && filepath.IsLocal(rel) && filepath.ToSlash(rel) == name {
					return dir, true
				}
			}
			if skillMD := filepath.Join(dir, "SKILL.md"); shortName(skillMD) == name && !slices.ContainsFunc(declared, func(d string) bool {
				return resolve(filepath.Join(d, "SKILL.md")) == resolve(skillMD)
			}) {
				declared = append(declared, dir)
			}
		}
	}
	if len(declared) == 1 {
		return declared[0], true
	}
	return "", false
}

// Not found refusal, naming the other profiles holding a skill of the name
func (s *Scope) notFound(name, suffix string) string {
	message := fmt.Sprintf("Skill '%s' not found in active profile '%s'.", name, s.Config.Profile())
	var others [][2]string
	profiles, _ := os.ReadDir(filepath.Dir(s.Home()))
	for _, profile := range profiles {
		dir := filepath.Join(filepath.Dir(s.Home()), profile.Name(), "skills")
		if profile.Name() == s.Config.Profile() || !fsx.IsDir(dir) {
			continue
		}
		for _, skill := range skillDirs(dir) {
			if filepath.Base(skill) == name {
				others = append(others, [2]string{profile.Name(), skill})
				break
			}
		}
	}
	switch {
	case len(others) == 1:
		message += fmt.Sprintf(" A skill by that name exists in profile '%s' (%s). To edit it, switch profiles (`/profile %s`) or edit the file directly (file tools / terminal).", others[0][0], others[0][1], others[0][0])
	case len(others) > 1:
		var names []string
		for _, other := range others {
			names = append(names, "'"+other[0]+"'")
		}
		message += fmt.Sprintf(" Skills by that name exist in other profiles: %s. Switch profiles (`/profile <name>`) to edit there, or edit the files directly (file tools / terminal).", strings.Join(names, ", "))
	default:
		message += " Use skills_list() to see available skills."
	}
	return message + suffix
}

// Parts of a path, a leading separator kept as one
func parts(p string) []string {
	var out []string
	if strings.HasPrefix(p, "/") || filepath.Separator == '\\' && strings.HasPrefix(p, `\`) {
		out = append(out, string(filepath.Separator))
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part != "." {
			out = append(out, part)
		}
	}
	return out
}

// Why a supporting file path is not one: outside the support folders, or a folder
func filePathError(file string) string {
	if file == "" {
		return "file_path is required."
	}
	p := parts(file)
	if traversal(file) {
		return "Path traversal ('..') is not allowed."
	}
	if len(p) > 0 && p[len(p)-1] == "SKILL.md" && len(p) <= 2 {
		return ""
	}
	if len(p) == 0 || !slices.Contains(supportFirst, p[0]) {
		return fmt.Sprintf("File must be under one of: %s. Got: '%s'", strings.Join(supportFirst, ", "), file)
	}
	if len(p) < 2 {
		return fmt.Sprintf("Provide a file path, not just a directory. Example: '%s/myfile.md'", p[0])
	}
	return ""
}

// Supporting file of a skill a write names, kept inside its folder
func supporting(dir, file string) (string, Result) {
	target := filepath.Join(dir, filepath.FromSlash(file))
	if filepath.IsAbs(file) {
		target = file
	}
	problem := filePathError(file)
	if problem == "" {
		problem = outside(target, dir)
	}
	if problem != "" {
		return "", failure(problem, nil)
	}
	return target, nil
}

// Folder of the skill a write changes, the background review's delete guard run for a delete
func (s *Scope) locateForWrite(ctx context.Context, name, action, suffix string) (string, Result) {
	dir, ok := s.Find(name)
	if !ok {
		return "", failure(s.notFound(name, suffix), nil)
	}
	if action == "delete" {
		if refused := s.reviewDeleteGuard(ctx, name, dir); refused != nil {
			return "", refused
		}
	}
	return dir, nil
}

// Text of a file with its line endings made newlines
func readNormal(path string) (string, error) {
	content, err := readStrict(path)
	return strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n"), err
}

// Writes a file of a skill after the background review's read guard and the description budget, then scans the skill when that is
// on and puts the file back as it was when the scan blocks it
func (s *Scope) guardedWrite(ctx context.Context, name, dir, target, action, label, content string) (Result, error) {
	var original *string
	if fsx.Exists(target) {
		if refused := s.readGuard(ctx, name, target, action, label); refused != nil {
			return refused, nil
		}
		text, err := readNormal(target)
		if err != nil {
			return nil, err
		}
		original = &text
	}
	if target == filepath.Join(dir, "SKILL.md") {
		if problem := budgetError(content, original); problem != "" {
			return failure(problem, nil), nil
		}
	}
	if err := fsx.Replace(target, []byte(content), 0o644); err != nil {
		return nil, err
	}
	blocked, err := s.scanWrite(ctx, dir)
	if err != nil || blocked == "" {
		return nil, err
	}
	if original != nil {
		err = fsx.Replace(target, []byte(*original), 0o644)
	} else if err = os.Remove(target); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return failure(blocked, nil), nil
}

// Why the scan of a skill the agent wrote blocks it, none when it doesn't or scanning is off
func (s *Scope) scanWrite(ctx context.Context, dir string) (string, error) {
	scanner, ok := capability[Scanner](s.registry)
	if !s.Config.GetBool(skillsconfig.GuardAgentCreated) || !ok {
		return "", nil
	}
	verdict, err := scanner.Scan(ctx, s.Config, dir, "agent-created")
	if err != nil {
		return "", err
	}
	if verdict.Allowed {
		return "", nil
	}
	return fmt.Sprintf("Security scan blocked this skill (%s):\n%s", verdict.Reason, verdict.Report), nil
}

// Adds the index's cut of the description when it will be cut
func withPreview(result Result, content string) Result {
	fm, _ := Parse(content)
	if utf8.RuneCountInString(fm.Description()) > IndexDescription {
		result["system_prompt_preview"] = fmt.Sprintf("System prompt will show: \"%s\" — shorten the description to %d chars so the index shows all of it.", clip(fm.Description()), IndexDescription)
	}
	return result
}

// Adds the advisory findings of a SKILL.md, only the rules this write brought in when the text before is given
func withLint(result Result, skillMD string, before *string) {
	findings, err := LintFile(skillMD)
	if err != nil {
		return
	}
	if before != nil {
		standing := map[string]bool{}
		for _, f := range lintText(*before, filepath.Dir(skillMD)) {
			standing[f.Rule] = true
		}
		findings = slices.DeleteFunc(findings, func(f Finding) bool { return standing[f.Rule] })
	}
	if len(findings) > 0 {
		result["lint_warnings"] = findings
		result["lint_hint"] = lintHint
	}
}

func (s *Scope) create(ctx context.Context, name, content string, category any) (Result, error) {
	for _, problem := range []string{nameError(name), categoryError(category), frontmatterError(content), sizeError(content, "SKILL.md")} {
		if problem != "" {
			return failure(problem, nil), nil
		}
	}
	if dir, ok := s.Find(name); ok {
		return failure(fmt.Sprintf("A skill named '%s' already exists at %s.", name, dir), nil), nil
	}
	cat, _ := category.(string)
	dir := s.newDir(name, strings.TrimSpace(cat))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if entries, err := os.ReadDir(dir); redirect(dir) || !fsx.IsDir(dir) || err != nil || len(entries) > 0 {
			return failure(fmt.Sprintf("Cannot create skill '%s': %s already exists (not an empty directory, or unreadable). Choose another name, or move/remove that path and retry.", name, dir), nil), nil
		}
	}
	skillMD := filepath.Join(dir, "SKILL.md")
	refused, err := s.guardedWrite(ctx, name, dir, skillMD, "create", "SKILL.md", content)
	if err != nil || refused != nil {
		os.Remove(dir)
		return refused, err
	}
	shown := dir
	if rel, err := filepath.Rel(s.Dir(), dir); err == nil && filepath.IsLocal(rel) {
		shown = rel
	}
	result := Result{
		"success":  true,
		"message":  fmt.Sprintf("Skill '%s' created.", name),
		"path":     shown,
		"skill_md": skillMD,
		"_change":  map[string]any{"description": preview(content)},
		"hint":     fmt.Sprintf(createHow, name),
	}
	if config.Truthy(category) {
		result["category"] = category
	}
	withLint(withPreview(result, content), skillMD, nil)
	return result, nil
}

func (s *Scope) edit(ctx context.Context, name, content string) (Result, error) {
	for _, problem := range []string{frontmatterError(content), sizeError(content, "SKILL.md")} {
		if problem != "" {
			return failure(problem, nil), nil
		}
	}
	dir, refused := s.locateForWrite(ctx, name, "edit", "")
	if refused != nil {
		return refused, nil
	}
	refused, err := s.guardedWrite(ctx, name, dir, filepath.Join(dir, "SKILL.md"), "edit", "SKILL.md", content)
	if err != nil || refused != nil {
		return refused, err
	}
	return withPreview(Result{
		"success": true,
		"message": fmt.Sprintf("Skill '%s' updated (full rewrite).", name),
		"path":    dir,
		"_change": map[string]any{"description": preview(content)},
	}, content), nil
}

func (s *Scope) patch(ctx context.Context, op Op) (Result, error) {
	if !set(op.OldString) {
		return failure(needsOld, nil), nil
	}
	if op.NewString == nil {
		return failure(needsNew, nil), nil
	}
	dir, refused := s.locateForWrite(ctx, op.Name, "patch", "")
	if refused != nil {
		return refused, nil
	}
	file := strs.Deref(op.FilePath)
	label, target := "SKILL.md", filepath.Join(dir, "SKILL.md")
	if file != "" {
		label = file
		if target, refused = supporting(dir, file); refused != nil {
			return refused, nil
		}
	}
	if !fsx.Exists(target) {
		rel, err := filepath.Rel(dir, target)
		if err != nil {
			rel = target
		}
		return failure("File not found: "+rel, nil), nil
	}
	if refused := s.readGuard(ctx, op.Name, target, "patch", label); refused != nil {
		return refused, nil
	}
	content, err := readNormal(target)
	if err != nil {
		return nil, err
	}
	edit, err := fuzzy.Replace(content, *op.OldString, *op.NewString, op.ReplaceAll)
	if err != nil {
		return failure(err.Error()+fuzzy.NoMatchHint(err.Error(), 0, *op.OldString, content), Result{"file_preview": clipText(content, 500, "...")}), nil
	}
	if problem := sizeError(edit.Content, label); problem != "" {
		return failure(problem, nil), nil
	}
	if file == "" {
		if problem := frontmatterError(edit.Content); problem != "" {
			return failure("Patch would break SKILL.md structure: "+problem, nil), nil
		}
	}
	if refused, err := s.guardedWrite(ctx, op.Name, dir, target, "patch", label, edit.Content); err != nil || refused != nil {
		return refused, err
	}
	plural := ""
	if edit.Count > 1 {
		plural = "s"
	}
	result := Result{
		"success": true,
		"message": fmt.Sprintf("Patched %s in skill '%s' (%d replacement%s).", label, op.Name, edit.Count, plural),
		"_change": map[string]any{"old": clipText(*op.OldString, 200, "…"), "new": clipText(*op.NewString, 200, "…")},
	}
	if edit.Note != "" {
		result["note"] = edit.Note
	}
	if file == "" {
		withLint(result, target, &content)
	}
	return result, nil
}

// Skill folder holding the path, the profile's own when none does
func (s *Scope) rootOf(p string) string {
	resolved := resolve(p)
	for _, root := range s.Dirs() {
		if within(resolved, resolve(root)) {
			return root
		}
	}
	return s.Dir()
}

// Why a delete must not run: a link, a folder that can't be resolved, a skill folder itself, or one outside every skill folder
func (s *Scope) deleteError(dir string) string {
	if redirect(dir) {
		return fmt.Sprintf("Refusing to delete '%s': the skill directory is a symlink/junction. Remove the link target manually if intended.", dir)
	}
	if _, err := filepath.EvalSymlinks(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("Refusing to delete '%s': could not resolve path (%s).", dir, pathError(err))
	}
	resolved := resolve(dir)
	for _, root := range s.Dirs() {
		if resolved == resolve(root) {
			return fmt.Sprintf("Refusing to delete '%s': resolves to the skills root itself, which would remove every installed skill.", dir)
		}
		if within(resolved, resolve(root)) {
			return ""
		}
	}
	return fmt.Sprintf("Refusing to delete '%s': path does not resolve inside any known skills root.", dir)
}

// Why a skill may not be deleted: it is essential, or pinned
func (s *Scope) pinnedError(name string) string {
	if Essential[name] {
		return fmt.Sprintf("Skill '%s' is essential to Icebath (the agent's own operating manual referenced by the system prompt) and cannot be deleted. Patches and edits are still allowed.", name)
	}
	if record, err := s.Record(name); err == nil && record.Pinned {
		return fmt.Sprintf("Skill '%s' is pinned and cannot be deleted by skill_manage. Ask the user to run `icebath curator unpin %s` if they want to delete it. Patches and edits are allowed on pinned skills; only deletion is blocked.", name, name)
	}
	return ""
}

func (s *Scope) delete(ctx context.Context, name string, absorbed *string) (Result, error) {
	dir, refused := s.locateForWrite(ctx, name, "delete", "")
	if refused == nil {
		refused = s.consolidationGuard(ctx, name, absorbed)
	}
	if refused != nil {
		return refused, nil
	}
	if problem := s.pinnedError(name); problem != "" {
		return failure(problem, nil), nil
	}
	into := strings.TrimSpace(strs.Deref(absorbed))
	if into != "" {
		if into == name {
			return failure(fmt.Sprintf("absorbed_into='%s' cannot equal the skill being deleted.", into), nil), nil
		}
		if _, ok := s.Find(into); !ok {
			return failure(fmt.Sprintf("absorbed_into='%s' does not exist. Create or patch the umbrella skill first, then retry the delete.", into), nil), nil
		}
	}
	root := s.rootOf(dir)
	if problem := s.deleteError(dir); problem != "" {
		return failure(problem, nil), nil
	}
	note := ""
	if into != "" {
		note = fmt.Sprintf(" Content absorbed into '%s'.", into)
	}
	if reviewing(ctx) != nil {
		dest, err := s.Archive(ctx, name)
		if err != nil {
			return failure(fmt.Sprintf("failed to archive '%s': %s", name, err), nil), nil
		}
		return Result{"success": true, "message": fmt.Sprintf("Skill '%s' archived (archived to %s).%s", name, dest, note), "_archived": true}, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if parent := filepath.Dir(dir); parent != root {
		if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
			os.Remove(parent)
		}
	}
	return Result{"success": true, "message": fmt.Sprintf("Skill '%s' deleted.%s", name, note)}, nil
}

func (s *Scope) writeFile(ctx context.Context, name, file string, content *string) (Result, error) {
	if problem := filePathError(file); problem != "" {
		return failure(problem, nil), nil
	}
	if content == nil {
		return failure("file_content is required.", nil), nil
	}
	if n := len(*content); n > MaxFile {
		return failure(fmt.Sprintf("File content is %s bytes (limit: %s bytes / 1 MiB). Consider splitting into smaller files.", strs.Thousands(n), strs.Thousands(MaxFile)), nil), nil
	}
	if problem := sizeError(*content, file); problem != "" {
		return failure(problem, nil), nil
	}
	dir, refused := s.locateForWrite(ctx, name, "write_file", " Create it first with action='create'.")
	if refused != nil {
		return refused, nil
	}
	target, refused := supporting(dir, file)
	if refused != nil {
		return refused, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	if refused, err := s.guardedWrite(ctx, name, dir, target, "write_file", file, *content); err != nil || refused != nil {
		return refused, err
	}
	result := Result{"success": true, "message": fmt.Sprintf("File '%s' written to skill '%s'.", file, name), "path": target}
	if skillMD := filepath.Join(dir, "SKILL.md"); strings.HasPrefix(file, "references/") && fsx.IsFile(skillMD) {
		withLint(result, skillMD, nil)
	}
	return result, nil
}

func (s *Scope) removeFile(ctx context.Context, name, file string) (Result, error) {
	if problem := filePathError(file); problem != "" {
		return failure(problem, nil), nil
	}
	dir, refused := s.locateForWrite(ctx, name, "remove_file", "")
	if refused != nil {
		return refused, nil
	}
	target, refused := supporting(dir, file)
	if refused != nil {
		return refused, nil
	}
	if !fsx.Exists(target) {
		var available []string
		for _, sub := range []string{"references", "templates", "scripts", "assets"} {
			for _, rel := range files(filepath.Join(dir, sub)) {
				available = append(available, sub+"/"+rel)
			}
		}
		extra := Result{"available_files": nil}
		if len(available) > 0 {
			extra["available_files"] = available
		}
		return failure(fmt.Sprintf("File '%s' not found in skill '%s'.", file, name), extra), nil
	}
	if refused := s.readGuard(ctx, name, target, "remove_file", file); refused != nil {
		return refused, nil
	}
	if err := os.Remove(target); err != nil {
		return nil, err
	}
	if parent := filepath.Dir(target); parent != dir {
		if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
			os.Remove(parent)
		}
	}
	return Result{"success": true, "message": fmt.Sprintf("File '%s' removed from skill '%s'.", file, name)}, nil
}

// Lock file of one skill's changes
func (s *Scope) skillLock(name string) string {
	sum := sha256.Sum256([]byte(filepath.Base(name)))
	return filepath.Join(s.Dir(), ".locks", hex.EncodeToString(sum[:])+".lock")
}

// Holds the locks of every named skill, in path order
func (s *Scope) holdSkills(ctx context.Context, names []string) (context.Context, func(), error) {
	var paths []string
	for _, name := range names {
		if p := s.skillLock(name); !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var releases []func()
	release := func() {
		for _, r := range slices.Backward(releases) {
			r()
		}
	}
	for _, p := range paths {
		next, r, err := hold(ctx, p)
		if err != nil {
			release()
			return ctx, nil, err
		}
		ctx = next
		releases = append(releases, r)
	}
	return ctx, release, nil
}

// Runs one operation's handler
func (s *Scope) apply(ctx context.Context, op Op) (Result, error) {
	switch op.Action {
	case "create":
		return s.create(ctx, op.Name, strs.Deref(op.Content), op.Category)
	case "edit":
		return s.edit(ctx, op.Name, strs.Deref(op.Content))
	case "patch":
		if set(op.Content) && (set(op.OldString) || op.NewString != nil) {
			return failure(eitherOr, nil), nil
		}
		if set(op.Content) {
			return s.edit(ctx, op.Name, *op.Content)
		}
		return s.patch(ctx, op)
	case "delete":
		return s.delete(ctx, op.Name, op.AbsorbedInto)
	case "write_file":
		return s.writeFile(ctx, op.Name, strs.Deref(op.FilePath), op.FileContent)
	case "remove_file":
		return s.removeFile(ctx, op.Name, strs.Deref(op.FilePath))
	}
	return failure(fmt.Sprintf("Unknown action '%s'. Use: create, edit, patch, delete, write_file, remove_file", op.Action), nil), nil
}

// Logs a change that worked, forgets the cached listings and counts it in the skill's record
func (s *Scope) recorded(ctx context.Context, op Op, result Result, before []Item) {
	evidence := map[string]any{}
	archived, _ := result["_archived"].(bool)
	if op.Action == "delete" {
		evidence["absorbed_into"] = op.AbsorbedInto
		evidence["archived"] = archived
	}
	if s.Session != "" {
		evidence["session_id"] = s.Session
	}
	if set(op.FilePath) {
		evidence["file_path"] = *op.FilePath
	}
	after, _ := s.Find(op.Name)
	s.log(ctx, op.Action, op.Name, change{before: before, captured: true, after: after, evidence: evidence})
	s.registry.Forget()
	switch op.Action {
	case "create":
		s.Created(ctx, op.Name, reviewing(ctx) != nil)
	case "patch", "edit", "write_file", "remove_file":
		s.patched(ctx, op.Name, op.Action)
	case "delete":
		if !archived {
			s.drop(ctx, op.Name)
		}
	}
}

// Asks the user to allow a write when skills.write_approval is on
func (s *Scope) approve(ctx context.Context, gist string) Result {
	if !s.Config.GetBool(skillsconfig.WriteApproval) {
		return nil
	}
	if s.Approve == nil {
		return failure("Skill writes need the user's approval (skills.write_approval is on), and no one can be asked here. Not saved.", nil)
	}
	if err := s.Approve(ctx, gist, RiskWrite); err != nil {
		return failure("Skill write not approved: "+err.Error()+" Not saved.", nil)
	}
	return nil
}

var frontmatterDescription = regexp.MustCompile(`(?m)^description:\s*(.+)$`)

// One line saying what a write does, for the user to approve
func gist(op Op) string {
	content := strs.Deref(op.Content)
	switch {
	case (op.Action == "create" || op.Action == "edit") && content != "":
		description := ""
		if m := frontmatterDescription.FindStringSubmatch(content); m != nil {
			description = strs.Head(strings.Trim(strings.TrimSpace(m[1]), `'"`), 140)
		}
		n := utf8.RuneCountInString(content)
		size := fmt.Sprintf("%d chars", n)
		if n >= 1024 {
			size = fmt.Sprintf("%d KB", n/1024+1)
		}
		verb := "rewrite"
		if op.Action == "create" {
			verb = "create"
		}
		if description != "" {
			description = " — " + description
		}
		return fmt.Sprintf("%s skill '%s'%s (%s)", verb, op.Name, description, size)
	case op.Action == "patch":
		added, removed := 0, 0
		if set(op.OldString) {
			removed = strings.Count(*op.OldString, "\n") + 1
		}
		if set(op.NewString) {
			added = strings.Count(*op.NewString, "\n") + 1
		}
		target := strs.Deref(op.FilePath)
		if target == "" {
			target = "SKILL.md"
		}
		return fmt.Sprintf("patch skill '%s' %s (+%d/-%d lines)", op.Name, target, added, removed)
	case op.Action == "write_file":
		return fmt.Sprintf("write %s in skill '%s'", strs.Deref(op.FilePath), op.Name)
	case op.Action == "remove_file":
		return fmt.Sprintf("remove %s from skill '%s'", strs.Deref(op.FilePath), op.Name)
	case op.Action == "delete":
		return fmt.Sprintf("delete skill '%s'", op.Name)
	}
	return fmt.Sprintf("%s skill '%s'", op.Action, op.Name)
}

// Required arguments of each action, and what a missing one says
var required = map[string][]struct {
	arg     string
	null    bool
	message string
}{
	"create":      {{"content", false, "content is required for 'create'. Provide the full SKILL.md text (frontmatter + body)."}},
	"edit":        {{"content", false, "content is required for a full rewrite. Provide the full updated SKILL.md text."}},
	"write_file":  {{"file_path", false, "file_path is required for 'write_file'. Example: 'references/api-guide.md'"}, {"file_content", true, "file_content is required for 'write_file'."}},
	"remove_file": {{"file_path", false, "file_path is required for 'remove_file'."}},
}

// Text slots, and the action that reads each
var slots = [][2]string{{"content", "create (and a full-rewrite patch)"}, {"new_string", "a targeted patch (with old_string)"}, {"file_content", "write_file"}}

// Slots each action reads, and where misfiled text belongs
var reads = map[string]struct {
	slots       []string
	destination string
}{
	"create":     {[]string{"content"}, "'content'"},
	"edit":       {[]string{"content"}, "'content'"},
	"patch":      {[]string{"content", "new_string"}, "old_string/new_string (targeted) or 'content' (full rewrite, last resort)"},
	"write_file": {[]string{"file_content"}, "'file_content'"},
}

// Arguments of an operation by name, nil for those not given
func (op Op) arg(name string) *string {
	switch name {
	case "content":
		return op.Content
	case "file_path":
		return op.FilePath
	case "file_content":
		return op.FileContent
	case "old_string":
		return op.OldString
	case "new_string":
		return op.NewString
	}
	return nil
}

// Sentence naming the text an operation carries in slots its action never reads
func misplaced(op Op) string {
	read, ok := reads[op.Action]
	if !ok {
		return ""
	}
	var carried []string
	for _, slot := range slots {
		if !slices.Contains(read.slots, slot[0]) && op.arg(slot[0]) != nil {
			carried = append(carried, fmt.Sprintf("'%s' (that key is for %s)", slot[0], slot[1]))
		}
	}
	if len(carried) == 0 {
		return ""
	}
	return fmt.Sprintf(" Note: this op carries %s — move that text to %s.", strings.Join(carried, " and "), read.destination)
}

// Why an operation's arguments don't fit its action, empty when they do
func shapeError(op Op) string {
	for _, req := range required[op.Action] {
		v := op.arg(req.arg)
		if req.null && v == nil || !req.null && !set(v) {
			return req.message + misplaced(op)
		}
	}
	if op.Action == "patch" {
		content, old := set(op.Content), set(op.OldString)
		switch {
		case content && (old || op.NewString != nil):
			return eitherOr
		case !old && !content:
			return needsOld + misplaced(op)
		case !content && op.NewString == nil:
			return needsNew
		}
	}
	return ""
}

// Error result skill_manage gives for arguments of the wrong shape
func toolError(message string) Result {
	return Result{"success": false, "error": message}
}

// Runs one operation: the user's lock, the review's guards, approval, its locks, the change and its record
func (s *Scope) one(ctx context.Context, op Op, approved bool) (Result, error) {
	if refused := s.preflight(ctx, op.Action, op.Name); refused != nil {
		return refused, nil
	}
	if !approved {
		if refused := s.approve(ctx, gist(op)); refused != nil {
			return refused, nil
		}
	}
	if problem := shapeError(op); problem != "" {
		return toolError(problem), nil
	}
	checked := op.Name
	if op.Action != "create" && op.Name != "" {
		checked = filepath.Base(op.Name)
	}
	if problem := nameError(checked); problem != "" {
		return failure(problem, nil), nil
	}
	locked, release, err := s.holdSkills(ctx, []string{op.Name})
	if err != nil {
		return nil, err
	}
	defer release()
	dir, _ := s.Find(op.Name)
	before := s.capture(locked, dir, op.Action == "delete", op.Name)
	result, err := s.apply(locked, op)
	if err != nil {
		return nil, err
	}
	if result.OK() {
		s.recorded(locked, op, result, before)
	}
	return result, nil
}

// Runs one skill_manage operation given as the call itself
func (s *Scope) Manage(ctx context.Context, op Op) (Result, error) {
	return s.one(ctx, op, false)
}

// Runs skill_manage's operations: applied all or none, a delete only on its own
func (s *Scope) Batch(ctx context.Context, ops []Op) (Result, error) {
	if len(ops) == 0 {
		return toolError("operations must be a non-empty array."), nil
	}
	if len(ops) > maxOps {
		return toolError(fmt.Sprintf("operations is capped at %d ops per call.", maxOps)), nil
	}
	if i := slices.IndexFunc(ops, func(op Op) bool { return op.Action == "delete" }); i >= 0 {
		if len(ops) != 1 {
			return toolError("delete must be the SOLE op in its call — it doesn't compose with other ops' rollback."), nil
		}
		if ops[0].Name == "" {
			return toolError("operations[0] (delete) needs a 'name'."), nil
		}
		return s.one(ctx, Op{Action: "delete", Name: ops[0].Name, AbsorbedInto: ops[0].AbsorbedInto}, false)
	}
	if refused := s.validate(ctx, ops); refused != nil {
		return refused, nil
	}
	var names []string
	for _, op := range ops {
		if !slices.Contains(names, op.Name) {
			names = append(names, op.Name)
		}
	}
	actions := make([]string, len(ops))
	for i, op := range ops {
		actions[i] = op.Action
	}
	summary := gist(ops[0])
	if len(ops) > 1 {
		summary = fmt.Sprintf("batch(%d ops: %s) on %s", len(ops), strings.Join(actions, ", "), strings.Join(sortedList(names), ", "))
	}
	if refused := s.approve(ctx, summary); refused != nil {
		return refused, nil
	}
	locked, release, err := s.holdSkills(ctx, names)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.batch(locked, ops, names)
}

// Actions a list of operations may hold, delete only ever on its own
var batchActions = []string{"create", "patch", "remove_file", "write_file"}

// Why a list of operations can't run as a whole, checked before any of it does
func (s *Scope) validate(ctx context.Context, ops []Op) Result {
	fail := func(i int, message string) Result { return toolError(fmt.Sprintf("operations[%d]%s", i, message)) }
	var seen []string
	for i, op := range ops {
		switch {
		case op.Action == "":
			return fail(i, " needs an 'action'.")
		case !slices.Contains(batchActions, op.Action):
			return fail(i, fmt.Sprintf(": unknown action '%s'. Batchable: %s; delete must be sole.", op.Action, strings.Join(batchActions, ", ")))
		case op.Name == "":
			return fail(i, " needs a 'name' (the skill it targets).")
		}
		if problem := shapeError(op); problem != "" {
			return fail(i, fmt.Sprintf(" (%s on '%s'): %s", op.Action, op.Name, problem))
		}
		if op.Action == "create" {
			if problem := categoryError(op.Category); problem != "" {
				return fail(i, fmt.Sprintf(" (%s on '%s'): %s", op.Action, op.Name, problem))
			}
			if slices.Contains(seen, op.Name) {
				return fail(i, fmt.Sprintf(": create for '%s' must precede that skill's other ops.", op.Name))
			}
		}
		seen = append(seen, op.Name)
		if refused := s.preflight(ctx, op.Action, op.Name); refused != nil {
			return refused
		}
	}
	touched := map[[2]string]bool{}
	for i, op := range ops {
		rewrite := op.Action == "patch" && set(op.Content)
		target := "SKILL.md"
		if file := strings.TrimSpace(strs.Deref(op.FilePath)); op.Action != "create" && !rewrite && file != "" {
			target = path.Clean(strings.TrimLeft(file, "/"))
		}
		key := [2]string{op.Name, target}
		if (op.Action == "create" || op.Action == "write_file" || op.Action == "remove_file" || rewrite) && touched[key] {
			return fail(i, fmt.Sprintf(": %s on '%s' of skill '%s' — an earlier op in this batch already touched that file, and this op would silently discard its work. One destructive op (write_file/remove_file/full rewrite) per file per batch; put it first, or fold the change in. Patch chains are fine.", op.Action, target, op.Name))
		}
		touched[key] = true
	}
	return nil
}

// Copy of one skill a batch touches, from before it ran
type snapshot struct {
	name string
	// Where the skill was, none for one the batch creates
	dir string
	// Where its copy is, none when it had no folder
	copy string
	// Whether the folder a create writes to was already there, empty
	existed bool
}

// Applies the operations in order, putting every touched skill back as it was when one fails
func (s *Scope) batch(ctx context.Context, ops []Op, names []string) (Result, error) {
	root, err := os.MkdirTemp("", "skill_batch_")
	if err != nil {
		return nil, err
	}
	creates := map[string]string{}
	for _, op := range ops {
		if op.Action == "create" {
			category, _ := op.Category.(string)
			creates[op.Name] = s.newDir(op.Name, strings.TrimSpace(category))
		}
	}
	var snapshots []snapshot
	for _, name := range names {
		dir, found := s.Find(name)
		snap := snapshot{name: name, dir: dir}
		if found && fsx.IsDir(dir) {
			snap.copy = filepath.Join(root, name)
			if err := copyTree(os.DirFS(dir), ".", snap.copy, false, false); err != nil {
				os.RemoveAll(root)
				return toolError(fmt.Sprintf("Could not snapshot '%s' for atomic batch: %s", name, pathError(err))), nil
			}
		}
		target, creating := creates[name]
		snap.existed = !found && creating && fsx.IsDir(target)
		snapshots = append(snapshots, snap)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(root)
		}
	}()
	type applied struct {
		op     Op
		result Result
	}
	var done []applied
	for i, op := range ops {
		result, err := s.one(ctx, op, true)
		if err != nil {
			return nil, err
		}
		if !result.OK() {
			note, failed := s.rollback(snapshots, func(name string) []string {
				var out []string
				for _, a := range done {
					if a.op.Name == name && a.op.Action == "write_file" && set(a.op.FilePath) {
						out = append(out, path.Clean(strings.TrimLeft(*a.op.FilePath, "/")))
					}
				}
				return out
			})
			if failed {
				keep = true
			}
			out := Result{
				"success":                  false,
				"error":                    fmt.Sprintf("operations[%d] (%s on '%s') failed: %s — batch aborted, %s.", i, op.Action, op.Name, cmp.Or(result.Text("error"), "unknown error"), note),
				"failed_index":             i,
				"completed_before_failure": i,
			}
			for key, value := range result {
				if _, taken := out[key]; !taken && value != nil {
					out[key] = value
				}
			}
			return out, nil
		}
		done = append(done, applied{op, result})
	}
	rows := make([]Result, len(done))
	for i, a := range done {
		row := Result{"name": a.op.Name, "action": a.op.Action, "file_path": a.op.FilePath, "success": true}
		for _, key := range []string{"lint_warnings", "lint_hint"} {
			if v, ok := a.result[key]; ok && v != nil {
				row[key] = v
			}
		}
		rows[i] = row
	}
	return Result{"success": true, "operations_applied": len(done), "results": rows}, nil
}

// Puts every snapshot back: what was said, and whether any could not be
func (s *Scope) rollback(snapshots []snapshot, written func(name string) []string) (string, bool) {
	var notes []string
	for _, snap := range snapshots {
		after, _ := s.Find(snap.name)
		if err := restoreSnapshot(snap, after, written(snap.name)); err != nil {
			note := fmt.Sprintf("ROLLBACK FAILED for '%s' (%s)", snap.name, pathError(err))
			if snap.copy != "" {
				note += fmt.Sprintf("; snapshot preserved at '%s'", snap.copy)
			}
			notes = append(notes, note)
		}
	}
	s.registry.Forget()
	if len(notes) == 0 {
		return "all touched skills rolled back", false
	}
	return strings.Join(notes, "; "), true
}

// Puts one skill back as its snapshot had it: one the batch made is removed, files it wrote into a folder already there removed
func restoreSnapshot(snap snapshot, after string, written []string) error {
	present := after != "" && fsx.IsDir(after)
	if snap.copy == "" {
		if !present {
			return nil
		}
		if !snap.existed {
			return os.RemoveAll(after)
		}
		for _, rel := range append([]string{"SKILL.md"}, written...) {
			target := filepath.Join(after, filepath.FromSlash(rel))
			os.Remove(target)
			for parent := filepath.Dir(target); parent != after && within(parent, after); parent = filepath.Dir(parent) {
				os.Remove(parent)
			}
		}
		os.Remove(after)
		return nil
	}
	if !present {
		return copyTree(os.DirFS(snap.copy), ".", snap.dir, false, false)
	}
	aside := after + ".rollback-broken"
	os.RemoveAll(aside)
	if err := os.Rename(after, aside); err != nil {
		return err
	}
	if err := copyTree(os.DirFS(snap.copy), ".", snap.dir, false, false); err != nil {
		os.RemoveAll(snap.dir)
		os.Rename(aside, snap.dir)
		return err
	}
	return os.RemoveAll(aside)
}

// A value a skill declares it reads from skills.config, with its default
type ConfigVar struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Default     any    `json:"default,omitempty"`
	Prompt      string `json:"prompt"`
	// Skill that declared it
	Skill string `json:"skill,omitempty"`
}

// Values a skill declares it reads, each once, those without a key or description left out
func configVars(fm Frontmatter) []ConfigVar {
	var out []ConfigVar
	for _, item := range entries(fm.Meta()["config"]) {
		m := mapping(item)
		key, description := clean(m["key"]), clean(m["description"])
		if key == "" || description == "" || slices.ContainsFunc(out, func(v ConfigVar) bool { return v.Key == key }) {
			continue
		}
		v := ConfigVar{Key: key, Description: description, Default: m["default"], Prompt: description}
		if prompt := clean(m["prompt"]); prompt != "" {
			v.Prompt = prompt
		}
		out = append(out, v)
	}
	return out
}

// Values every skill of the profile's folders declares, each once, named by the first skill declaring it; disabled skills and those
// for other platforms left out
func (s *Scope) declaredConfig() []ConfigVar {
	var out []ConfigVar
	for _, e := range s.declaring() {
		for _, v := range configVars(e.Frontmatter) {
			if !slices.ContainsFunc(out, func(o ConfigVar) bool { return o.Key == v.Key }) {
				v.Skill = e.Name
				out = append(out, v)
			}
		}
	}
	return out
}

// Skills of the profile's folders whose declarations count: enabled, for this platform, not a project's
func (s *Scope) declaring() []Entry {
	disabled := s.Disabled()
	return slices.DeleteFunc(s.catalog(true, true), func(e Entry) bool {
		return e.Tier == TierProject || disabled[e.Name] || !platformMatches(e.Frontmatter["platforms"])
	})
}

// Key of the setting a declared value is kept under
func configKey(key string) string {
	return skillsconfig.Values + "." + strings.ToLower(key)
}

var homeVariable = regexp.MustCompile(`\$(?:\{HOME\}|HOME)`)

// Current value of each declared value: what the profile sets, else the declared default; ~ and $HOME taken as the home commands see
func (s *Scope) resolved(vars []ConfigVar) map[string]any {
	out := map[string]any{}
	for _, v := range vars {
		value := s.Config.Get(configKey(v.Key))
		if text, isText := value.(string); value == nil || isText && strings.TrimSpace(text) == "" {
			value = v.Default
			if value == nil {
				value = ""
			}
		}
		if text, isText := value.(string); isText && strings.ContainsAny(text, "~$") {
			value = s.expandHome(text)
		}
		out[v.Key] = value
	}
	return out
}

// Expands ~ and $HOME to the home folder commands see, then other variables
func (s *Scope) expandHome(value string) string {
	if home := s.environment()["HOME"]; home != "" {
		if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
			value = home + value[1:]
		}
		var b strings.Builder
		last := 0
		for _, m := range homeVariable.FindAllStringIndex(value, -1) {
			if m[1] < len(value) && value[m[1]] != '/' && value[m[1]] != '\\' {
				continue
			}
			b.WriteString(value[last:m[0]] + home)
			last = m[1]
		}
		value = b.String() + value[last:]
	}
	return fsx.ExpandHome(s.expandVars(value))
}

// Settings the user sets declared values by, one per value the profile's skills declare
func (s *Scope) ConfigSettings() []config.Setting {
	var out []config.Setting
	for _, v := range s.declaredConfig() {
		var fallback any = ""
		if v.Default != nil {
			fallback = v.Default
		}
		out = append(out, config.Setting{Key: configKey(v.Key), Title: v.Key, About: v.Prompt + " (skill " + v.Skill + ")", Default: fallback})
	}
	return out
}
