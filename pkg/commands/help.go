package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// GatewayHelpInput carries the registry, config, and skill commands the gateway executors read
type GatewayHelpInput struct {
	Registry *Registry
	Cfg      config.Tree
	Skills   map[string]SkillInfo
	Notes    []string
}

func allowedOption(ctx CommandContext) []string {
	raw, ok := ctx.Options["allowed_commands"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return []string{}
}

// Builds the gateway /help executor whose body ignores the surface
func GatewayHelpExecutor(in GatewayHelpInput) Executor {
	return func(ctx CommandContext) (CommandReply, error) {
		allowed := allowedOption(ctx)
		lines := append([]string{"📖 **Icebath Commands**\n"}, GatewayHelpLines(in.Registry, in.Cfg, allowed)...)
		if allowed == nil && len(in.Skills) > 0 {
			names := sortedKeys(in.Skills)
			lines = append(lines, fmt.Sprintf("\n⚡ **Skill Commands** (%d active):", len(names)))
			shown := names
			if len(shown) > 10 {
				shown = shown[:10]
			}
			for _, cmd := range shown {
				lines = append(lines, "`"+cmd+"` — "+in.Skills[cmd].Description)
			}
			if len(names) > 10 {
				lines = append(lines, fmt.Sprintf("\n... and %d more. Use `/commands` for the full paginated list.", len(names)-10))
			}
		}
		return CommandReply{Text: strings.Join(lines, "\n"), Format: "markdown"}, nil
	}
}

// Builds the gateway /commands executor that paginates commands and skill commands
func GatewayCommandsPageExecutor(in GatewayHelpInput) Executor {
	return func(ctx CommandContext) (CommandReply, error) {
		requested := 1
		if s := strings.TrimSpace(ctx.Args); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return CommandReply{Text: "Usage: `/commands [page]`", Format: "markdown"}, nil
			}
			requested = n
		}
		allowed := allowedOption(ctx)
		entries := GatewayHelpLines(in.Registry, in.Cfg, allowed)
		if allowed == nil && len(in.Skills) > 0 {
			entries = append(entries, "", "⚡ **Skill Commands**:")
			for _, cmd := range sortedKeys(in.Skills) {
				desc := strings.TrimSpace(in.Skills[cmd].Description)
				if desc == "" {
					desc = "Skill command"
				}
				entries = append(entries, "`"+cmd+"` — "+desc)
			}
		}
		if allowed == nil {
			for _, note := range in.Notes {
				if note != "" {
					entries = append(entries, "⚠ "+note)
				}
			}
		}
		if len(entries) == 0 {
			return CommandReply{Text: "No commands available.", Format: "markdown"}, nil
		}
		pageSize := 20
		switch v := ctx.Options["page_size"].(type) {
		case int:
			pageSize = v
		case int64:
			pageSize = int(v)
		case float64:
			pageSize = int(v)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				pageSize = n
			}
		}
		if pageSize < 1 {
			pageSize = 1
		}
		totalPages := (len(entries) + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		page := requested
		if page > totalPages {
			page = totalPages
		}
		if page < 1 {
			page = 1
		}
		start := (page - 1) * pageSize
		end := start + pageSize
		if end > len(entries) {
			end = len(entries)
		}
		lines := append([]string{fmt.Sprintf("📚 **Commands** (%d total, page %d/%d)", len(entries), page, totalPages), ""}, entries[start:end]...)
		if totalPages > 1 {
			var nav []string
			if page > 1 {
				nav = append(nav, fmt.Sprintf("`/commands %d` ← prev", page-1))
			}
			if page < totalPages {
				nav = append(nav, fmt.Sprintf("next → `/commands %d`", page+1))
			}
			lines = append(lines, "", strings.Join(nav, " | "))
		}
		if page != requested {
			lines = append(lines, fmt.Sprintf("_(Requested page %d was out of range, showing page %d.)_", requested, page))
		}
		return CommandReply{Text: strings.Join(lines, "\n"), Format: "markdown"}, nil
	}
}
