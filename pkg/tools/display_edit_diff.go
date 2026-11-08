package tools

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Skill directory a create action targets, the skills package installs it
var ResolveSkillDir func(env *Env, name, category string) string

// Inline diff caps and the ANSI reset every colored diff line ends with
const (
	displayANSIReset   = "\033[0m"
	maxInlineDiffFiles = 6
	maxInlineDiffLines = 80
)

// Red, green and blue of a #rrggbb color
func hexRGB(h string) [3]int {
	r, _ := strconv.ParseInt(h[1:3], 16, 0)
	g, _ := strconv.ParseInt(h[3:5], 16, 0)
	b, _ := strconv.ParseInt(h[5:7], 16, 0)
	return [3]int{int(r), int(g), int(b)}
}

// Active skin, nil when the skin engine is unavailable
func getSkin(ctx context.Context) (skin *config.SkinConfig) {
	defer func() {
		if recover() != nil {
			skin = nil
		}
	}()
	return config.GetActiveSkin(ctx)
}

// Foreground diff colors, key to skin color key and dark terminal fallback
var diffFG = map[string]struct {
	skinKey  string
	fallback [3]int
}{
	"dim":  {"banner_dim", [3]int{150, 150, 150}},
	"file": {"session_label", [3]int{180, 160, 255}},
	"hunk": {"session_border", [3]int{120, 120, 140}},
}

// Background diff colors, key to skin key, default hex, dominant channel and dark terminal fallback
var diffBG = map[string]struct {
	skinKey, defaultHex string
	dominant            int
	fallback            string
}{
	"minus": {"ui_error", "#ef5350", 0, "\033[38;2;255;255;255;48;2;120;20;20m"},
	"plus":  {"ui_ok", "#4caf50", 1, "\033[38;2;255;255;255;48;2;20;90;20m"},
}

var (
	diffColorsMu     sync.Mutex
	diffColorsCached map[string]string
)

// Truecolor foreground escape
func fgEscape(rgb [3]int) string { return fmt.Sprintf("\033[38;2;%d;%d;%dm", rgb[0], rgb[1], rgb[2]) }

// White text on a dark tint of the color, the dominant channel kept brighter
func tintedBG(rgb [3]int, dominant int) string {
	var out [3]int
	for i, v := range rgb {
		if i == dominant {
			out[i] = max(v/2, 20)
		} else {
			out[i] = max(v/4, 10)
		}
	}
	return fmt.Sprintf("\033[38;2;255;255;255;48;2;%d;%d;%dm", out[0], out[1], out[2])
}

// ANSI escapes for diff display resolved from the active skin once and cached
func diffANSI(ctx context.Context) map[string]string {
	diffColorsMu.Lock()
	defer diffColorsMu.Unlock()
	if diffColorsCached != nil {
		return diffColorsCached
	}
	colors := map[string]string{}
	for key, spec := range diffFG {
		colors[key] = fgEscape(spec.fallback)
	}
	for key, spec := range diffBG {
		colors[key] = spec.fallback
	}
	if skin := getSkin(ctx); skin != nil {
		for key, spec := range diffFG {
			h := skin.GetColor(spec.skinKey, "")
			rgb := spec.fallback
			if h != "" && pystr.Len(h) == 7 && h[0] == '#' {
				rgb = hexRGB(h)
			}
			colors[key] = fgEscape(rgb)
		}
		for key, spec := range diffBG {
			if h := skin.GetColor(spec.skinKey, spec.defaultHex); h != "" && pystr.Len(h) == 7 {
				colors[key] = tintedBG(hexRGB(h), spec.dominant)
			}
		}
	}
	diffColorsCached = colors
	return colors
}

// Pre tool filesystem snapshot used to render diffs locally after writes, nil content for an unreadable file
type LocalEditSnapshot struct {
	Paths  []string
	Before map[string]*string
}

// Filesystem path of a possibly relative tool path against the current directory
func resolvedDisplayPath(path string) string {
	expanded := pypath.Expanduser(path)
	if filepath.IsAbs(expanded) {
		return expanded
	}
	cwd, err := os.Getwd()
	if err != nil {
		return expanded
	}
	return filepath.Join(cwd, expanded)
}

// UTF-8 text of a file, false for a missing, unreadable or undecodable file
func snapshotText(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := strictUTF8Text(data)
	if text == "" && len(strings.TrimPrefix(string(data), "\xef\xbb\xbf")) != 0 {
		return "", false
	}
	return text, true
}

// Path relative to the current directory when it lies under it, else the path itself
func displayDiffPath(path string) string {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return path
	}
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	base, err := pypath.Resolve(cwd)
	if err != nil {
		return path
	}
	if rel, under := pypath.PathRelativeTo(resolved, base); under {
		return rel
	}
	return path
}

// Filesystem targets a skill_manage write action touches
func resolveSkillManagePaths(env *Env, args map[string]any) []string {
	action, _ := args["action"].(string)
	name, _ := args["name"].(string)
	if action == "" || name == "" {
		return nil
	}
	if action == "create" {
		if ResolveSkillDir == nil {
			panic("tools: ResolveSkillDir is nil, pkg/skills must install the skill directory resolver")
		}
		category, _ := args["category"].(string)
		return []string{filepath.Join(ResolveSkillDir(env, name, category), "SKILL.md")}
	}
	skillDir := findSkillPath(env, name)
	if skillDir == "" {
		return nil
	}
	filePath, _ := args["file_path"].(string)
	if action == "delete" {
		var files []string
		filepath.WalkDir(skillDir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
		return files
	}
	switch action {
	case "edit", "patch", "write_file", "remove_file":
		if filePath != "" {
			return []string{filepath.Join(skillDir, filePath)}
		}
	}
	if action == "edit" || action == "patch" {
		return []string{filepath.Join(skillDir, "SKILL.md")}
	}
	return nil
}

// Local filesystem targets of a write capable tool call, none for a remote task target
func resolveLocalEditPaths(env *Env, toolName string, args map[string]any, taskID *string) []string {
	if args == nil {
		return nil
	}
	if toolName == "skill_manage" {
		return resolveSkillManagePaths(env, args)
	}
	path := ""
	if toolName == "write_file" || toolName == "patch" {
		path, _ = args["path"].(string)
	}
	if path != "" && taskID != nil {
		if terminalEnvTypeForTask(env, *taskID) != "local" {
			return nil
		}
		resolved, err := resolvePathForTaskID(env, *taskID, path)
		if err != nil {
			return nil
		}
		return []string{resolved}
	}
	if path != "" {
		return []string{resolvedDisplayPath(path)}
	}
	return nil
}

// Before state of the files a local write preview will diff, nil when the call targets none
func CaptureLocalEditSnapshot(env *Env, toolName string, args map[string]any, taskID *string) *LocalEditSnapshot {
	paths := resolveLocalEditPaths(env, toolName, args, taskID)
	if len(paths) == 0 {
		return nil
	}
	before := map[string]*string{}
	for _, path := range paths {
		if text, ok := snapshotText(path); ok {
			before[path] = &text
		} else {
			before[path] = nil
		}
	}
	return &LocalEditSnapshot{Paths: paths, Before: before}
}

// Whether a tool result reads as success, a missing success key counting as success
func resultSucceeded(result string) bool {
	if result == "" {
		return false
	}
	data, ok := safeJSONLoads(result).(map[string]any)
	if !ok || pyTruthy(data["error"]) {
		return false
	}
	if success, has := data["success"]; has {
		return pyTruthy(success)
	}
	return true
}

// Unified diff of every snapshot file against its current content, empty when nothing changed
func diffFromSnapshot(snapshot *LocalEditSnapshot) string {
	if snapshot == nil {
		return ""
	}
	var chunks []string
	for _, path := range snapshot.Paths {
		before := snapshot.Before[path]
		afterText, afterOK := snapshotText(path)
		var after *string
		if afterOK {
			after = &afterText
		}
		if (before == nil && after == nil) || (before != nil && after != nil && *before == *after) {
			continue
		}
		display := displayDiffPath(path)
		beforeText, afterLines := "", ""
		if before != nil {
			beforeText = *before
		}
		if after != nil {
			afterLines = *after
		}
		diff := strings.Join(unifiedDiff(pySplitLinesKeepEnds(beforeText), pySplitLinesKeepEnds(afterLines), "a/"+display, "b/"+display, 3), "")
		if diff != "" {
			if !strings.HasSuffix(diff, "\n") {
				diff += "\n"
			}
			chunks = append(chunks, diff)
		}
	}
	return strings.Join(chunks, "")
}

// Unified diff of a file edit tool result, the patch tool's own diff first, else the snapshot diff
func ExtractEditDiff(toolName, result string, args map[string]any, snapshot *LocalEditSnapshot) string {
	if toolName == "patch" && result != "" {
		if data, ok := safeJSONLoads(result).(map[string]any); ok {
			if diff, ok := data["diff"].(string); ok && strings.TrimSpace(diff) != "" {
				return diff
			}
		}
	}
	if (toolName != "write_file" && toolName != "patch" && toolName != "skill_manage") || !resultSucceeded(result) {
		return ""
	}
	return diffFromSnapshot(snapshot)
}

// Prints the review header and every rendered diff line through the CLI printer, false when nothing printed
func emitInlineDiff(ctx context.Context, diffText string, printFn func(string)) (emitted bool) {
	if printFn == nil || diffText == "" {
		return false
	}
	defer func() {
		if recover() != nil {
			emitted = false
		}
	}()
	printFn(displayT(ctx, "display.diff.review_header", nil))
	for _, line := range strings.Split(strings.TrimRight(diffText, "\n"), "\n") {
		printFn(line)
	}
	return true
}

// Unified diff line prefixes with their color keys, checked in order after the file header lines
var diffLineColors = [][2]string{{"@@", "hunk"}, {"-", "minus"}, {"+", "plus"}, {" ", "dim"}}

// Unified diff lines in the inline transcript style, file headers folded into one arrow line
func renderInlineUnifiedDiff(ctx context.Context, diff string) []string {
	rendered := []string{}
	fromFile, toFile := "", ""
	for _, raw := range pySplitLines(diff) {
		if strings.HasPrefix(raw, "--- ") {
			fromFile = pystr.Strip(raw[4:])
			continue
		}
		if strings.HasPrefix(raw, "+++ ") {
			toFile = pystr.Strip(raw[4:])
			if fromFile != "" || toFile != "" {
				from, to := fromFile, toFile
				if from == "" {
					from = "a/?"
				}
				if to == "" {
					to = "b/?"
				}
				rendered = append(rendered, diffANSI(ctx)["file"]+from+" → "+to+displayANSIReset)
			}
			continue
		}
		color := ""
		for _, pair := range diffLineColors {
			if strings.HasPrefix(raw, pair[0]) {
				color = pair[1]
				break
			}
		}
		if color != "" {
			rendered = append(rendered, diffANSI(ctx)[color]+raw+displayANSIReset)
		} else if raw != "" {
			rendered = append(rendered, raw)
		}
	}
	return rendered
}

// Per file sections of a unified diff
func splitUnifiedDiffSections(diff string) []string {
	sections := [][]string{{}}
	for _, line := range pySplitLines(diff) {
		if strings.HasPrefix(line, "--- ") && len(sections[len(sections)-1]) > 0 {
			sections = append(sections, []string{})
		}
		sections[len(sections)-1] = append(sections[len(sections)-1], line)
	}
	out := []string{}
	for _, section := range sections {
		if len(section) > 0 {
			out = append(out, strings.Join(section, "\n"))
		}
	}
	return out
}

// Rendered diff sections capped by file count and total lines, a summary line naming what was omitted
func summarizeRenderedDiffSections(ctx context.Context, diff string, maxFiles, maxLines int) []string {
	sections := splitUnifiedDiffSections(diff)
	rendered := []string{}
	omittedFiles, omittedLines := 0, 0
	for idx, section := range sections {
		lines := renderInlineUnifiedDiff(ctx, section)
		remaining := maxLines - len(rendered)
		if idx >= maxFiles || remaining <= 0 {
			omittedFiles++
			omittedLines += len(lines)
			continue
		}
		if len(lines) <= remaining {
			rendered = append(rendered, lines...)
			continue
		}
		rendered = append(rendered, lines[:remaining]...)
		omittedLines += len(lines) - remaining
		omittedFiles += 1 + max(0, len(sections)-idx-1)
		for _, leftover := range sections[idx+1:] {
			omittedLines += len(renderInlineUnifiedDiff(ctx, leftover))
		}
		break
	}
	if omittedFiles > 0 || omittedLines > 0 {
		summary := displayT(ctx, "display.diff.omitted_lines", map[string]any{"count": omittedLines})
		if omittedFiles > 0 {
			summary += displayT(ctx, "display.diff.omitted_files", map[string]any{"count": omittedFiles})
		}
		rendered = append(rendered, diffANSI(ctx)["hunk"]+summary+displayANSIReset)
	}
	return rendered
}

// Section summarizer the inline renderer calls, a var the renderer error test replaces
var summarizeDiffSectionsFn = func(ctx context.Context, diff string) ([]string, error) {
	return summarizeRenderedDiffSections(ctx, diff, maxInlineDiffFiles, maxInlineDiffLines), nil
}

// Renders an edit diff inline through the printer without taking over the terminal, false when nothing shown
func RenderEditDiffWithDelta(ctx context.Context, toolName, result string, args map[string]any, snapshot *LocalEditSnapshot, printFn func(string)) bool {
	diff := ExtractEditDiff(toolName, result, args, snapshot)
	if diff == "" {
		return false
	}
	lines, err := summarizeDiffSectionsFn(ctx, diff)
	if err != nil {
		slog.Default().With("logger", "agent.display").Debug(fmt.Sprintf("Could not render inline diff: %s", err))
		return false
	}
	return emitInlineDiff(ctx, strings.Join(lines, "\n"), printFn)
}
