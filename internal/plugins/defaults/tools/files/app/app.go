package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	filestools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/files/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Most files and lines of a patch's diff shown before the rest is counted
const (
	diffFiles = 6
	diffLines = 80
)

type views struct{}

func Register(registry *toolsapp.Registry) {
	registry.Register(views{})
}

// A read shows its page folded, a write what it wrote and how its checks went, a patch its diff, and a search its hits folded
func (views) Views() []toolsapp.View {
	return []toolsapp.View{
		{Tool: filestools.ReadFile, Line: readLine, Result: readResult},
		{Tool: filestools.WriteFile, Line: pathLine, Result: writeResult, Settings: []string{toolsconfig.InlineDiffs}},
		{Tool: filestools.Patch, Line: patchLine, Result: patchResult, Settings: []string{toolsconfig.InlineDiffs}},
		{Tool: filestools.SearchFiles, Line: searchLine, Result: searchResult},
	}
}

func arguments(call toolsapp.Call) map[string]any {
	var args map[string]any
	json.Unmarshal([]byte(call.Arguments), &args)
	return args
}

// Whether the chat shows changes to files as diffs, as the inline diffs setting says, on unless it is set off
func inline(call toolsapp.Call) bool {
	return strings.TrimSpace(call.Settings[toolsconfig.InlineDiffs]) != "false"
}

// The path, and the lines asked for when the read starts past the first
func readLine(call toolsapp.Call) string {
	args := arguments(call)
	path, _ := args["path"].(string)
	offset, ok := config.Whole(args["offset"])
	if !ok || offset <= 0 {
		return path
	}
	if limit, ok := config.Whole(args["limit"]); ok && limit > 1 {
		return fmt.Sprintf("%s L%d-%d", path, offset, offset+limit-1)
	}
	return fmt.Sprintf("%s L%d", path, offset)
}

func pathLine(call toolsapp.Call) string {
	path, _ := arguments(call)["path"].(string)
	return path
}

// The path edited, or the files a V4A patch names
func patchLine(call toolsapp.Call) string {
	args := arguments(call)
	if path, _ := args["path"].(string); path != "" {
		return path
	}
	patch, _ := args["patch"].(string)
	var files []string
	for line := range strings.SplitSeq(patch, "\n") {
		for _, lead := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "*** Move File:"} {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), lead); ok {
				files = append(files, strings.TrimSpace(rest))
			}
		}
	}
	switch len(files) {
	case 0:
		return ""
	case 1:
		return files[0]
	}
	return fmt.Sprintf("%s (+%d more)", files[0], len(files)-1)
}

// What was looked for, and how
func searchLine(call toolsapp.Call) string {
	args := arguments(call)
	verb := "grep"
	if target, _ := args["target"].(string); target == "files" || target == "find" {
		verb = "find"
	}
	pattern, _ := args["pattern"].(string)
	line := verb + " " + pattern
	if path, _ := args["path"].(string); path != "" && path != "." {
		line += " in " + path
	}
	return line
}

// Lines of text with tabs made spaces
func rows(text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n")
}

// The page read under a head saying which lines of how many, folded
func readResult(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var r struct {
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
		Truncated  bool   `json:"truncated"`
		NextOffset int    `json:"next_offset"`
		HexView    bool   `json:"hex_view"`
		Document   bool   `json:"extracted_document"`
		Status     string `json:"status"`
		Hint       string `json:"hint"`
	}
	if json.Unmarshal([]byte(call.Result), &r) != nil {
		return nil
	}
	if r.Status == "unchanged" {
		return &toolsapp.Shown{Head: render.Dim("unchanged since the last read", look)}
	}
	lines := rows(r.Content)
	if len(lines) == 0 {
		if r.Hint != "" {
			return &toolsapp.Shown{Head: render.Dim(r.Hint, look)}
		}
		return nil
	}
	unit := "lines"
	switch {
	case r.HexView:
		unit = "rows"
	case r.Document:
		unit = "lines of extracted text"
	}
	head := fmt.Sprintf("%d of %d %s", len(lines), r.TotalLines, unit)
	if !r.Truncated || r.TotalLines <= len(lines) {
		head = fmt.Sprintf("%d %s", len(lines), unit)
	}
	if r.NextOffset > 0 {
		head += fmt.Sprintf(" · continues at %d", r.NextOffset)
	}
	return &toolsapp.Shown{Head: render.Dim(head, look), Rows: lines}
}

// How a syntax check went, as a short tag
func lintTag(raw json.RawMessage, look theme.Theme) string {
	var lint struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &lint) != nil {
		return ""
	}
	switch lint.Status {
	case "ok":
		return render.Good("syntax ok", look)
	case "error":
		return render.Bad("syntax errors", look)
	}
	return ""
}

// What a write put in the file under a head with its size and how its checks went, folded
func writeResult(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var r struct {
		BytesWritten int             `json:"bytes_written"`
		Lint         json.RawMessage `json:"lint"`
		Diagnostics  string          `json:"lsp_diagnostics"`
		Warning      string          `json:"_warning"`
		Hint         string          `json:"hint"`
	}
	if json.Unmarshal([]byte(call.Result), &r) != nil {
		return nil
	}
	content, _ := arguments(call)["content"].(string)
	lines := rows(content)
	parts := []string{render.Dim(fmt.Sprintf("wrote %d lines, %s", len(lines), render.Bytes(int64(r.BytesWritten))), look)}
	if tag := lintTag(r.Lint, look); tag != "" {
		parts = append(parts, tag)
	}
	shown := &toolsapp.Shown{Head: strings.Join(parts, render.Dim(" · ", look))}
	for _, note := range []string{r.Warning, r.Hint, r.Diagnostics} {
		for _, line := range rows(note) {
			shown.Rows = append(shown.Rows, render.Warm(line, look))
		}
	}
	if call.Display != "" && inline(call) {
		// What the write changed, as a diff against the file it replaced, open as a patch's is
		shown.Open = true
		shown.Rows = append(shown.Rows, diffRows(call.Display, look)...)
		return shown
	}
	shown.Rows = append(shown.Rows, lines...)
	return shown
}

// The diff of a patch, its file headers made one arrow line and its rows toned, shown open under a head counting what changed
func patchResult(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var r struct {
		Success     bool            `json:"success"`
		NoChange    bool            `json:"no_change"`
		Note        string          `json:"note"`
		Diff        string          `json:"diff"`
		Lint        json.RawMessage `json:"lint"`
		Diagnostics string          `json:"lsp_diagnostics"`
		Warning     string          `json:"_warning"`
	}
	if json.Unmarshal([]byte(call.Result), &r) != nil || !r.Success {
		return nil
	}
	if r.NoChange {
		return &toolsapp.Shown{Head: render.Dim("already applied, nothing changed", look)}
	}
	added, removed := 0, 0
	for _, line := range strings.Split(r.Diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	parts := []string{render.Good(fmt.Sprintf("+%d", added), look) + " " + render.Bad(fmt.Sprintf("-%d", removed), look)}
	if tag := lintTag(r.Lint, look); tag != "" {
		parts = append(parts, tag)
	}
	shown := &toolsapp.Shown{Head: strings.Join(parts, render.Dim(" · ", look)), Open: inline(call)}
	for _, note := range []string{r.Warning, r.Note, r.Diagnostics} {
		for _, line := range rows(note) {
			shown.Rows = append(shown.Rows, render.Warm(line, look))
		}
	}
	shown.Rows = append(shown.Rows, diffRows(r.Diff, look)...)
	return shown
}

// Sections of a unified diff, one for each file
func sections(diff string) [][]string {
	var out [][]string
	for _, line := range rows(diff) {
		if strings.HasPrefix(line, "--- ") && len(out) > 0 && len(out[len(out)-1]) > 0 || len(out) == 0 {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], line)
	}
	return out
}

// Rows of one section: its headers as one arrow line, hunk heads in the accent, added and removed lines in their tones
func sectionRows(section []string, look theme.Theme) []string {
	var out []string
	from := ""
	for _, line := range section {
		switch {
		case strings.HasPrefix(line, "--- "):
			from = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "+++ "):
			to := strings.TrimSpace(line[4:])
			if from == "" {
				from = "a/?"
			}
			out = append(out, render.Heading(from+" → "+to, look))
		case strings.HasPrefix(line, "@@"):
			out = append(out, render.Warm(line, look))
		case strings.HasPrefix(line, "+"):
			out = append(out, render.Good(line, look))
		case strings.HasPrefix(line, "-"):
			out = append(out, render.Bad(line, look))
		case strings.HasPrefix(line, "#"):
			out = append(out, render.Heading(line, look))
		default:
			out = append(out, render.Dim(line, look))
		}
	}
	return out
}

// Rows of a diff, at most six files and 80 lines of it, the rest counted in a last row
func diffRows(diff string, look theme.Theme) []string {
	var out []string
	omittedFiles, omittedLines := 0, 0
	all := sections(diff)
	for i, section := range all {
		lines := sectionRows(section, look)
		left := diffLines - len(out)
		if i >= diffFiles || left <= 0 {
			omittedFiles++
			omittedLines += len(lines)
			continue
		}
		if len(lines) <= left {
			out = append(out, lines...)
			continue
		}
		out = append(out, lines[:left]...)
		omittedLines += len(lines) - left
		omittedFiles += 1 + len(all) - i - 1
		for _, rest := range all[i+1:] {
			omittedLines += len(sectionRows(rest, look))
		}
		break
	}
	if omittedFiles > 0 || omittedLines > 0 {
		summary := fmt.Sprintf("… omitted %d diff line(s)", omittedLines)
		if omittedFiles > 0 {
			summary += fmt.Sprintf(" across %d additional file(s)/section(s)", omittedFiles)
		}
		out = append(out, render.Dim(summary, look))
	}
	return out
}

// What a search found under a head counting it, folded
func searchResult(call toolsapp.Call, _ int, look theme.Theme) *toolsapp.Shown {
	var r struct {
		Total   int               `json:"total_count"`
		Text    string            `json:"matches_text"`
		Matches []json.RawMessage `json:"matches"`
		Files   []string          `json:"files"`
		Counts  map[string]int    `json:"counts"`
		Warning string            `json:"warning"`
	}
	if json.Unmarshal([]byte(call.Result), &r) != nil {
		return nil
	}
	shown := &toolsapp.Shown{}
	switch {
	case r.Text != "":
		shown.Head = fmt.Sprintf("%d match(es)", r.Total)
		shown.Rows = rows(r.Text)
	case len(r.Matches) > 0:
		shown.Head = fmt.Sprintf("%d match(es)", r.Total)
		for _, raw := range r.Matches {
			var m struct {
				Path    string `json:"path"`
				Line    int    `json:"line"`
				Content string `json:"content"`
			}
			json.Unmarshal(raw, &m)
			shown.Rows = append(shown.Rows, fmt.Sprintf("%s:%d: %s", m.Path, m.Line, strings.ReplaceAll(m.Content, "\t", "    ")))
		}
	case len(r.Files) > 0:
		shown.Head = fmt.Sprintf("%d file(s)", r.Total)
		shown.Rows = r.Files
	case len(r.Counts) > 0:
		shown.Head = fmt.Sprintf("%d match(es) in %d file(s)", r.Total, len(r.Counts))
		for _, path := range slices.Sorted(maps.Keys(r.Counts)) {
			shown.Rows = append(shown.Rows, fmt.Sprintf("%s: %d", path, r.Counts[path]))
		}
	default:
		shown.Head = "nothing found"
	}
	shown.Head = render.Dim(shown.Head, look)
	var warning []string
	for _, line := range rows(r.Warning) {
		warning = append(warning, render.Warm(line, look))
	}
	shown.Rows = append(warning, shown.Rows...)
	return shown
}
