package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	journeyTitleColor         = "#E8C463"
	chartedSignalMinContrast  = 4.5
	journeyPlayFrames         = 42
	journeyPlayDefaultFPS     = 12
	journeyDefaultCols        = 90
	journeyDefaultRows        = 30
	journeyChartedSignalLimit = 6
)

// Line reader of the journey confirmations, swapped by tests
var journeyInput = func(prompt string) (string, error) { return cliui.Default().Input(prompt) }

// The active skin's primary color, the TUI theme primary, gold when no skin answers
func journeyPrimaryHex(ctx context.Context) (hex string) {
	defer func() {
		if r := recover(); r != nil {
			hex = "#FFD700"
		}
	}()
	skin := config.GetActiveSkin(ctx)
	if c := skin.GetColor("ui_primary", ""); c != "" {
		return c
	}
	return skin.GetColor("banner_title", "#FFD700")
}

// Palette of the journey render, a seam the tests replace
var journeyPalette = func(ctx context.Context) map[string]string { return DerivePalette(journeyPrimaryHex(ctx), true) }

// One styled run of a journey line, an empty style printing plain
type journeyRun struct{ Text, Style string }

type journeyLine []journeyRun

func (l journeyLine) plain() string {
	var b strings.Builder
	for _, r := range l {
		b.WriteString(r.Text)
	}
	return b.String()
}

// Base ink faded toward the background by alpha, the rgba over bg blend
func journeyFade(palette map[string]string, base string, alpha float64) string {
	if base == "" {
		return ""
	}
	if alpha >= 0.999 {
		return base
	}
	return RGBToHex(rgbFloats(MixRGB(HexToRGB(palette["bg"]), HexToRGB(base), alpha)))
}

func journeyResolve(palette map[string]string, style string, alpha float64) string {
	return journeyFade(palette, palette[style], alpha)
}

func relativeLuminance(rgb [3]int) float64 {
	channel := func(value int) float64 {
		n := float64(value) / 255
		if n <= 0.03928 {
			return n / 12.92
		}
		return math.Pow((n+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(rgb[0]) + 0.7152*channel(rgb[1]) + 0.0722*channel(rgb[2])
}

func contrastRatio(foreground, background [3]int) float64 {
	high, low := relativeLuminance(foreground), relativeLuminance(background)
	if low > high {
		high, low = low, high
	}
	return (high + 0.05) / (low + 0.05)
}

// Lifts a foreground toward the readable pole until it clears the minimum contrast
func ensureContrast(color, background string, minimum float64) string {
	if color == "" {
		return ""
	}
	fg, bg := HexToRGB(color), HexToRGB(background)
	if contrastRatio(fg, bg) >= minimum {
		return color
	}
	pole := [3]int{255, 255, 255}
	if relativeLuminance(bg) > 0.5 {
		pole = [3]int{0, 0, 0}
	}
	for step := 1; step <= 20; step++ {
		candidate := MixRGB(fg, pole, float64(step)*0.05)
		if contrastRatio(candidate, bg) >= minimum {
			return RGBToHex(rgbFloats(candidate))
		}
	}
	return RGBToHex(rgbFloats(pole))
}

// Age tinted signal ink kept readable against the background
func resolveChartedSignal(palette map[string]string, style string, alpha float64) string {
	return ensureContrast(journeyResolve(palette, style, alpha), palette["bg"], chartedSignalMinContrast)
}

// A grid row of render runs as styled text, plain when color is off
func journeyRowRuns(palette map[string]string, row []any, color bool) journeyLine {
	var line journeyLine
	for _, item := range row {
		run, _ := item.([]any)
		if len(run) < 2 {
			continue
		}
		chunk, style := pyrepr.Str(run[0]), pyrepr.Str(run[1])
		alpha := 1.0
		if len(run) > 2 {
			if f, ok := journeyFloat(run[2]); ok {
				alpha = f
			}
		}
		override := ""
		if len(run) > 3 && pyrepr.Truthy(run[3]) {
			override = pyrepr.Str(run[3])
		}
		switch {
		case !color:
			line = append(line, journeyRun{Text: chunk})
		case override != "":
			line = append(line, journeyRun{Text: chunk, Style: journeyFade(palette, override, alpha)})
		default:
			line = append(line, journeyRun{Text: chunk, Style: journeyResolve(palette, style, alpha)})
		}
	}
	return line
}

func journeyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// Terminal size of stdout with the hermes fallback of 90 by 30, overrides winning, floors of 40 and 10
func journeyTermSize(width, height int) (int, int) {
	cols, rows := journeyDefaultCols, journeyDefaultRows
	if c, r, err := xterm.GetSize(os.Stdout.Fd()); err == nil && c > 0 && r > 0 {
		cols, rows = c, r
	}
	if width != 0 {
		cols = width
	}
	if height != 0 {
		rows = height
	}
	return max(40, cols), max(10, rows)
}

func styleIf(color bool, style string) string {
	if color {
		return style
	}
	return ""
}

// The journey frame as styled lines, title, legends, field, axis, footer, charted signals and summary
func journeyFrameLines(palette map[string]string, payload map[string]any, cols, rows int, reveal float64, color bool) ([]journeyLine, error) {
	legend, err := BuildLegend(payload)
	if err != nil {
		return nil, err
	}
	categories, err := CategoryLegend(payload, 4)
	if err != nil {
		return nil, err
	}
	summary, err := BuildSummary(payload)
	if err != nil {
		return nil, err
	}
	axis, err := AxisLabels(payload)
	if err != nil {
		return nil, err
	}
	inner := max(24, cols-2)
	fieldRows := max(6, rows-10-len(summary))
	frame, err := RenderGraph(payload, inner, fieldRows, reveal)
	if err != nil {
		return nil, err
	}
	nodes, _ := payload["nodes"].([]any)
	count := len(nodes)
	if typed, ok := payload["nodes"].([]map[string]any); ok {
		count = len(typed)
	}
	lines := []journeyLine{{{Text: "✦ Journey ", Style: styleIf(color, "bold "+journeyTitleColor)}, {Text: "· learned skills & memories over time", Style: styleIf(color, "grey62")}}}
	legendLine := journeyLine{{Text: "  "}}
	for i, item := range legend {
		if i > 0 {
			legendLine = append(legendLine, journeyRun{Text: "   "})
		}
		glyphStyle := ""
		if color {
			glyphStyle = journeyResolve(palette, pyrepr.Str(item["style"]), 1.0)
		}
		legendLine = append(legendLine, journeyRun{Text: pyrepr.Str(item["glyph"]) + " ", Style: glyphStyle}, journeyRun{Text: pyrepr.Str(item["label"]), Style: styleIf(color, "grey62")})
	}
	lines = append(lines, legendLine)
	if len(categories) > 0 {
		catLine := journeyLine{{Text: "  "}}
		for i, item := range categories {
			if i > 0 {
				catLine = append(catLine, journeyRun{Text: "  "})
			}
			glyphStyle := ""
			if color {
				glyphStyle = journeyFade(palette, pyrepr.Str(item["color"]), 1.0)
			}
			catLine = append(catLine, journeyRun{Text: pyrepr.Str(item["glyph"]) + " ", Style: glyphStyle}, journeyRun{Text: pyrepr.Str(item["label"]), Style: styleIf(color, "grey54")})
		}
		lines = append(lines, catLine)
	}
	lines = append(lines, journeyLine{})
	grid, _ := frame["grid"].([]any)
	for _, growAny := range grid {
		grow, _ := growAny.([]any)
		lines = append(lines, append(journeyLine{{Text: "  "}}, journeyRowRuns(palette, grow, color)...))
	}
	gap := max(1, inner-pystr.Len(axis["start"])-pystr.Len(axis["end"]))
	lines = append(lines, journeyLine{{Text: "  "}, {Text: axis["start"], Style: styleIf(color, "grey54")}, {Text: strings.Repeat(" ", gap)}, {Text: axis["end"], Style: styleIf(color, "grey54")}})
	pct := int(math.RoundToEven(reveal * 100))
	date := pyrepr.Str(frame["date"])
	if !pyrepr.Truthy(frame["date"]) {
		date = "—"
	}
	lines = append(lines, journeyLine{{Text: "  "}, {Text: "◷ ", Style: styleIf(color, "grey54")}, {Text: date, Style: styleIf(color, journeyTitleColor)},
		{Text: fmt.Sprintf("   %s/%d revealed · %d%%", pyrepr.Str(frame["visible"]), count, pct), Style: styleIf(color, "grey54")}})
	labels := journeyLabelEntries(frame["labels"])
	if len(labels) > 0 {
		lines = append(lines, journeyLine{}, journeyLine{{Text: "  charted signals", Style: styleIf(color, "grey62")}})
		for _, item := range labels[:min(journeyChartedSignalLimit, len(labels))] {
			signalStyle := ""
			if color {
				alpha := 1.0
				if f, ok := journeyFloat(item["alpha"]); ok {
					alpha = f
				}
				signalStyle = resolveChartedSignal(palette, pyrepr.Str(item["style"]), alpha)
			}
			meta := pyrepr.Str(item["meta"])
			if pystr.Len(meta) > 32 {
				meta = pystr.Slice(meta, 0, 29) + "…"
			}
			lines = append(lines, journeyLine{{Text: "  "}, {Text: pyrepr.Str(item["key"]) + " ", Style: styleIf(color, "grey70")},
				{Text: pyrepr.Str(item["glyph"]) + " ", Style: signalStyle}, {Text: pyrepr.Str(item["label"]), Style: signalStyle},
				{Text: "  " + meta, Style: styleIf(color, "grey54")}})
		}
	}
	for _, text := range summary {
		lines = append(lines, journeyLine{{Text: "  " + text, Style: styleIf(color, "grey62")}})
	}
	return lines, nil
}

// Label entries of a frame, maps whatever list shape the renderer returned
func journeyLabelEntries(v any) []map[string]any {
	switch items := v.(type) {
	case []map[string]any:
		return items
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Console the journey prints on, ANSI only on a terminal or when forced for re emission
type journeyConsole struct {
	out    io.Writer
	ansi   bool
	system string
	width  int
}

func newJourneyConsole(w io.Writer, color bool, width int, force bool) *journeyConsole {
	tty := false
	if f, ok := w.(*os.File); ok {
		tty = xterm.IsTerminal(f.Fd())
	}
	c := &journeyConsole{out: w, width: width}
	switch {
	case !color:
	case force:
		c.ansi, c.system = true, "truecolor"
	case tty:
		c.ansi, c.system = true, pyrich.TermColorSystem()
	}
	if c.width == 0 {
		c.width = pyrich.DetectWidth()
	}
	return c
}

// One styled run rendered, a style that rich cannot parse surfacing as the markup error it raises
func (c *journeyConsole) render(run journeyRun) (string, error) {
	if !c.ansi || run.Style == "" || run.Text == "" {
		return run.Text, nil
	}
	style, err := pyrich.ParseStyle(run.Style)
	if err != nil {
		return "", err
	}
	return style.Render(run.Text, c.system), nil
}

// Prints lines wrapped at the console width the way a rich console prints Text renderables
func (c *journeyConsole) printLines(lines []journeyLine) error {
	for _, line := range lines {
		var b strings.Builder
		for _, run := range line {
			text, err := c.render(run)
			if err != nil {
				return err
			}
			b.WriteString(text)
		}
		for _, wrapped := range pyrich.WrapStyled(b.String(), c.width, true) {
			fmt.Fprintln(c.out, wrapped)
		}
	}
	return nil
}

// Lines a frame occupies on screen after wrapping
func (c *journeyConsole) lineCount(lines []journeyLine) (int, error) {
	n := 0
	for _, line := range lines {
		var b strings.Builder
		for _, run := range line {
			text, err := c.render(run)
			if err != nil {
				return 0, err
			}
			b.WriteString(text)
		}
		n += len(pyrich.WrapStyled(b.String(), c.width, true))
	}
	return n, nil
}

// Rich JSON highlighter styles over the pretty printed payload
var journeyJSONStyles = map[string]string{"brace": "bold", "bool_true": "italic bright_green", "bool_false": "italic bright_red",
	"null": "italic magenta", "number": "bold not italic cyan", "str": "not bold not italic green", "key": "bold blue"}

// The payload as print_json shows it, indented two and highlighted like rich on a color console
func (c *journeyConsole) printJSON(payload map[string]any) error {
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	if !c.ansi {
		fmt.Fprintln(c.out, text)
		return nil
	}
	var b strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r == '"':
			j := i + 1
			for j < len(runes) && !(runes[j] == '"' && runes[j-1] != '\\') {
				j++
			}
			j = min(j+1, len(runes))
			token := string(runes[i:j])
			kind := "str"
			k := j
			for k < len(runes) && (runes[k] == ' ' || runes[k] == '\t') {
				k++
			}
			if k < len(runes) && runes[k] == ':' {
				kind = "key"
			}
			styled, err := c.render(journeyRun{Text: token, Style: journeyJSONStyles[kind]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i = j
		case strings.ContainsRune("{}[]()", r):
			styled, err := c.render(journeyRun{Text: string(r), Style: journeyJSONStyles["brace"]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i++
		case r == '-' || (r >= '0' && r <= '9'):
			j := i + 1
			for j < len(runes) && strings.ContainsRune("0123456789.eE-+", runes[j]) {
				j++
			}
			styled, err := c.render(journeyRun{Text: string(runes[i:j]), Style: journeyJSONStyles["number"]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i = j
		case strings.HasPrefix(string(runes[i:min(i+5, len(runes))]), "true"):
			styled, err := c.render(journeyRun{Text: "true", Style: journeyJSONStyles["bool_true"]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i += 4
		case strings.HasPrefix(string(runes[i:min(i+5, len(runes))]), "false"):
			styled, err := c.render(journeyRun{Text: "false", Style: journeyJSONStyles["bool_false"]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i += 5
		case strings.HasPrefix(string(runes[i:min(i+4, len(runes))]), "null"):
			styled, err := c.render(journeyRun{Text: "null", Style: journeyJSONStyles["null"]})
			if err != nil {
				return err
			}
			b.WriteString(styled)
			i += 4
		default:
			b.WriteRune(r)
			i++
		}
	}
	fmt.Fprintln(c.out, b.String())
	return nil
}

func journeyClamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	return math.Min(v, hi)
}

func nsJourneyBool(ns map[string]any, key string) bool { return pyrepr.Truthy(ns[key]) }

func nsJourneyInt(ns map[string]any, key string) int {
	f, ok := journeyFloat(ns[key])
	if !ok {
		return 0
	}
	return int(f)
}

// Shows the timeline once, animates it with --play, or prints the payload with --json
func journeyCmdShow(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	noColor := nsJourneyBool(ns, "no_color")
	if nsJourneyBool(ns, "json") {
		payload, err := BuildLearningGraph(ctx, env)
		if err != nil {
			return 1, err
		}
		return 0, newJourneyConsole(w, !noColor, 0, false).printJSON(payload)
	}
	payload, err := BuildLearningGraph(ctx, env)
	if err != nil {
		return 1, err
	}
	color := !noColor
	cols, rows := journeyTermSize(nsJourneyInt(ns, "width"), nsJourneyInt(ns, "height"))
	console := newJourneyConsole(w, color, cols, nsJourneyBool(ns, "force_color"))
	if !pyrepr.Truthy(payload["nodes"]) || journeyNodeCount(payload) == 0 {
		return 0, console.printLines([]journeyLine{{{Text: "No learning yet — use Icebath a while and your learned skills and memories will start mapping out here.", Style: styleIf(color, "grey62")}}})
	}
	palette := journeyPalette(ctx)
	if nsJourneyBool(ns, "play") {
		fps := nsJourneyInt(ns, "fps")
		if fps == 0 && ns["fps"] == nil {
			fps = journeyPlayDefaultFPS
		}
		return journeyPlay(console, palette, payload, cols, rows, color, fps)
	}
	reveal := 1.0
	if f, ok := journeyFloat(ns["reveal"]); ok && f != 0 {
		reveal = f
	}
	reveal = journeyClamp(reveal, 0, 1)
	lines, err := journeyFrameLines(palette, payload, cols, rows, reveal, color)
	if err != nil {
		return 1, err
	}
	return 0, console.printLines(lines)
}

func journeyNodeCount(payload map[string]any) int {
	switch nodes := payload["nodes"].(type) {
	case []any:
		return len(nodes)
	case []map[string]any:
		return len(nodes)
	}
	return 0
}

// Animates the build up over time in place, Ctrl-C printing interrupted and exiting 130
func journeyPlay(console *journeyConsole, palette map[string]string, payload map[string]any, cols, rows int, color bool, fps int) (int, error) {
	delay := time.Duration(float64(time.Second) / float64(max(1, min(60, fps))))
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	drawn := 0
	draw := func(reveal float64) error {
		lines, err := journeyFrameLines(palette, payload, cols, rows, reveal, color)
		if err != nil {
			return err
		}
		if drawn > 0 {
			fmt.Fprintf(console.out, "\x1b[%dA", drawn)
			for i := 0; i < drawn; i++ {
				fmt.Fprint(console.out, "\x1b[2K\n")
			}
			fmt.Fprintf(console.out, "\x1b[%dA", drawn)
		}
		if err := console.printLines(lines); err != nil {
			return err
		}
		drawn, err = console.lineCount(lines)
		return err
	}
	if !console.ansi {
		lines, err := journeyFrameLines(palette, payload, cols, rows, 1.0, color)
		if err != nil {
			return 1, err
		}
		return 0, console.printLines(lines)
	}
	for i := 0; i < journeyPlayFrames; i++ {
		if err := draw(float64(i) / float64(journeyPlayFrames-1)); err != nil {
			return 1, err
		}
		select {
		case <-interrupts:
			if err := console.printLines([]journeyLine{{{Text: "interrupted", Style: styleIf(color, "grey54")}}}); err != nil {
				return 1, err
			}
			return 130, nil
		case <-time.After(delay):
		}
	}
	if err := draw(1.0); err != nil {
		return 1, err
	}
	return 0, nil
}

// Lists node ids with their glyph, label and date for delete and edit
func journeyCmdList(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	console := newJourneyConsole(w, !nsJourneyBool(ns, "no_color"), 0, nsJourneyBool(ns, "force_color"))
	payload, err := BuildLearningGraph(ctx, env)
	if err != nil {
		return 1, err
	}
	nodes := journeyNodes(payload)
	sort.SliceStable(nodes, func(i, j int) bool {
		ti, _ := journeyFloat(nodes[i]["timestamp"])
		tj, _ := journeyFloat(nodes[j]["timestamp"])
		return ti < tj
	})
	color := console.ansi || !nsJourneyBool(ns, "no_color")
	if len(nodes) == 0 {
		return 0, console.printLines([]journeyLine{{{Text: "No learning yet.", Style: styleIf(color, "grey62")}}})
	}
	var lines []journeyLine
	for _, node := range nodes {
		glyph := "●"
		if pyrepr.Str(node["kind"]) == "memory" {
			glyph = "◆"
		}
		var ts *float64
		if f, ok := journeyFloat(node["timestamp"]); ok {
			ts = &f
		}
		lines = append(lines, journeyLine{{Text: pyrepr.Str(node["id"]), Style: styleIf(color, "grey54")}, {Text: fmt.Sprintf("  %s %s  ", glyph, pyrepr.Str(node["label"]))},
			{Text: FormatDate(ts), Style: styleIf(color, "grey54")}})
	}
	return 0, console.printLines(lines)
}

func journeyNodes(payload map[string]any) []map[string]any {
	switch nodes := payload["nodes"].(type) {
	case []map[string]any:
		return append([]map[string]any{}, nodes...)
	case []any:
		out := make([]map[string]any, 0, len(nodes))
		for _, n := range nodes {
			if m, ok := n.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Node detail of an id, the failure printed and nil returned when missing
func journeyLookupNode(ctx context.Context, env *tools.Env, w io.Writer, nodeID string) (map[string]any, error) {
	detail, err := NodeDetail(ctx, env, nodeID)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(detail["ok"]) {
		message := "not found"
		if pyrepr.Truthy(detail["message"]) {
			message = pyrepr.Str(detail["message"])
		}
		fmt.Fprintf(w, "  %s\n", message)
		return nil, nil
	}
	return detail, nil
}

// Deletes a learned skill or memory by node id after a confirmation unless --yes
func journeyCmdDelete(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	nodeID := pyrepr.Str(ns["node"])
	detail, err := journeyLookupNode(ctx, env, w, nodeID)
	if err != nil {
		return 1, err
	}
	if detail == nil {
		return 1, nil
	}
	if !nsJourneyBool(ns, "yes") {
		answer, err := journeyInput(fmt.Sprintf("  Delete %s? [y/N] ", pyrepr.Repr(pyrepr.Str(detail["label"]))))
		if err != nil {
			fmt.Fprintln(w, "\n  aborted")
			return 1, nil
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(w, "  aborted")
			return 1, nil
		}
	}
	res, err := DeleteNode(ctx, env, nodeID)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "  %s\n", pyrepr.Str(res["message"]))
	if pyrepr.Truthy(res["ok"]) {
		return 0, nil
	}
	return 1, nil
}

// Edits a learned skill or memory by node id in the user's editor
func journeyCmdEdit(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	nodeID := pyrepr.Str(ns["node"])
	detail, err := journeyLookupNode(ctx, env, w, nodeID)
	if err != nil {
		return 1, err
	}
	if detail == nil {
		return 1, nil
	}
	suffix := ".txt"
	if pyrepr.Str(detail["kind"]) == "skill" {
		suffix = ".md"
	}
	content := pyrepr.Str(detail["content"])
	edited, ok := journeyOpenInEditor(w, content, suffix)
	if !ok || strings.TrimSpace(edited) == strings.TrimSpace(content) {
		fmt.Fprintln(w, "  no changes")
		return 0, nil
	}
	res, err := EditNode(ctx, env, nodeID, edited)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "  %s\n", pyrepr.Str(res["message"]))
	if pyrepr.Truthy(res["ok"]) {
		return 0, nil
	}
	return 1, nil
}

// Editor launcher of the edit subcommand, swapped by tests
var journeyRunEditor = func(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// Opens the text in EDITOR, VISUAL or vi through a temp file and returns what was saved, false when the editor failed to run
func journeyOpenInEditor(w io.Writer, initial, suffix string) (string, bool) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "tmp*"+suffix)
	if err != nil {
		fmt.Fprintf(w, "  editor failed: %s\n", pyrepr.ExceptionMessage(err))
		return "", false
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		fmt.Fprintf(w, "  editor failed: %s\n", pyrepr.ExceptionMessage(err))
		return "", false
	}
	f.Close()
	argv := append(strings.Fields(editor), path)
	err = journeyRunEditor(argv)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		fmt.Fprintf(w, "  editor failed: %s\n", pyrepr.ExceptionMessage(err))
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(w, "  editor failed: %s\n", pyrepr.ExceptionMessage(err))
		return "", false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		fmt.Fprintf(w, "  editor failed: %s\n", pyrepr.ExceptionMessage(err))
		return "", false
	}
	return text, true
}

// JourneyCommand runs icebath journey, hermes_cli/journey.py show, list, delete and edit
func JourneyCommand(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	action := ""
	if pyrepr.Truthy(ns["journey_action"]) {
		action = pyrepr.Str(ns["journey_action"])
	}
	switch action {
	case "list":
		return journeyCmdList(ctx, env, ns, w)
	case "delete":
		return journeyCmdDelete(ctx, env, ns, w)
	case "edit":
		return journeyCmdEdit(ctx, env, ns, w)
	}
	return journeyCmdShow(ctx, env, ns, w)
}
