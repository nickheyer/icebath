package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Content and file budgets of skill_manage writes
const (
	MaxSkillContentChars = 100_000
	MaxSkillFileBytes    = 1_048_576
	skillNameRule        = "Use lowercase letters, numbers, hyphens, dots, and underscores."
)

var (
	validSkillNameRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\n?$`)
	manageFrontmatterEndRe = regexp.MustCompile(`\n---[` + pySpaceClass + `]*\n`)
	allowedSkillSubdirs    = []string{"assets", "references", "scripts", "templates"}
)

type skillGateBypassKey struct{}

// Reports whether the context replays an approved staged write past the gate
func gateBypassed(ctx context.Context) bool {
	v, _ := ctx.Value(skillGateBypassKey{}).(bool)
	return v
}

// Error text when the identifier is too long or not filesystem safe
func checkIdentifier(value, label, invalid string) string {
	if utf8.RuneCountInString(value) > MaxNameLength {
		return fmt.Sprintf("%s exceeds %d characters.", label, MaxNameLength)
	}
	if validSkillNameRe.MatchString(value) {
		return ""
	}
	return invalid
}

// Error text for a missing or malformed skill name
func validateSkillName(name string) string {
	if name == "" {
		return "Skill name is required."
	}
	return checkIdentifier(name, "Skill name", fmt.Sprintf("Invalid skill name '%s'. %s Must start with a letter or digit.", name, skillNameRule))
}

// Error text for a category that is not a single safe directory name
func validateSkillCategory(category any) string {
	if category == nil {
		return ""
	}
	str, ok := category.(string)
	if !ok {
		return "Category must be a string."
	}
	str = pystr.Strip(str)
	if str == "" {
		return ""
	}
	invalid := fmt.Sprintf("Invalid category '%s'. %s Categories must be a single directory name.", str, skillNameRule)
	if strings.ContainsAny(str, "/\\") {
		return invalid
	}
	return checkIdentifier(str, "Category", invalid)
}

// Error text when frontmatter lacks name or description or the body is empty
func validateSkillFrontmatter(content string) string {
	if pystr.Strip(content) == "" {
		return "Content cannot be empty."
	}
	for strings.HasPrefix(content, "\xEF\xBB\xBF") {
		content = strings.TrimPrefix(content, "\xEF\xBB\xBF")
	}
	if !strings.HasPrefix(content, "---") {
		return "SKILL.md must start with YAML frontmatter (---). See existing skills for format."
	}
	loc := manageFrontmatterEndRe.FindStringIndex(content[3:])
	if loc == nil {
		return "SKILL.md frontmatter is not closed. Ensure you have a closing '---' line."
	}
	parsed, err := yamlLoad(content[3 : loc[0]+3])
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
	if utf8.RuneCountInString(pyStrValue(fm["description"])) > MaxDescriptionLength {
		return fmt.Sprintf("Description exceeds %d characters.", MaxDescriptionLength)
	}
	if pystr.Strip(content[loc[1]+3:]) == "" {
		return "SKILL.md must have content after the frontmatter (instructions, procedures, etc.)."
	}
	return ""
}

// Normalized frontmatter description, false when absent
func descriptionOf(content string) (string, bool) {
	fm, _ := ParseFrontmatter(content)
	v, ok := fm["description"]
	if !ok {
		return "", false
	}
	return strings.Trim(pystr.Strip(pyStrValue(v)), "'\""), true
}

// Error text when this write sets a description wider than the skill index
func descriptionBudgetError(content string, before *string) string {
	desc, _ := descriptionOf(content)
	n := utf8.RuneCountInString(desc)
	if n <= SkillPromptDescLimit {
		return ""
	}
	if before != nil {
		prev, ok := descriptionOf(*before)
		if ok && prev == desc {
			return ""
		}
	}
	return fmt.Sprintf("Description is %d chars; a skill description must fit the %d-char skill index (one sentence, trigger first, ends with a period, without repeating the category name). The index clips anything longer. Move detail into the skill body.", n, SkillPromptDescLimit)
}

// Error text when content exceeds the SKILL.md character budget
func validateContentSize(content, label string) string {
	n := utf8.RuneCountInString(content)
	if n <= MaxSkillContentChars {
		return ""
	}
	return fmt.Sprintf("%s content is %s characters (limit: %s). Consider splitting into a smaller SKILL.md with supporting files in references/ or templates/.", label, commaInt(n), commaInt(MaxSkillContentChars))
}

// First 120 characters of the frontmatter description, empty on any failure
func descriptionPreview(content string) string {
	if len(content) < 3 {
		return ""
	}
	loc := manageFrontmatterEndRe.FindStringIndex(content[3:])
	if loc == nil {
		return ""
	}
	parsed, err := yamlLoad(content[3 : loc[0]+3])
	if err != nil {
		return ""
	}
	fm, ok := parsed.(map[string]any)
	if !ok {
		return ""
	}
	v, ok := fm["description"]
	if !ok {
		return ""
	}
	runes := []rune(pyStrValue(v))
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return string(runes)
}

// Directory a new skill lands in, honoring skills.create_dir
func (s *Scope) ResolveSkillDir(name, category string) string {
	base := s.SkillCreateDir()
	if base == "" {
		base = s.SkillsDir()
	}
	return filepath.Join(base, category, name)
}

// Skill directories under root holding a SKILL.md, excluded paths skipped and links not followed
func iterSkillDirs(root string) []string {
	var out []string
	_ = filepath.WalkDir(root+string(filepath.Separator), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "SKILL.md" || isExcludedSkillPath(p, "") {
			return nil
		}
		out = append(out, filepath.Dir(p))
		return nil
	})
	return out
}

// Frontmatter name of a SKILL.md clipped to the name budget, empty when unreadable or not a string
func readFrontmatterName(skillMD string) string {
	text, err := readSkillText(skillMD)
	if err != nil {
		return ""
	}
	if r := []rune(text); len(r) > 4000 {
		text = string(r[:4000])
	}
	fm, _ := ParseFrontmatter(text)
	name, ok := fm["name"].(string)
	if !ok {
		return ""
	}
	if r := []rune(name); len(r) > MaxNameLength {
		name = string(r[:MaxNameLength])
	}
	return name
}

// Skill directory by directory name, category path or unique frontmatter name
func (s *Scope) FindSkill(name string) (string, bool) {
	localRoot := ""
	if strings.ContainsAny(name, "/\\") {
		localRoot = resolvePath(s.SkillsDir())
	}
	var displayMatches []string
	seen := map[string]bool{}
	for _, skillsDir := range s.AllSkillsDirs() {
		if _, err := os.Stat(skillsDir); err != nil {
			continue
		}
		for _, dir := range iterSkillDirs(skillsDir) {
			if filepath.Base(dir) == name {
				return dir, true
			}
			if localRoot != "" {
				resolved := resolvePath(dir)
				if isRelativeTo(resolved, localRoot) {
					if rel, err := filepath.Rel(localRoot, resolved); err == nil && filepath.ToSlash(rel) == name {
						return dir, true
					}
				}
			}
			skillMD := filepath.Join(dir, "SKILL.md")
			if readFrontmatterName(skillMD) == name {
				key := resolvePath(skillMD)
				if !seen[key] {
					seen[key] = true
					displayMatches = append(displayMatches, dir)
				}
			}
		}
	}
	if len(displayMatches) == 1 {
		return displayMatches[0], true
	}
	if len(displayMatches) > 0 {
		s.logger().Warn(fmt.Sprintf("Skill display name '%s' is ambiguous (%d skills claim it: %s) — refusing to guess; resolve it by directory name instead", name, len(displayMatches), strings.Join(displayMatches, "; ")))
	}
	return "", false
}

// Profile and directory pairs of other profiles holding the skill
func (s *Scope) findSkillInOtherProfiles(name string) [][2]string {
	root := home.RootOf(s.Home)
	active := s.SkillsDir()
	activeDir := active
	if _, err := os.Stat(active); err == nil {
		activeDir = resolvePath(active)
	}
	var candidates [][2]string
	if resolvePath(filepath.Join(root, "skills")) != activeDir {
		candidates = append(candidates, [2]string{"default", filepath.Join(root, "skills")})
	}
	profilesDir := filepath.Join(root, home.ProfilesDir)
	if isDir(profilesDir) {
		entries, _ := os.ReadDir(profilesDir)
		for _, e := range entries {
			entry := filepath.Join(profilesDir, e.Name())
			if !isDir(entry) || resolvePath(filepath.Join(entry, "skills")) == activeDir {
				continue
			}
			candidates = append(candidates, [2]string{e.Name(), filepath.Join(entry, "skills")})
		}
	}
	var matches [][2]string
	for _, c := range candidates {
		if !isDir(c[1]) {
			continue
		}
		for _, d := range iterSkillDirs(c[1]) {
			if filepath.Base(d) == name {
				matches = append(matches, [2]string{c[0], d})
				break
			}
		}
	}
	return matches
}

// Active profile name of the scope home, default for the root home or on failure
func (s *Scope) activeProfileName() string {
	rel, err := filepath.Rel(filepath.Join(resolvePath(home.RootOf(s.Home)), home.ProfilesDir), resolvePath(s.Home))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "default"
	}
	return strings.Split(filepath.ToSlash(rel), "/")[0]
}

// Not found error naming other profiles that hold the skill, plus the suffix
func (s *Scope) skillNotFoundError(name, suffix string) string {
	base := fmt.Sprintf("Skill '%s' not found in active profile '%s'.", name, s.activeProfileName())
	others := s.findSkillInOtherProfiles(name)
	switch {
	case len(others) == 1:
		base += fmt.Sprintf(" A skill by that name exists in profile '%s' (%s). To edit it, switch profiles (`icebath -p %s`) or edit the file directly (file tools / terminal).", others[0][0], others[0][1], others[0][0])
	case len(others) > 1:
		names := make([]string, len(others))
		for i, o := range others {
			names[i] = "'" + o[0] + "'"
		}
		base += fmt.Sprintf(" Skills by that name exist in other profiles: %s. Switch profiles (`icebath -p <name>`) to edit there, or edit the files directly (file tools / terminal).", strings.Join(names, ", "))
	default:
		base += " Use skills_list() to see available skills."
	}
	return base + suffix
}

// Path components the way pathlib splits a relative or absolute path
func pathParts(p string) []string {
	var parts []string
	if strings.HasPrefix(p, "/") || (filepath.Separator == '\\' && strings.HasPrefix(p, "\\")) {
		parts = append(parts, string(filepath.Separator))
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// Error text for a supporting file path outside the allowed subdirectories
func validateSkillFilePath(filePath string) string {
	if filePath == "" {
		return "file_path is required."
	}
	parts := pathParts(filePath)
	if tools.HasTraversalComponent(filePath) {
		return "Path traversal ('..') is not allowed."
	}
	if len(parts) > 0 && parts[len(parts)-1] == "SKILL.md" && (len(parts) == 1 || len(parts) == 2) {
		return ""
	}
	allowed := false
	if len(parts) > 0 {
		for _, d := range allowedSkillSubdirs {
			allowed = allowed || parts[0] == d
		}
	}
	if !allowed {
		return fmt.Sprintf("File must be under one of: %s. Got: '%s'", strings.Join(allowedSkillSubdirs, ", "), filePath)
	}
	if len(parts) < 2 {
		return fmt.Sprintf("Provide a file path, not just a directory. Example: '%s/myfile.md'", parts[0])
	}
	return ""
}

// Validated supporting file target inside the skill dir, else the refusal
func resolveSupportingFile(skillDir, filePath string) (string, *pyjson.Object) {
	target := filepath.Join(skillDir, filePath)
	if filepath.IsAbs(filePath) {
		target = filePath
	}
	err := validateSkillFilePath(filePath)
	if err == "" {
		err = tools.ValidateWithinDir(target, skillDir)
	}
	if err != "" {
		return "", refusal(err)
	}
	return target, nil
}

// Skill directory for a write, running the review delete guard for deletes
func (s *Scope) locateForWrite(name, action, notFoundSuffix string) (string, *pyjson.Object) {
	dir, ok := s.FindSkill(name)
	if !ok {
		return "", refusal(s.skillNotFoundError(name, notFoundSuffix))
	}
	if action == "delete" {
		if guard := s.backgroundReviewDeleteGuard(name, dir); guard != nil {
			return "", guard
		}
	}
	return dir, nil
}

// Text of a file read in text mode as utf-8-sig with universal newlines
func readTextUniversal(path string) (string, error) {
	text, err := readSkillMDStrict(path)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), nil
}

// Atomic write keeping an existing file's mode and owner, the create mode for a new file when given
func writePreservingMode(ctx context.Context, target, content string, createMode *os.FileMode) error {
	return pyfs.AtomicWriteTextWith(ctx, target, content, pyfs.TextWrite{PreserveMode: true, CreateMode: createMode})
}

// Read guard, description budget, atomic write and security scan, restoring the original when blocked
func (s *Scope) guardedWrite(ctx context.Context, name, skillDir, target, action, label, content string) (*pyjson.Object, error) {
	var original *string
	if _, err := os.Stat(target); err == nil {
		if guard := s.backgroundReviewReadBeforeWriteGuard(name, target, action, label); guard != nil {
			return guard, nil
		}
		text, err := readTextUniversal(target)
		if err != nil {
			return nil, err
		}
		original = &text
	}
	if target == filepath.Join(skillDir, "SKILL.md") {
		if msg := descriptionBudgetError(content, original); msg != "" {
			return refusal(msg), nil
		}
	}
	if _, err := home.Mkdir(home.WithOverride(ctx, s.Home), filepath.Dir(target)); err != nil {
		return nil, err
	}
	createMode := os.FileMode(0o644)
	if err := writePreservingMode(home.WithOverride(ctx, s.Home), target, content, &createMode); err != nil {
		return nil, err
	}
	scanErr := manageSteps.securityScan(s, skillDir)
	if scanErr == "" {
		return nil, nil
	}
	if original != nil {
		if err := writePreservingMode(home.WithOverride(ctx, s.Home), target, *original, nil); err != nil {
			return nil, err
		}
	} else if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return refusal(scanErr), nil
}

// Post write scan step, swapped by tests to force a blocking verdict
var manageSteps = struct {
	securityScan func(s *Scope, skillDir string) string
}{securityScan: (*Scope).securityScanSkill}

// Blocking message of the opt in post write scan, empty when allowed or disabled
func (s *Scope) securityScanSkill(skillDir string) string {
	if !s.guardAgentCreatedEnabled() {
		return ""
	}
	result := ScanSkill(skillDir, "agent-created")
	allowed, reason := ShouldAllowInstall(result, false)
	if allowed == nil {
		s.logger().Warn(fmt.Sprintf("Agent-created skill blocked (dangerous findings): %s", reason))
	}
	if allowed == nil || !*allowed {
		return fmt.Sprintf("Security scan blocked this skill (%s):\n%s", reason, FormatScanReport(result))
	}
	return ""
}

// Reports skills.guard_agent_created, off by default
func (s *Scope) guardAgentCreatedEnabled() bool {
	return isTruthyValue(s.skillsCfgGet("guard_agent_created"), false)
}

// Shared truthy coercion, None takes the default and strings match the truthy set
func isTruthyValue(v any, def bool) bool {
	if v == nil {
		return def
	}
	if str, ok := v.(string); ok {
		switch strings.ToLower(pystr.Strip(str)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(v)
}

// Adds the prompt preview when the description will be clipped in the index
func addDescriptionPromptPreview(result *pyjson.Object, content string) *pyjson.Object {
	fm, _ := ParseFrontmatter(content)
	if isSkillDescriptionTruncatedForPrompt(fm) {
		result.Set("system_prompt_preview", fmt.Sprintf("System prompt will show: \"%s\" — shorten the description to %d chars so the index shows all of it.", extractSkillDescription(fm), SkillPromptDescLimit))
	}
	return result
}

// Attaches advisory lint findings, only the rules this write introduced when before is given
func attachLintFindings(result *pyjson.Object, skillMD string, before *string) {
	findings, err := LintSkill(skillMD)
	if err != nil {
		return
	}
	if before != nil {
		standing := map[string]bool{}
		for _, f := range LintContent(*before, filepath.Dir(skillMD)) {
			standing[f.Rule] = true
		}
		kept := findings[:0]
		for _, f := range findings {
			if !standing[f.Rule] {
				kept = append(kept, f)
			}
		}
		findings = kept
	}
	if len(findings) == 0 {
		return
	}
	rows := make([]any, 0, len(findings))
	for _, f := range findings {
		rows = append(rows, resultObj("severity", f.Severity, "rule", f.Rule, "message", f.Message))
	}
	result.Set("lint_warnings", rows)
	result.Set("lint_hint", "The write succeeded. These are advisory authoring-convention findings (not blockers) — fix them with skill_manage(action='patch') to match Icebath skill standards.")
}

// First n characters with the ellipsis appended when text was longer
func clipText(text string, n int, ellipsis string) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n]) + ellipsis
}

// Creates a new skill directory with its SKILL.md
func (s *Scope) createSkill(ctx context.Context, name, content string, category any) (*pyjson.Object, error) {
	if msg := firstNonEmpty(validateSkillName(name), validateSkillCategory(category), validateSkillFrontmatter(content), validateContentSize(content, "SKILL.md")); msg != "" {
		return refusal(msg), nil
	}
	if dir, ok := s.FindSkill(name); ok {
		return refusal(fmt.Sprintf("A skill named '%s' already exists at %s.", name, dir)), nil
	}
	cat, _ := category.(string)
	skillDir := s.ResolveSkillDir(name, cat)
	if _, err := home.Mkdir(home.WithOverride(ctx, s.Home), filepath.Dir(skillDir)); err != nil {
		return nil, err
	}
	if err := os.Mkdir(skillDir, 0o777); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		usable := false
		if !isPathRedirect(skillDir) && isDir(skillDir) {
			if entries, rerr := os.ReadDir(skillDir); rerr == nil && len(entries) == 0 {
				usable = true
			}
		}
		if !usable {
			return refusal(fmt.Sprintf("Cannot create skill '%s': %s already exists (not an empty directory, or unreadable). Choose another name, or move/remove that path and retry.", name, skillDir)), nil
		}
	}
	skillMD := filepath.Join(skillDir, "SKILL.md")
	guard, err := s.guardedWrite(home.WithOverride(ctx, s.Home), name, skillDir, skillMD, "create", "SKILL.md", content)
	if err != nil {
		return nil, err
	}
	if guard != nil {
		os.Remove(skillDir)
		return guard, nil
	}
	root := s.SkillsDir()
	display := skillDir
	if isRelativeTo(skillDir, root) {
		if rel, err := filepath.Rel(root, skillDir); err == nil {
			display = rel
		}
	}
	result := resultObj("success", true, "message", fmt.Sprintf("Skill '%s' created.", name), "path", display,
		"skill_md", skillMD, "_change", resultObj("description", descriptionPreview(content)))
	if pyrepr.Truthy(category) {
		result.Set("category", category)
	}
	result.Set("hint", fmt.Sprintf("To add reference files, templates, or scripts, use skill_manage(action='write_file', name='%s', file_path='references/example.md', file_content='...')", name))
	attachLintFindings(addDescriptionPromptPreview(result, content), skillMD, nil)
	return result, nil
}

// First non empty message
func firstNonEmpty(msgs ...string) string {
	for _, m := range msgs {
		if m != "" {
			return m
		}
	}
	return ""
}

// Replaces the SKILL.md of an existing skill
func (s *Scope) editSkill(ctx context.Context, name, content string) (*pyjson.Object, error) {
	if msg := firstNonEmpty(validateSkillFrontmatter(content), validateContentSize(content, "SKILL.md")); msg != "" {
		return refusal(msg), nil
	}
	skillDir, guard := s.locateForWrite(name, "edit", "")
	if guard != nil {
		return guard, nil
	}
	guard, err := s.guardedWrite(home.WithOverride(ctx, s.Home), name, skillDir, filepath.Join(skillDir, "SKILL.md"), "edit", "SKILL.md", content)
	if err != nil || guard != nil {
		return guard, err
	}
	result := resultObj("success", true, "message", fmt.Sprintf("Skill '%s' updated (full rewrite).", name),
		"path", skillDir, "_change", resultObj("description", descriptionPreview(content)))
	return addDescriptionPromptPreview(result, content), nil
}

// Full SKILL.md rewrite of an existing skill as a plain result map
func EditSkill(ctx context.Context, env *tools.Env, name, content string) map[string]any {
	result, err := ScopeFromEnv(env).editSkill(tools.WithEnvHome(ctx, env), name, content)
	if err != nil {
		result = raisedObj(err)
	}
	return objToMap(result)
}

// Plain map of an ordered result object, nested objects included
func objToMap(o *pyjson.Object) map[string]any {
	out := make(map[string]any, o.Len())
	for _, k := range o.Keys {
		v := o.Vals[k]
		if nested, ok := v.(*pyjson.Object); ok {
			v = objToMap(nested)
		}
		out[k] = v
	}
	return out
}

// Targeted find and replace in SKILL.md or a supporting file
func (s *Scope) patchSkill(ctx context.Context, name string, oldString, newString, filePath *string, replaceAll bool) (*pyjson.Object, error) {
	if oldString == nil || *oldString == "" {
		return refusal(patchNeedsOldString), nil
	}
	if newString == nil {
		return refusal(patchNeedsNewString), nil
	}
	skillDir, guard := s.locateForWrite(name, "patch", "")
	if guard != nil {
		return guard, nil
	}
	fp := ""
	if filePath != nil {
		fp = *filePath
	}
	label := fp
	if label == "" {
		label = "SKILL.md"
	}
	target := filepath.Join(skillDir, "SKILL.md")
	if fp != "" {
		t, ref := resolveSupportingFile(skillDir, fp)
		if ref != nil {
			return ref, nil
		}
		target = t
	}
	if _, err := os.Stat(target); err != nil {
		rel, rerr := filepath.Rel(skillDir, target)
		if rerr != nil {
			rel = target
		}
		return refusal("File not found: " + rel), nil
	}
	if guard := s.backgroundReviewReadBeforeWriteGuard(name, target, "patch", label); guard != nil {
		return guard, nil
	}
	content, err := readTextUniversal(target)
	if err != nil {
		return nil, err
	}
	var notes []string
	newContent, count, _, matchErr := tools.FuzzyFindAndReplace(content, *oldString, *newString, replaceAll, &notes)
	if matchErr != "" {
		matchErr += tools.FormatNoMatchHint(matchErr, count, *oldString, content)
		out := refusal(matchErr)
		out.Set("file_preview", clipText(content, 500, "..."))
		return out, nil
	}
	if msg := validateContentSize(newContent, label); msg != "" {
		return refusal(msg), nil
	}
	if fp == "" {
		if msg := validateSkillFrontmatter(newContent); msg != "" {
			return refusal("Patch would break SKILL.md structure: " + msg), nil
		}
	}
	guard, err = s.guardedWrite(home.WithOverride(ctx, s.Home), name, skillDir, target, "patch", label, newContent)
	if err != nil || guard != nil {
		return guard, err
	}
	plural := ""
	if count > 1 {
		plural = "s"
	}
	result := resultObj("success", true,
		"message", fmt.Sprintf("Patched %s in skill '%s' (%d replacement%s).", label, name, count, plural),
		"_change", resultObj("old", clipText(*oldString, 200, "…"), "new", clipText(*newString, 200, "…")))
	if len(notes) > 0 {
		result.Set("note", strings.Join(notes, "\n"))
	}
	if fp == "" {
		attachLintFindings(result, target, &content)
	}
	return result, nil
}

// Deletes a skill, archiving instead inside the review fork
func (s *Scope) deleteSkill(ctx context.Context, name string, absorbedInto *string) (*pyjson.Object, error) {
	skillDir, guard := s.locateForWrite(name, "delete", "")
	if guard == nil {
		guard = s.curatorConsolidationDeleteGuard(name, absorbedInto)
	}
	if guard != nil {
		return guard, nil
	}
	if msg := s.pinnedGuard(name); msg != "" {
		return refusal(msg), nil
	}
	target := ""
	if absorbedInto != nil {
		target = pystr.Strip(*absorbedInto)
	}
	if target != "" {
		if target == name {
			return refusal(fmt.Sprintf("absorbed_into='%s' cannot equal the skill being deleted.", target)), nil
		}
		if _, ok := s.FindSkill(target); !ok {
			return refusal(fmt.Sprintf("absorbed_into='%s' does not exist. Create or patch the umbrella skill first, then retry the delete.", target)), nil
		}
	}
	skillsRoot := s.containingSkillsRoot(skillDir)
	if unsafe := s.validateDeleteTarget(skillDir); unsafe != "" {
		return refusal(unsafe), nil
	}
	note := ""
	if target != "" {
		note = fmt.Sprintf(" Content absorbed into '%s'.", target)
	}
	if IsBackgroundReview(s.Env) {
		ok, msg, err := s.ArchiveSkill(ctx, name)
		if err != nil {
			return refusal(fmt.Sprintf("failed to archive '%s': %s", name, err)), nil
		}
		if !ok {
			return refusal(msg), nil
		}
		return resultObj("success", true, "message", fmt.Sprintf("Skill '%s' archived (%s).%s", name, msg, note), "_archived", true), nil
	}
	if err := pyfs.RmTree(skillDir, nil); err != nil {
		return nil, err
	}
	if err := rmdirIfEmpty(filepath.Dir(skillDir), skillsRoot); err != nil {
		return nil, err
	}
	return resultObj("success", true, "message", fmt.Sprintf("Skill '%s' deleted.%s", name, note)), nil
}

// Removes parent when it is empty and not the stop dir
func rmdirIfEmpty(parent, stop string) error {
	if parent == stop {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) > 0 {
		return nil
	}
	return os.Remove(parent)
}

// Adds or overwrites a supporting file of a skill
func (s *Scope) writeFile(ctx context.Context, name string, filePath string, fileContent *string) (*pyjson.Object, error) {
	if msg := validateSkillFilePath(filePath); msg != "" {
		return refusal(msg), nil
	}
	if fileContent == nil {
		return refusal("file_content is required."), nil
	}
	if n := len(*fileContent); n > MaxSkillFileBytes {
		return refusal(fmt.Sprintf("File content is %s bytes (limit: %s bytes / 1 MiB). Consider splitting into smaller files.", commaInt(n), commaInt(MaxSkillFileBytes))), nil
	}
	if msg := validateContentSize(*fileContent, filePath); msg != "" {
		return refusal(msg), nil
	}
	skillDir, guard := s.locateForWrite(name, "write_file", " Create it first with action='create'.")
	if guard != nil {
		return guard, nil
	}
	target, ref := resolveSupportingFile(skillDir, filePath)
	if ref != nil {
		return ref, nil
	}
	guard, err := s.guardedWrite(home.WithOverride(ctx, s.Home), name, skillDir, target, "write_file", filePath, *fileContent)
	if err != nil || guard != nil {
		return guard, err
	}
	result := resultObj("success", true, "message", fmt.Sprintf("File '%s' written to skill '%s'.", filePath, name), "path", target)
	if strings.HasPrefix(filePath, "references/") && isFile(filepath.Join(skillDir, "SKILL.md")) {
		attachLintFindings(result, filepath.Join(skillDir, "SKILL.md"), nil)
	}
	return result, nil
}

// Removes a supporting file of a skill, listing what exists when the path misses
func (s *Scope) removeFile(name, filePath string) (*pyjson.Object, error) {
	if msg := validateSkillFilePath(filePath); msg != "" {
		return refusal(msg), nil
	}
	skillDir, guard := s.locateForWrite(name, "remove_file", "")
	if guard != nil {
		return guard, nil
	}
	target, ref := resolveSupportingFile(skillDir, filePath)
	if ref != nil {
		return ref, nil
	}
	if _, err := os.Stat(target); err != nil {
		var available []any
		for _, sub := range []string{"references", "templates", "scripts", "assets"} {
			dir := filepath.Join(skillDir, sub)
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			for _, rel := range rglobFiles(dir) {
				available = append(available, filepath.Join(sub, filepath.FromSlash(rel)))
			}
		}
		var listed any
		if len(available) > 0 {
			listed = available
		}
		return refusal(fmt.Sprintf("File '%s' not found in skill '%s'.", filePath, name), "available_files", listed), nil
	}
	if guard := s.backgroundReviewReadBeforeWriteGuard(name, target, "remove_file", filePath); guard != nil {
		return guard, nil
	}
	if err := os.Remove(target); err != nil {
		return nil, err
	}
	if err := rmdirIfEmpty(filepath.Dir(target), skillDir); err != nil {
		return nil, err
	}
	return resultObj("success", true, "message", fmt.Sprintf("File '%s' removed from skill '%s'.", filePath, name)), nil
}

// Shared write gate, nil to proceed else the blocked or staged result
func (s *Scope) runWriteGate(ctx context.Context, staging func() (map[string]any, string)) *pyjson.Object {
	decision := tools.CheckWriteApproval(ctx, s.Env, tools.WriteApprovalSkills, "", "")
	if decision.Allow {
		return nil
	}
	if decision.Blocked {
		return toolErrorObj(decision.Message)
	}
	payload, gist := staging()
	record := tools.StageWrite(home.WithOverride(ctx, s.Home), s.Env, tools.WriteApprovalSkills, payload, gist, tools.CurrentOrigin(s.Env))
	return resultObj("success", true, "staged", true, "pending_id", record["id"], "gist", gist, "message", decision.Message)
}

// Flat shape gate staging the full arguments for replay, bypassed during replay
func (s *Scope) applySkillWriteGate(ctx context.Context, a manageArgs) *pyjson.Object {
	if !skillActions[a.action] || gateBypassed(ctx) {
		return nil
	}
	return s.runWriteGate(ctx, func() (map[string]any, string) {
		payload := map[string]any{"action": a.action, "name": a.name, "replace_all": a.replaceAll}
		for _, kv := range []struct {
			key string
			val any
		}{{"content", a.content}, {"category", a.category}, {"file_path", a.filePath}, {"file_content", a.fileContent},
			{"old_string", a.oldString}, {"new_string", a.newString}, {"absorbed_into", a.absorbedInto}} {
			if p, ok := kv.val.(*string); ok {
				if p != nil {
					payload[kv.key] = *p
				}
			} else if kv.val != nil {
				payload[kv.key] = kv.val
			}
		}
		return payload, tools.SkillGist(a.action, a.name, deref(a.content), deref(a.filePath), deref(a.oldString), deref(a.newString))
	})
}

// Value of an optional string, empty when absent
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Actions skill_manage dispatches, edit kept as the legacy full rewrite alias
var skillActions = map[string]bool{"create": true, "edit": true, "patch": true, "delete": true, "write_file": true, "remove_file": true}

// Arguments of one skill_manage call in the flat shape
type manageArgs struct {
	action, name string
	content      *string
	category     any
	filePath     *string
	fileContent  *string
	oldString    *string
	newString    *string
	absorbedInto *string
	replaceAll   bool
	operations   any
	hasOps       bool
	taskID       string
	sessionID    string
}

// Optional string argument, a TypeError for any other non null value
func optStringArg(payload map[string]any, key string) (*string, error) {
	v, ok := payload[key]
	if !ok || v == nil {
		return nil, nil
	}
	str, ok := v.(string)
	if !ok {
		return nil, &manageTypeError{msg: fmt.Sprintf("%s must be a string, not %s", key, pyTypeName(v))}
	}
	return &str, nil
}

// TypeError a non string argument raises inside the Python handlers
type manageTypeError struct{ msg string }

func (e *manageTypeError) Error() string      { return e.msg }
func (e *manageTypeError) PyTypeName() string { return "TypeError" }

// Flat shape arguments of a payload, operations kept when present and not null
func parseManageArgs(payload map[string]any, taskID, sessionID string) (manageArgs, error) {
	a := manageArgs{taskID: taskID, sessionID: sessionID, category: payload["category"]}
	a.action, _ = payload["action"].(string)
	a.name, _ = payload["name"].(string)
	a.replaceAll = pyrepr.Truthy(payload["replace_all"])
	if ops, ok := payload["operations"]; ok && ops != nil {
		a.operations, a.hasOps = ops, true
	}
	var err error
	for _, f := range []struct {
		key string
		dst **string
	}{{"content", &a.content}, {"file_path", &a.filePath}, {"file_content", &a.fileContent},
		{"old_string", &a.oldString}, {"new_string", &a.newString}, {"absorbed_into", &a.absorbedInto}} {
		if *f.dst, err = optStringArg(payload, f.key); err != nil {
			return a, err
		}
	}
	return a, nil
}

// Truthiness of an optional string
func truthyStr(p *string) bool { return p != nil && *p != "" }

// Runs the handler of one action, tool error objects for argument shape problems
func (s *Scope) dispatchAction(ctx context.Context, a manageArgs) (*pyjson.Object, error) {
	switch a.action {
	case "create":
		return s.createSkill(home.WithOverride(ctx, s.Home), a.name, deref(a.content), a.category)
	case "edit":
		return s.editSkill(home.WithOverride(ctx, s.Home), a.name, deref(a.content))
	case "patch":
		if truthyStr(a.content) && (truthyStr(a.oldString) || a.newString != nil) {
			return toolErrorObj(patchEitherOr), nil
		}
		if truthyStr(a.content) {
			return s.editSkill(home.WithOverride(ctx, s.Home), a.name, *a.content)
		}
		return s.patchSkill(home.WithOverride(ctx, s.Home), a.name, a.oldString, a.newString, a.filePath, a.replaceAll)
	case "delete":
		return s.deleteSkill(ctx, a.name, a.absorbedInto)
	case "write_file":
		return s.writeFile(home.WithOverride(ctx, s.Home), a.name, deref(a.filePath), a.fileContent)
	case "remove_file":
		return s.removeFile(a.name, deref(a.filePath))
	}
	return refusal(fmt.Sprintf("Unknown action '%s'. Use: create, edit, patch, delete, write_file, remove_file", a.action)), nil
}

// Ledger row, prompt cache clear and curator telemetry after a successful mutation
func (s *Scope) recordSuccess(ctx context.Context, a manageArgs, result *pyjson.Object, before []map[string]string) {
	post, found := s.FindSkill(a.name)
	evidence := pyjson.NewObject()
	if a.action == "delete" {
		var absorbed any
		if a.absorbedInto != nil {
			absorbed = *a.absorbedInto
		}
		evidence.Set("absorbed_into", absorbed)
		evidence.Set("archived", objTruthy(result, "_archived"))
	}
	if a.sessionID != "" {
		evidence.Set("session_id", a.sessionID)
	}
	if truthyStr(a.filePath) {
		evidence.Set("file_path", *a.filePath)
	}
	if before == nil {
		before = []map[string]string{}
	}
	after := ""
	if found {
		after = post
	}
	s.LedgerRecordMutation(ctx, a.action, a.name, LedgerMutation{Before: before, BeforeCaptured: true, AfterRoot: after, Evidence: evidence})
	ClearSkillsSystemPromptCache(s.Home, true)
	switch a.action {
	case "create":
		s.RecordCreated(ctx, a.name, IsBackgroundReview(s.Env), a.taskID, a.sessionID)
	case "patch", "edit", "write_file", "remove_file":
		s.BumpPatch(ctx, a.name, a.action, a.taskID, a.sessionID)
	case "delete":
		if !objTruthy(result, "_archived") {
			s.Forget(ctx, a.name)
		}
	}
}

// Per skill lock path keyed by a digest of the basename under skills/.locks
func (s *Scope) skillLockPath(name string) string {
	sum := sha256.Sum256([]byte(filepath.Base(name)))
	return filepath.Join(s.SkillsDir(), ".locks", hex.EncodeToString(sum[:])+".lock")
}

// Acquires every named skill lock in sorted path order, the returned ctx re entering them
func (s *Scope) skillMutationLocks(ctx context.Context, names []string) (context.Context, func(), error) {
	set := map[string]bool{}
	for _, n := range names {
		set[s.skillLockPath(n)] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var releases []func()
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, p := range paths {
		next, release, err := SkillFileLock(ctx, p)
		if err != nil {
			releaseAll()
			return ctx, nil, err
		}
		ctx = next
		releases = append(releases, release)
	}
	return ctx, releaseAll, nil
}

// Dispatches one flat call or an operations batch to its result object
func (s *Scope) skillManage(ctx context.Context, a manageArgs) (*pyjson.Object, error) {
	if a.hasOps {
		return s.skillManageBatch(ctx, a.operations, a.name, a.taskID, a.sessionID)
	}
	if pre := s.backgroundReviewPreflight(a.action, a.name); pre != nil {
		return pre, nil
	}
	if gate := s.applySkillWriteGate(ctx, a); gate != nil {
		return gate, nil
	}
	if msg := opShapeError(a.action, flatArgsMap(a)); msg != "" {
		return toolErrorObj(msg), nil
	}
	checked := a.name
	if a.action != "create" && a.name != "" {
		checked = filepath.Base(a.name)
	}
	if msg := validateSkillName(checked); msg != "" {
		return refusal(msg), nil
	}
	lockedCtx, release, err := s.skillMutationLocks(ctx, []string{a.name})
	if err != nil {
		return nil, err
	}
	defer release()
	var before []map[string]string
	if dir, ok := s.FindSkill(a.name); ok {
		before = s.LedgerCaptureBefore(lockedCtx, dir, a.action == "delete", a.name)
	} else {
		before = s.LedgerCaptureBefore(lockedCtx, "", a.action == "delete", a.name)
	}
	result, err := s.dispatchAction(lockedCtx, a)
	if err != nil {
		return nil, err
	}
	if objTruthy(result, "success") {
		s.recordSuccess(lockedCtx, a, result, before)
	}
	return result, nil
}

// Flat arguments as the dict the shape checks read
func flatArgsMap(a manageArgs) map[string]any {
	out := map[string]any{"category": a.category, "replace_all": a.replaceAll}
	for k, p := range map[string]*string{"content": a.content, "file_path": a.filePath, "file_content": a.fileContent,
		"old_string": a.oldString, "new_string": a.newString, "absorbed_into": a.absorbedInto} {
		if p != nil {
			out[k] = *p
		} else {
			out[k] = nil
		}
	}
	return out
}

// Runs skill_manage over a payload in the flat or operations shape and returns the JSON result
func (s *Scope) SkillManage(ctx context.Context, payload map[string]any, taskID, sessionID string) string {
	a, err := parseManageArgs(payload, taskID, sessionID)
	if err != nil {
		return tools.JSON(raisedObj(err))
	}
	result, err := s.skillManage(ctx, a)
	if err != nil {
		result = raisedObj(err)
	}
	return tools.JSON(result)
}

// Replays an approved staged skill write past the approval gate
func ApplySkillPending(ctx context.Context, env *tools.Env, payload map[string]any) string {
	return ScopeFromEnv(env).SkillManage(context.WithValue(ctx, skillGateBypassKey{}, true), payload, "", "")
}

// Tool description of skill_manage
func skillManageDescription() string {
	return "Create, update or delete skills: your procedural memory for recurring tasks. The call is an operations array (a single edit is a list of one); it applies atomically — any failure rolls every touched skill back. Ops: create (full SKILL.md; lands in the profile's skills directory or configured skills.create_dir; must precede that skill's other ops), patch (targeted old_string/new_string fix — preferred; content alone REPLACES the whole file, read it via skill_view() first), write_file/remove_file (supporting files), delete (sole op only). Existing skills are modified wherever they live. " +
		fmt.Sprintf("Description: max %d chars, 'Use when <trigger>. <behavior>.', never the category name (the index shows it). Write lessons, not logs: imperative rule + why, no PR numbers/dates/incident narration, one rule per lesson, references/ named by topic (extend before adding). skill_view() shows format conventions.", SkillPromptDescLimit)
}

// One self contained per action op shape with additional properties refused
func opSchema(action string, props map[string]any, required ...string) map[string]any {
	properties := map[string]any{"name": map[string]any{"type": "string"}, "action": map[string]any{"type": "string", "enum": []any{action}}}
	for k, v := range props {
		properties[k] = v
	}
	req := []any{"name", "action"}
	for _, r := range required {
		req = append(req, r)
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": req}
}

// Schema of the skill_manage tool advertising only the operations array
func SkillManageSchema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	items := map[string]any{"anyOf": []any{
		opSchema("create", map[string]any{"content": str("Full SKILL.md text (YAML frontmatter + markdown body)."), "category": str("Optional category subdir (e.g. 'devops').")}, "content"),
		opSchema("patch", map[string]any{"old_string": str("Text to find (same matching semantics as the patch tool)."), "new_string": str("Replacement; empty string deletes the match."), "replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences (default false)."}, "file_path": str("Optional supporting file (write_file's shape); default SKILL.md.")}, "old_string", "new_string"),
		opSchema("patch", map[string]any{"content": str("Full SKILL.md rewrite (REPLACES the whole file; last resort).")}, "content"),
		opSchema("write_file", map[string]any{"file_path": str("Path RELATIVE to the skill's own directory, e.g. 'references/api.md' — no leading slash, never absolute; first segment references/, templates/, scripts/, or assets/."), "file_content": str("Full text of the supporting file.")}, "file_path", "file_content"),
		opSchema("remove_file", map[string]any{"file_path": str("Supporting file (write_file's shape).")}, "file_path"),
		opSchema("delete", map[string]any{"absorbed_into": str("Curator consolidation only: umbrella skill that absorbed this one (must exist).")}),
	}}
	return map[string]any{
		"name":        "skill_manage",
		"description": skillManageDescription(),
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"operations": map[string]any{
			"type": "array", "description": "Ordered ops; each names its target skill (lowercase, hyphens/underscores, max 64 chars). Each action is its own shape; another action's text slot is invalid.", "items": items}},
			"required": []any{"operations"}},
	}
}

// Runs skill_manage for the model with the call task and session ids
func skillManageHandler(ctx context.Context, call *tools.Call) string {
	return ScopeFromEnv(call.Env).SkillManage(ctx, call.Args, call.TaskID, call.SessionID)
}

// Skill lock queries tools reads for terminal and file guards
type skillLockSource struct{}

func (skillLockSource) LockedSkillNames(env *tools.Env) []string {
	names, err := ScopeFromEnv(env).LockedSkillNames()
	if err != nil {
		panic(err)
	}
	return names
}
func (skillLockSource) SkillsDir(env *tools.Env) string { return ScopeFromEnv(env).SkillsDir() }
func (skillLockSource) AllSkillsDirs(env *tools.Env) []string {
	return ScopeFromEnv(env).AllSkillsDirs()
}
func (skillLockSource) IsExcludedSkillPath(p, root string) bool { return isExcludedSkillPath(p, root) }
func (skillLockSource) FindSkillDirName(env *tools.Env, name string) string {
	if dir, ok := ScopeFromEnv(env).FindSkill(name); ok {
		return filepath.Base(dir)
	}
	return ""
}
func (skillLockSource) ExternalSkillsDirs(env *tools.Env) []string {
	return ScopeFromEnv(env).ExternalSkillsDirs()
}
func (skillLockSource) ProjectSkillsDirs(env *tools.Env) []string {
	return ScopeFromEnv(env).ProjectSkillsDirs()
}
func (skillLockSource) IsExcludedSkillDir(name string) bool { return ExcludedSkillDirs[name] }

func init() {
	tools.Register(tools.Def{Name: "skill_manage", Toolset: "skills", Schema: SkillManageSchema(), Handler: skillManageHandler, Emoji: "📝"})
	tools.FindSkillDir = func(env *tools.Env, name string) string {
		dir, _ := ScopeFromEnv(env).FindSkill(name)
		return dir
	}
	tools.ResolveSkillDir = func(env *tools.Env, name, category string) string {
		return ScopeFromEnv(env).ResolveSkillDir(name, category)
	}
	tools.SkillLocks = skillLockSource{}
}
