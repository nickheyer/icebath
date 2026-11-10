package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// HelpKind tags one rendered help line so a surface can style it
type HelpKind int

const (
	HelpBox     HelpKind = iota // one of the three box lines around the header
	HelpSection                 // a section heading of the form -- title --
	HelpRow                     // a padded command and description row
	HelpText                    // a plain line
	HelpDim                     // a de-emphasised hint line
	HelpBlank                   // an empty line
)

// HelpLine is one output line of the CLI help with its styling hints
type HelpLine struct {
	Kind        HelpKind
	Text        string
	Command     string
	Description string
	Width       int
	Suffix      string
}

// HelpInput carries everything the CLI help renderer reads from the host
type HelpInput struct {
	Registry      *Registry
	Cfg           config.Tree
	Skills        map[string]SkillInfo
	Bundles       map[string]BundleInfo
	SkillNotes    []string
	Header        string
	FastAvailable bool
}

const (
	helpDefaultHeader   = "(^_^)? Available Commands"
	helpBoxWidth        = 55
	helpTipLine         = "Tip: /help skills lists skill commands · /help <text> filters · Ctrl+P opens the command palette"
	helpMultilineHint   = "Multi-line: Ctrl+J, Alt+Enter, or \\+Enter for a new line"
	helpDraftEditorHint = "Draft editor: Ctrl+G (Alt+G in VSCode/Cursor)"
	helpPasteImageHint  = "Paste image: Alt+V (or /paste)"
)

// Reports whether this process runs inside Termux on Android
func IsTermux() bool {
	if os.Getenv("TERMUX_VERSION") != "" {
		return true
	}
	return strings.Contains(os.Getenv("PREFIX"), "com.termux")
}

// Returns a realistic example image path for the current Termux storage layout
func TermuxExampleImagePath(filename string) string {
	home, _ := os.UserHomeDir()
	candidates := []string{filepath.Join(home, "storage", "shared"), "/sdcard", "/storage/emulated/0", "/storage/self/primary"}
	for _, root := range candidates {
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			return root + "/Pictures/" + filename
		}
	}
	return "~/storage/shared/Pictures/" + filename
}

func helpRow(cmd, desc string, width int) HelpLine {
	return HelpLine{Kind: HelpRow, Command: cmd, Description: desc, Width: width,
		Text: fmt.Sprintf("    %-*s - %s", width, cmd, desc)}
}

func helpAvailable(in HelpInput, cmd string) bool {
	if cmd == "/fast" {
		return in.FastAvailable
	}
	return true
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Renders bare /help, /help skills, or /help <query> as styled lines for the CLI
func RenderCLIHelp(in HelpInput, arg string) []HelpLine {
	arg = strings.TrimSpace(arg)
	var out []HelpLine
	if l := strings.ToLower(arg); l == "skills" || l == "skill" {
		out = append(out, HelpLine{Kind: HelpBlank})
		if len(in.Skills) > 0 {
			out = append(out, HelpLine{Kind: HelpText, Text: fmt.Sprintf("  ⚡ Skill Commands (%d installed):", len(in.Skills))})
			for _, cmd := range sortedKeys(in.Skills) {
				out = append(out, helpRow(cmd, in.Skills[cmd].Description, 22))
			}
		} else {
			out = append(out, HelpLine{Kind: HelpText, Text: "  No skill commands installed."})
		}
		for _, note := range in.SkillNotes {
			out = append(out, HelpLine{Kind: HelpDim, Text: "    ⚠ " + note})
		}
		out = append(out, HelpLine{Kind: HelpBlank})
		return out
	}
	query := strings.ToLower(arg)
	header := strings.TrimSpace(in.Header)
	if header == "" {
		header = helpDefaultHeader
	}
	if r := []rune(header); len(r) > helpBoxWidth {
		header = string(r[:helpBoxWidth])
	}
	pad := helpBoxWidth - len([]rune(header))
	rule := "+" + strings.Repeat("-", helpBoxWidth) + "+"
	out = append(out, HelpLine{Kind: HelpBlank},
		HelpLine{Kind: HelpBox, Text: rule},
		HelpLine{Kind: HelpBox, Text: "|" + strings.Repeat(" ", pad/2) + header + strings.Repeat(" ", pad-pad/2) + "|"},
		HelpLine{Kind: HelpBox, Text: rule})
	section := func(title string, rows []Row) {
		printed := false
		for _, row := range rows {
			if !helpAvailable(in, row.Command) {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(row.Command), query) && !strings.Contains(strings.ToLower(row.Description), query) {
				continue
			}
			if !printed {
				out = append(out, HelpLine{Kind: HelpBlank}, HelpLine{Kind: HelpSection, Text: "  ── " + title + " ──"})
				printed = true
			}
			out = append(out, helpRow(row.Command, row.Description, 15))
		}
	}
	for _, g := range CommandsByCategory(in.Registry) {
		if g.Category != CategorySession {
			section(g.Category, g.Rows)
			continue
		}
		subOf := map[string]string{}
		for _, sg := range HelpSessionSubgroups {
			for _, n := range sg.Names {
				subOf["/"+n] = sg.Name
			}
		}
		buckets := map[string][]Row{}
		for _, row := range g.Rows {
			sub, ok := subOf[row.Command]
			if !ok {
				sub = CategorySession
			}
			buckets[sub] = append(buckets[sub], row)
		}
		section(CategorySession, buckets[CategorySession])
		for _, sg := range HelpSessionSubgroups {
			section(sg.Name, buckets[sg.Name])
		}
	}
	if query != "" {
		var matched []string
		for _, cmd := range sortedKeys(in.Skills) {
			if strings.Contains(strings.ToLower(cmd), query) || strings.Contains(strings.ToLower(in.Skills[cmd].Description), query) {
				matched = append(matched, cmd)
			}
		}
		if len(matched) > 0 {
			out = append(out, HelpLine{Kind: HelpBlank}, HelpLine{Kind: HelpText, Text: fmt.Sprintf("  ⚡ Skill Commands (matching '%s'):", arg)})
			for _, cmd := range matched {
				out = append(out, helpRow(cmd, in.Skills[cmd].Description, 22))
			}
		}
	} else if len(in.Skills) > 0 {
		out = append(out, HelpLine{Kind: HelpBlank},
			HelpLine{Kind: HelpText, Text: fmt.Sprintf("  ⚡ Skill Commands: %d installed — /help skills to list them", len(in.Skills))})
	}
	if len(in.Bundles) > 0 && query == "" {
		out = append(out, HelpLine{Kind: HelpBlank}, HelpLine{Kind: HelpText, Text: fmt.Sprintf("  ▣ Skill Bundles (%d installed):", len(in.Bundles))})
		for _, cmd := range sortedKeys(in.Bundles) {
			info := in.Bundles[cmd]
			n := len(info.Skills)
			desc := info.Description
			if desc == "" {
				desc = fmt.Sprintf("Load %d skills", n)
			}
			line := helpRow(cmd, desc, 22)
			line.Suffix = fmt.Sprintf("(%d skills)", n)
			line.Text += " " + line.Suffix
			out = append(out, line)
		}
	}
	quick := QuickCommands(in.Cfg)
	if len(quick) > 0 && query == "" {
		out = append(out, HelpLine{Kind: HelpBlank}, HelpLine{Kind: HelpText, Text: fmt.Sprintf("  ⚡ Quick Commands (%d configured):", len(quick))})
		for _, name := range sortedKeys(quick) {
			q := quick[name]
			desc := q.Description
			if desc == "" {
				desc = q.Type
			}
			out = append(out, helpRow("/"+name, desc, 22))
		}
	}
	if query != "" {
		out = append(out, HelpLine{Kind: HelpBlank},
			HelpLine{Kind: HelpDim, Text: fmt.Sprintf("  Filtered by '%s' — run /help for the full list.", arg)},
			HelpLine{Kind: HelpBlank})
		return out
	}
	out = append(out, HelpLine{Kind: HelpBlank},
		HelpLine{Kind: HelpDim, Text: "  " + helpTipLine},
		HelpLine{Kind: HelpDim, Text: "  " + helpMultilineHint},
		HelpLine{Kind: HelpDim, Text: "  " + helpDraftEditorHint})
	if IsTermux() {
		out = append(out, HelpLine{Kind: HelpDim, Text: "  Attach image: /image " + TermuxExampleImagePath("cat.png") + " or start your prompt with a local image path"})
	} else {
		out = append(out, HelpLine{Kind: HelpDim, Text: "  " + helpPasteImageHint})
	}
	out = append(out, HelpLine{Kind: HelpBlank})
	return out
}

// Joins help lines into plain text without styling
func JoinHelpLines(lines []HelpLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = l.Text
	}
	return strings.Join(parts, "\n")
}
