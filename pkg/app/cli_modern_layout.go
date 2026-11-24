package app

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	modernRST                   = "\x1b[0m"
	modernBold                  = "\x1b[1m"
	modernDim                   = "\x1b[2m"
	modernRed                   = "\x1b[38;2;239;83;80m"
	modernSeparatorWidth        = 40
	modernReasoningPreviewLines = 10
	modernDefaultAccent         = "#9cc7e8"
	modernDefaultResponseLabel  = " 󰆧 Icebath "
	modernDefaultBrandGlyph     = "󰆧"
	modernDefaultBrandName      = "Icebath"
)

var (
	modernLayoutActive atomic.Bool
	modernGapPending   atomic.Bool
	modernANSIRe       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	modernDiffFileRe   = regexp.MustCompile(`^a/(.+?) → b/(.+)$`)
)

// Reports whether this CLI draws the glyph layout
func isModern(c *CLI) bool { return c != nil && c.modernLayout }

// Reports the glyph layout, the default, with classic keeping the boxed transcript
func layoutFromConfig(ctx context.Context) bool {
	return settings.DisplayCliLayout.Get(ctx) != "classic"
}

// Color of the active skin for the key, the fallback when the skin holds none
func skinHex(key, fallback string) string {
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		if v := skin.GetColor(key, fallback); v != "" {
			return v
		}
	}
	return fallback
}

// True color foreground escape of a hex color, bold alone or nothing when the color does not parse
func modernFg(hexColor string, bold bool) string {
	digits := strings.TrimLeft(hexColor, "#")
	var rgb [3]string
	for i, start := range []int{0, 2, 4} {
		n, ok := pyint.ParseBase(textutil.Between(digits, start, start+2), 16)
		if !ok {
			if bold {
				return modernBold
			}
			return ""
		}
		rgb[i] = n.String()
	}
	prefix := ""
	if bold {
		prefix = "1;"
	}
	return "\x1b[" + prefix + "38;2;" + rgb[0] + ";" + rgb[1] + ";" + rgb[2] + "m"
}

// Glyph and name from the skin response label, the cube mark and brand name when the label has one word
func brandGlyph() (string, string) {
	label := modernDefaultResponseLabel
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		label = skin.GetBranding("response_label", modernDefaultResponseLabel)
	}
	parts := textutil.FieldsN(label, 1)
	if len(parts) == 2 {
		return parts[0], strings.TrimSpace(parts[1])
	}
	name := strings.TrimSpace(label)
	if name == "" {
		name = modernDefaultBrandName
	}
	return modernDefaultBrandGlyph, name
}

// Opening bar of a block, accent for user messages and answers, dim for reasoning, settling any owed gap
func separator(accent bool) string {
	modernGapPending.Store(false)
	color := modernDim
	if accent {
		color = modernFg(skinHex("ui_accent", modernDefaultAccent), false)
	}
	return color + strings.Repeat("─", modernSeparatorWidth) + modernRST
}

// Closing bar of a block, leaving one blank line owed to whatever prints next
func closingSeparator(accent bool) string {
	bar := separator(accent)
	modernGapPending.Store(true)
	return bar
}

// Newline paying the gap a closing bar left owed, empty when none is owed
func takeGap() string {
	if modernGapPending.Swap(false) {
		return "\n"
	}
	return ""
}

// Rows opening an answer, the accent bar then the skin brand glyph and name
func answerLabel(timestamp string) string {
	glyph, name := brandGlyph()
	stamp := ""
	if timestamp != "" {
		stamp = " " + modernDim + timestamp + modernRST
	}
	return "\n" + separator(true) + "\n" + modernFg(skinHex("ui_accent", modernDefaultAccent), true) + glyph + " " + name + modernRST + stamp
}

// Dim italic thought summary closing a reasoning block, with the seconds when known
func thoughtLine(seconds *float64) string {
	ctx := context.Background()
	took := i18n.T(ctx, "cli.modern.thought", "", nil)
	if seconds != nil && *seconds != 0 {
		took = i18n.T(ctx, "cli.modern.thought_for", "", map[string]any{"seconds": fmt.Sprintf("%.0f", max(*seconds, 0.0))})
	}
	return "\x1b[2;3m∴ " + took + modernRST
}

// One streamed reasoning line, dim with no frame
func reasoningTextLine(line string) string { return modernDim + line + modernRST }

// Reasoning block printed after the fact, a dim bar, ten lines unless full, then the thought summary
func thoughtRows(seconds *float64, text string, full bool) []string {
	lines := textutil.Lines(strings.TrimSpace(text))
	shown := lines
	if !full && len(lines) > modernReasoningPreviewLines {
		shown = lines[:modernReasoningPreviewLines]
	}
	rows := []string{"", separator(false)}
	for _, line := range shown {
		rows = append(rows, reasoningTextLine(line))
	}
	if len(shown) < len(lines) {
		more := i18n.T(context.Background(), "cli.chat.reasoning_more_lines", "", map[string]any{"count": len(lines) - len(shown)})
		rows = append(rows, modernDim+"  "+more+modernRST)
	}
	return append(rows, thoughtLine(seconds), closingSeparator(false))
}

// Scrollback line of a finished tool call in the active layout
func completedToolLine(c *CLI, functionName string, args map[string]any, duration float64, result any, isError bool) string {
	if isModern(c) {
		return takeGap() + toolRow(c.ctx, c.env, functionName, args, duration, result, isError)
	}
	return "  " + tools.GetCuteToolMessage(c.ctx, c.env, functionName, args, duration, result)
}

// One row per completed call led by the tool emoji, a red cross when that entry failed
func toolRow(ctx context.Context, env *tools.Env, functionName string, args map[string]any, duration float64, result any, isError bool) string {
	if args == nil {
		args = map[string]any{}
	}
	rows := tools.GetToolRowParts(ctx, env, functionName, args, result, isError)
	neutral := tools.IsNeutralFailure(functionName, result)
	out := make([]string, 0, len(rows))
	for index, row := range rows {
		verb := row.Verb
		if verb == "" {
			verb = functionName
		}
		verb = strings.TrimSpace(verb)
		mark := row.Emoji
		if row.Failure != nil && !neutral {
			mark = modernRed + "✗" + modernRST
		} else if mark == "" {
			mark = "●"
		}
		line := mark + " " + modernBold + strings.ToUpper(textutil.First(verb, 1)) + textutil.DropFirst(verb, 1) + modernRST
		if row.Detail != "" {
			line += " " + row.Detail
		}
		if index == len(rows)-1 && duration >= 0.1 {
			line += "  " + modernDim + fmt.Sprintf("%.1fs", duration) + modernRST
		}
		if row.Failure != nil && *row.Failure != "" {
			color := modernRed
			if neutral {
				color = modernDim
			}
			line += color + *row.Failure + modernRST
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Records whether the modern layout is active for the process
func setLayoutActive(enabled bool) { modernLayoutActive.Store(enabled) }

// Reports whether the modern layout is active for the process
func layoutActive() bool { return modernLayoutActive.Load() }

// Path relative to the terminal working directory, unchanged when it lies outside it
func displayPath(path string) string {
	if strings.HasPrefix(path, "//") {
		path = path[1:]
	}
	if !pypath.IsAbs(path) {
		return path
	}
	base, err := pypath.Abspath(localFolder())
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(base, pypath.Normpath(path))
	if err != nil {
		return path
	}
	if strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// Wraps a line printer so a diff hangs behind a dim gutter with one relative path as its header
func gutterPrint(printFn func(string)) func(string) {
	header := strings.TrimSpace(modernANSIRe.ReplaceAllString(i18n.T(context.Background(), "display.diff.review_header", "", nil), ""))
	return func(text string) {
		for _, line := range strings.Split(text, "\n") {
			plain := strings.TrimSpace(modernANSIRe.ReplaceAllString(line, ""))
			if plain == header {
				continue
			}
			if m := modernDiffFileRe.FindStringSubmatch(plain); m != nil {
				oldPath, newPath := displayPath(m[1]), displayPath(m[2])
				shown := newPath
				if oldPath != newPath {
					shown = oldPath + " → " + newPath
				}
				line = modernBold + shown + modernRST
			}
			if plain != "" {
				printFn(modernDim + "│" + modernRST + " " + line)
			} else {
				printFn(modernDim + "│" + modernRST)
			}
		}
	}
}

// The printer itself, wrapped in the gutter when the glyph layout is active in this process
func diffPrinter(printFn func(string)) func(string) {
	if layoutActive() {
		return gutterPrint(printFn)
	}
	return printFn
}
