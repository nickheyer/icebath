package gatewayrun

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
)

var (
	skillSlugInvalidChars = regexp.MustCompile(`[^a-z0-9-]`)
	skillSlugMultiHyphen  = regexp.MustCompile(`-{2,}`)
)

// Slug and declared name of a SKILL.md from its frontmatter name, both empty when unreadable or unnamed
func skillSlugFromFrontmatter(skillMD string) (string, string) {
	raw, err := os.ReadFile(skillMD)
	if err != nil {
		return "", ""
	}
	content := strings.ToValidUTF8(string(raw), "�")
	content = strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(content, "---") {
		return "", ""
	}
	end := strings.Index(content[3:], "\n---")
	if end < 0 {
		return "", ""
	}
	declared := ""
	for _, line := range pystr.Splitlines(content[3:3+end], false) {
		line = pystr.Strip(line)
		if strings.HasPrefix(line, "name:") {
			value := pystr.Strip(strings.SplitN(line, ":", 2)[1])
			if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '"' || value[0] == '\'') {
				value = value[1 : len(value)-1]
			}
			declared = pystr.Strip(value)
			break
		}
	}
	if declared == "" {
		return "", ""
	}
	slug := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(declared), " ", "-"), "_", "-")
	slug = skillSlugInvalidChars.ReplaceAllString(slug, "")
	slug = strings.Trim(skillSlugMultiHyphen.ReplaceAllString(slug, "-"), "-")
	return slug, declared
}

// Every SKILL.md under a directory that the skill scanners do not skip
func skillManifests(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "SKILL.md" && !skills.IsExcludedSkillPath(path, "") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// Hint for a command that names a disabled skill or an optional skill not installed, empty otherwise
func checkUnavailableSkill(ctx context.Context, commandName string) string {
	normalized := strings.ReplaceAll(strings.ToLower(commandName), "_", "-")
	scope := skills.NewScope(home.Dir(ctx), nil)
	disabled := scope.DisabledSkillNames("")
	for _, dir := range scope.AllSkillsDirs() {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		for _, skillMD := range skillManifests(dir) {
			slug, declared := skillSlugFromFrontmatter(skillMD)
			if slug == "" || declared == "" {
				continue
			}
			if slug == normalized && disabled[declared] {
				return i18n.T(ctx, "gateway.skills.disabled", "", map[string]any{"name": commandName})
			}
		}
	}
	optionalDir := home.OptionalSkillsDir(ctx, filepath.Join(pm.RepoRoot(), "optional-skills"))
	if _, err := os.Stat(optionalDir); err != nil {
		return ""
	}
	for _, skillMD := range skillManifests(optionalDir) {
		slug, _ := skillSlugFromFrontmatter(skillMD)
		if slug == "" || slug != normalized {
			continue
		}
		rel, err := filepath.Rel(optionalDir, filepath.Dir(skillMD))
		if err != nil {
			return ""
		}
		installPath := "official/" + filepath.ToSlash(rel)
		return i18n.T(ctx, "gateway.skills.not_installed", "", map[string]any{"name": commandName, "install_name": installPath})
	}
	return ""
}
