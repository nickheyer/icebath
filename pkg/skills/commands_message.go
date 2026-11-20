package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	skillDirNote = "Resolve any relative paths in this skill (e.g. `scripts/foo.js`, " +
		"`templates/config.yaml`) against that directory, then run them " +
		"with the terminal tool using the absolute path."
	supportingFilesEnd = "/scripts/foo.js`)."
	setupSkippedNote   = "Required environment setup was skipped. Continue loading the skill " +
		"and explain any reduced functionality if it matters."
)

var (
	stablePrefixMu   sync.RWMutex
	stablePrefixSink func(prefix string)
)

// Installs the prompt cache boundary registry, called by pkg/provider at init with RegisterStablePrefix
func RegisterStablePrefixSink(fn func(prefix string)) {
	stablePrefixMu.Lock()
	stablePrefixSink = fn
	stablePrefixMu.Unlock()
}

// Hands a stable scaffold prefix to the cache boundary registry
func (s *Scope) registerStablePrefix(prefix string) {
	stablePrefixMu.RLock()
	fn := stablePrefixSink
	stablePrefixMu.RUnlock()
	if fn == nil {
		s.logger().Warn("prompt cache stable prefix sink is not registered")
		return
	}
	fn(prefix)
}

// Setup note for a loaded skill, empty when nothing needs saying
func setupNote(loaded map[string]any) string {
	if pyrepr.Truthy(loaded["setup_skipped"]) {
		return setupSkippedNote
	}
	if v := loaded["gateway_setup_hint"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	if pyrepr.Truthy(loaded["setup_needed"]) {
		if v := loaded["setup_note"]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return ""
}

// POSIX form of a relative path the way pathlib normalizes it
func pathAsPosix(p string) string {
	p = filepath.ToSlash(p)
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if lead == "" && len(parts) == 0 {
		return "."
	}
	return lead + strings.Join(parts, "/")
}

// Skill relative support file paths from linked_files or a walk of the support dirs
func supportingFilesListing(loaded map[string]any, skillDir string) []string {
	var supporting []string
	if linked, ok := loaded["linked_files"].(map[string]any); ok {
		keys := linkedFileKeys(linked)
		for _, k := range keys {
			entries, ok := linked[k].([]any)
			if !ok {
				continue
			}
			for _, e := range entries {
				supporting = append(supporting, pathAsPosix(pyrepr.Str(e)))
			}
		}
	}
	if len(supporting) > 0 || skillDir == "" {
		return supporting
	}
	for _, sub := range []string{"references", "templates", "scripts", "assets"} {
		files := rglob(filepath.Join(skillDir, sub), "*")
		sort.Slice(files, func(i, j int) bool { return pathPartsLess(files[i], files[j]) })
		for _, f := range files {
			if isFile(f) && !isSymlink(f) {
				rel, err := filepath.Rel(skillDir, f)
				if err == nil {
					supporting = append(supporting, filepath.ToSlash(rel))
				}
			}
		}
	}
	return supporting
}

// Orders paths component by component the way pathlib sorts them
func pathPartsLess(a, b string) bool {
	pa := strings.Split(filepath.ToSlash(a), "/")
	pb := strings.Split(filepath.ToSlash(b), "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// Linked file categories in the order skill_view emits them, unknown categories after in sorted order
func linkedFileKeys(linked map[string]any) []string {
	var keys, rest []string
	for _, k := range supportDirOrder {
		if _, ok := linked[k]; ok {
			keys = append(keys, k)
		}
	}
	for k := range linked {
		if !slices.Contains(supportDirOrder, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// Reports whether a path is a symlink
func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// Formats a loaded skill into the activation message payload, registering its stable scaffold prefix
func (s *Scope) buildSkillMessage(ctx context.Context, loaded map[string]any, skillDir, activationNote, userInstruction, runtimeNote, sessionID string) string {
	content := s.PreprocessSkillContent(ctx, pyStrOr(firstTruthy(loaded["content"], "")), skillDir, sessionID, s.skillsCfg())
	parts := []string{activationNote, "", strings.TrimSpace(content)}
	if skillDir != "" {
		parts = append(parts, "", fmt.Sprintf("[Skill directory: %s]", skillDir), skillDirNote)
	}
	s.injectSkillConfig(loaded, &parts)
	if note := setupNote(loaded); note != "" {
		parts = append(parts, "", fmt.Sprintf("[Skill setup note: %s]", note))
	}
	if supporting := supportingFilesListing(loaded, skillDir); len(supporting) > 0 && skillDir != "" {
		target := ""
		if rel, ok := lexicalRelative(skillDir, s.SkillsDir()); ok {
			target = rel
		} else if n := loaded["name"]; pyrepr.Truthy(n) {
			target = pyrepr.Str(n)
		} else {
			target = filepath.Base(skillDir)
		}
		parts = append(parts, "", "[This skill has supporting files (paths relative to the skill directory above):]")
		for _, sf := range supporting {
			parts = append(parts, "- "+sf)
		}
		parts = append(parts, fmt.Sprintf("\nLoad any of these with skill_view(name=\"%s\", file_path=\"<path>\"), or run scripts directly by absolute path (e.g. `node %s%s", target, skillDir, supportingFilesEnd))
	}
	stable, hasStable := "", false
	if userInstruction != "" {
		parts = append(parts, "")
		stable, hasStable = AppendUserInstruction(&parts, userInstruction), true
	}
	if runtimeNote != "" {
		parts = append(parts, "", fmt.Sprintf("[Runtime note: %s]", runtimeNote))
	}
	message := strings.Join(parts, "\n")
	if hasStable && strings.HasPrefix(message, stable) && len(message) > len(stable) {
		s.registerStablePrefix(stable)
	}
	return message
}

// Lexical path of child below parent like PurePath.relative_to, false when child is not under parent
func lexicalRelative(child, parent string) (string, bool) {
	c, p := filepath.Clean(child), filepath.Clean(parent)
	if c == p {
		return ".", true
	}
	prefix := p
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if !strings.HasPrefix(c, prefix) {
		return "", false
	}
	return c[len(prefix):], true
}

// Loaded skill payload, its directory and its resolved name
type loadedSkill struct {
	payload map[string]any
	dir     string
	name    string
}

// Counts the use for curator tracking then builds the message block for one loaded skill
func (s *Scope) renderSkillBlock(ctx context.Context, loaded loadedSkill, activationNote, taskID, userInstruction, runtimeNote string) string {
	s.BumpUse(ctx, loaded.name, taskID, "")
	return s.buildSkillMessage(ctx, loaded.payload, loaded.dir, activationNote, userInstruction, runtimeNote, taskID)
}

// Header for bundle and stacked multi skill messages, the subject ending in skill bundle
func scaffoldHeader(subject string, loadedNames, leadLines, missing, disabled []string, extraInstruction, userInstruction string) string {
	lines := []string{
		fmt.Sprintf("[IMPORTANT: The user has invoked the %s, loading %d skills together. Treat every skill below as active guidance for this turn.]", subject, len(loadedNames)),
		"",
	}
	lines = append(lines, leadLines...)
	lines = append(lines, "Skills loaded: "+strings.Join(loadedNames, ", "))
	if len(missing) > 0 {
		lines = append(lines, "Skills missing (skipped): "+strings.Join(missing, ", "))
	}
	if len(disabled) > 0 {
		lines = append(lines, "Skills disabled for this platform (skipped): "+strings.Join(disabled, ", "))
	}
	if extraInstruction != "" {
		lines = append(lines, "", "Bundle instruction: "+extraInstruction)
	}
	if userInstruction != "" {
		lines = append(lines, "", "User instruction: "+userInstruction)
	}
	return strings.Join(lines, "\n")
}
