package app

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Rich markup of text in a style, the text escaped
func richStyled(style, text string) string {
	if style == "" {
		return pyrich.Escape(text)
	}
	return "[" + style + "]" + pyrich.Escape(text) + "[/]"
}

// Prints a Rich panel through the prompt safe console once the prompt is live, else straight to the console
func (c *CLI) consolePrintPanel(panel pyrich.Panel) {
	if c.app != nil {
		defaultChatConsole.PrintPanel(panel)
		return
	}
	columns, _ := pyshutil.GetTerminalSize(80, 24)
	fmt.Fprintln(c.console.Out, panel.Render(c.console.ShouldUseColor(), columns))
}

// Paints the recap panel of a resumed session, dim earlier exchanges and the last assistant reply in full
func paintResumeHistoryPanel(c *CLI, entries []resumeEntry, skipped int, colors [4]string) {
	historyText, sessionLabel, sessionBorder, assistantLabel := colors[0], colors[1], colors[2], colors[3]
	youLabel := "  " + c.t("cli.resume.label_you", nil) + " "
	agentLabel := "  " + c.t("cli.resume.label_assistant", map[string]any{"agent_name": "Icebath"}) + " "
	type roleStyle struct{ label, labelStyle, bodyStyle string }
	roles := map[string]roleStyle{
		"user":           {youLabel, "dim bold " + sessionLabel, "dim"},
		"assistant":      {agentLabel, "dim bold " + assistantLabel, "dim"},
		"assistant_last": {agentLabel, "bold " + assistantLabel, ""},
	}
	var body strings.Builder
	if skipped > 0 {
		body.WriteString(richStyled("dim italic", "  "+c.t("cli.resume.earlier_messages", map[string]any{"count": skipped})+"\n\n"))
	}
	for _, entry := range entries {
		if entry.Role == "event" {
			body.WriteString(richStyled("dim italic", "  ◈ "+entry.Text+"\n"))
			continue
		}
		style := roles[entry.Role]
		indent := strings.Repeat(" ", pystr.Len(style.label))
		lines := pystr.Splitlines(entry.Text, false)
		if len(lines) == 0 {
			lines = []string{""}
		}
		body.WriteString(richStyled(style.labelStyle, style.label))
		body.WriteString(richStyled(style.bodyStyle, lines[0]+"\n"))
		for _, line := range lines[1:] {
			body.WriteString(richStyled(style.bodyStyle, indent+line+"\n"))
		}
	}
	panel := pyrich.Panel{Body: body.String(), Title: "[dim " + sessionLabel + "]" + pyrich.Escape(c.t("cli.resume.panel_title", nil)) + "[/]",
		BorderStyle: "dim " + sessionBorder, Padding: &[2]int{0, 1}, Style: historyText}
	recordOutputHistoryEntry(func() []string { return c.renderResumeHistoryPanelLines(panel) })
	defer suspendOutputHistory()()
	c.consolePrintPanel(panel)
}
