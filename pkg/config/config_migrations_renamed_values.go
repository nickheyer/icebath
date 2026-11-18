package config

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Stored values an older icebath wrote under the name it carried before, each moved to the value icebath writes
var renamedStoredValues = []rewriteSpec{
	{section: "compression", key: "codex_app_server_auto", newValue: "icebath", match: lowerIs("hermes"),
		added:   "compression.codex_app_server_auto=icebath (renamed value)",
		message: "  ✓ compression.codex_app_server_auto now reads icebath, the current name of the mode it was set to"},
	{section: "attachments", key: "storage", newValue: "icebath-home", match: lowerIs("hermes-home"),
		added:   "attachments.storage=icebath-home (renamed value)",
		message: "  ✓ attachments.storage now reads icebath-home, the current name of the profile home storage it was set to"},
}

// Skill frontmatter metadata key skills written before the rename carry
const retiredSkillMetadataKey = "hermes"

// Toolset bundle prefix and bare platform alias configs written before the rename carry
const (
	retiredToolsetPrefix = "hermes-"
	retiredToolsetAlias  = "hermes"
)

// Rewrites every stored value, toolset name and skill metadata key of the older naming to the one icebath writes now
func migrateTo64(r *migrationRun) error {
	for _, spec := range renamedStoredValues {
		if err := rewriteKey(spec)(r); err != nil {
			return err
		}
	}
	if err := renameRetiredToolsetNames(r); err != nil {
		return err
	}
	return renameRetiredSkillMetadata(r)
}

// Renames the older toolset spellings in every toolset list of the config to their icebath names
func renameRetiredToolsetNames(r *migrationRun) error {
	cfg := r.read()
	var renamed []string
	renameList := func(where string, list []any, platformList bool) {
		for i, item := range list {
			name, ok := item.(string)
			if !ok {
				continue
			}
			switch {
			case platformList && name == retiredToolsetAlias:
				list[i] = "icebath"
			case strings.HasPrefix(name, retiredToolsetPrefix):
				list[i] = "icebath-" + strings.TrimPrefix(name, retiredToolsetPrefix)
			default:
				continue
			}
			renamed = append(renamed, where+": "+list[i].(string))
		}
	}
	if list, ok := cfg["toolsets"].([]any); ok {
		renameList("toolsets", list, false)
	}
	if agent, ok := cfg["agent"].(map[string]any); ok {
		if list, ok := agent["disabled_toolsets"].([]any); ok {
			renameList("agent.disabled_toolsets", list, false)
		}
	}
	for _, section := range []string{"platform_toolsets", "known_builtin_toolsets", "known_plugin_toolsets"} {
		platforms, ok := cfg[section].(map[string]any)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(platforms))
		for platform := range platforms {
			keys = append(keys, platform)
		}
		sort.Strings(keys)
		for _, platform := range keys {
			if list, ok := platforms[platform].([]any); ok {
				renameList(section+"."+platform, list, section == "platform_toolsets")
			}
		}
	}
	if len(renamed) == 0 {
		return nil
	}
	return r.commit(cfg, "toolset names renamed ("+strings.Join(renamed, ", ")+")",
		"  ✓ Toolset names now use their icebath spelling: "+strings.Join(renamed, ", "))
}

// Moves the older metadata key of every SKILL.md frontmatter in the home skill tree to metadata.icebath
func renameRetiredSkillMetadata(r *migrationRun) error {
	if !r.localFilesAllowed() {
		return nil
	}
	root := home.SkillsDir(r.h)
	var renamed []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || d.Name() != "SKILL.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out, changed := renameSkillMetadataKey(data)
		if !changed {
			return nil
		}
		if err := atomicPreservingWrite(context.Background(), path, out, ".SKILL.md."); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		renamed = append(renamed, filepath.ToSlash(rel))
		return nil
	})
	if err != nil || len(renamed) == 0 {
		return err
	}
	r.res.ConfigAdded = append(r.res.ConfigAdded, "skills metadata.icebath ("+strings.Join(renamed, ", ")+")")
	r.say("  ✓ Skill frontmatter now keeps its tags and settings under metadata.icebath: " + strings.Join(renamed, ", "))
	return nil
}

// Frontmatter text of a SKILL.md and the byte offset it starts at, false without a closed frontmatter
func skillFrontmatter(data []byte) ([]byte, int, bool) {
	start := 0
	if bytes.HasPrefix(data, []byte("\xEF\xBB\xBF")) {
		start = 3
	}
	rest := data[start:]
	open := 0
	switch {
	case bytes.HasPrefix(rest, []byte("---\r\n")):
		open = 5
	case bytes.HasPrefix(rest, []byte("---\n")):
		open = 4
	default:
		return nil, 0, false
	}
	body := rest[open:]
	for offset := 0; offset <= len(body); {
		end := bytes.IndexByte(body[offset:], '\n')
		line := body[offset:]
		if end >= 0 {
			line = body[offset : offset+end]
		}
		if strings.TrimRight(string(line), "\r") == "---" {
			return body[:offset], start + open, true
		}
		if end < 0 {
			break
		}
		offset += end + 1
	}
	return nil, 0, false
}

// SKILL.md bytes with metadata of the older key renamed in place, false when nothing carries it
func renameSkillMetadataKey(data []byte) ([]byte, bool) {
	front, base, ok := skillFrontmatter(data)
	if !ok {
		return data, false
	}
	var doc yaml.Node
	if yaml.Unmarshal(front, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return data, false
	}
	var metadata *yaml.Node
	top := doc.Content[0].Content
	for i := 0; i+1 < len(top); i += 2 {
		if top[i].Value == "metadata" && top[i+1].Kind == yaml.MappingNode {
			metadata = top[i+1]
		}
	}
	if metadata == nil {
		return data, false
	}
	var retired *yaml.Node
	for i := 0; i+1 < len(metadata.Content); i += 2 {
		switch metadata.Content[i].Value {
		case "icebath":
			return data, false
		case retiredSkillMetadataKey:
			retired = metadata.Content[i]
		}
	}
	if retired == nil {
		return data, false
	}
	lines := bytes.SplitAfter(front, []byte("\n"))
	if retired.Line < 1 || retired.Line > len(lines) {
		return data, false
	}
	offset := base
	for _, line := range lines[:retired.Line-1] {
		offset += len(line)
	}
	runes := []rune(string(lines[retired.Line-1]))
	if retired.Column < 1 || retired.Column > len(runes) {
		return data, false
	}
	offset += len(string(runes[:retired.Column-1]))
	if retired.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		offset++
	}
	if !bytes.HasPrefix(data[offset:], []byte(retiredSkillMetadataKey)) {
		return data, false
	}
	out := make([]byte, 0, len(data)+len("icebath")-len(retiredSkillMetadataKey))
	out = append(out, data[:offset]...)
	out = append(out, "icebath"...)
	return append(out, data[offset+len(retiredSkillMetadataKey):]...), true
}
