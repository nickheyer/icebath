package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/prompt"
	promptv1 "github.com/nickheyer/icebath/pkg/gen/icebath/prompt/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/prompt/v1/promptv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// What each tier of the prompt holds, as the breakdown labels it
var tiers = map[string]string{
	prompt.Stable.String():  "stable (identity/guidance)",
	prompt.Project.String(): "project (context files/workspace)",
	prompt.Kept.String():    "kept (memory/skills)",
	prompt.Turn.String():    "turn (profile/time/environment)",
}

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := promptv1connect.NewPromptServiceClient(conn)
	var platform string
	var sizeJSON bool
	size := &cobra.Command{
		Use:     "prompt-size",
		GroupID: cli.GroupSessions,
		Short:   "Show how big the agent's system prompt and tools are",
		Long: "Shows, part by part, how many bytes the system prompt and the tool descriptions take for a session started in this folder. " +
			"Use prompt-size to see what fills the model's context window before you send anything.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			directory, err := os.Getwd()
			if err == nil {
				var response *promptv1.SizeResponse
				if response, err = service.Size(cmd.Context(), &promptv1.SizeRequest{Directory: directory, Platform: platform}); err == nil {
					if sizeJSON {
						return breakdownJSON(out, response)
					}
					_, err = fmt.Fprintln(out, breakdown(response))
					return err
				}
			}
			_, err = fmt.Fprintln(out, "✗ The size of the system prompt could not be worked out. "+tui.Reason(err))
			return err
		},
	}
	size.Flags().StringVar(&platform, "platform", "tui", "Where the session runs: tui for this app, cli, cron and so on")
	size.Flags().BoolVar(&sizeJSON, "json", false, "Print the breakdown as JSON")
	return []*cobra.Command{size}
}

// Parts of the prompt by how often they change, in words
var tierWords = map[string]string{
	prompt.Stable.String():  "Always the same: identity and advice",
	prompt.Project.String(): "Per project: project files and folder",
	prompt.Kept.String():    "Kept for the session: memory and skills",
	prompt.Turn.String():    "Per message: profile, time and setup",
}

// Size in words, right aligned in ten columns
func sized10(n int64) string {
	return fmt.Sprintf("%10s", strs.Size(float64(n)))
}

func breakdown(size *promptv1.SizeResponse) string {
	model := cmp.Or(size.GetModel(), "no model set")
	where := size.GetPlatform()
	if where == "tui" {
		where = "this app"
	}
	lines := []string{
		"System prompt of a session in this folder, shown in " + where + ", with " + model, "",
		"  Whole prompt   " + sized10(size.GetBytes()) + ", " + strs.Counted(float64(size.GetChars()), "characters"),
	}
	if sections := size.GetSections(); len(sections) > 0 {
		lines = append(lines, "", "  Parts")
		for _, section := range sections {
			lines = append(lines, "    "+fmt.Sprintf("%-40s", section.GetName())+sized10(section.GetBytes()))
		}
	}
	lines = append(lines, "", "  By how often they change")
	for _, tier := range size.GetTiers() {
		lines = append(lines, "    "+fmt.Sprintf("%-40s", cmp.Or(tierWords[tier.GetName()], tier.GetName()))+sized10(tier.GetBytes()))
	}
	lines = append(lines, "", "  Tool descriptions "+sized10(size.GetToolBytes())+", "+strs.Count(int(size.GetTools()), "tool"))
	if toolsets := size.GetToolsets(); len(toolsets) > 0 {
		lines = append(lines, "", "  Groups of tools, largest first")
		for _, toolset := range toolsets {
			lines = append(lines, "    "+fmt.Sprintf("%-28s", toolset.GetName())+fmt.Sprintf("%10s", strs.Count(int(toolset.GetTools()), "tool"))+"  "+
				sized10(toolset.GetBytes()))
		}
	}
	return strings.Join(lines, "\n")
}

type sized struct {
	Chars int64 `json:"chars"`
	Bytes int64 `json:"bytes"`
}

type block struct {
	Name  string `json:"name"`
	Tier  string `json:"tier"`
	Chars int64  `json:"chars"`
	Bytes int64  `json:"bytes"`
}

type toolset struct {
	Toolset   string `json:"toolset"`
	ToolCount int64  `json:"tool_count"`
	JSONBytes int64  `json:"json_bytes"`
}

func breakdownJSON(out io.Writer, size *promptv1.SizeResponse) error {
	document := struct {
		Platform     string  `json:"platform"`
		Model        string  `json:"model"`
		SystemPrompt sized   `json:"system_prompt"`
		Blocks       []block `json:"blocks"`
		Tools        struct {
			Count     int64 `json:"count"`
			JSONBytes int64 `json:"json_bytes"`
		} `json:"tools"`
		Sections          [][]any   `json:"sections"`
		ToolsetsBreakdown []toolset `json:"toolsets_breakdown"`
	}{
		Platform:          size.GetPlatform(),
		Model:             size.GetModel(),
		SystemPrompt:      sized{size.GetChars(), size.GetBytes()},
		Blocks:            []block{},
		Sections:          [][]any{},
		ToolsetsBreakdown: []toolset{},
	}
	document.Tools.Count, document.Tools.JSONBytes = size.GetTools(), size.GetToolBytes()
	for _, section := range size.GetSections() {
		document.Blocks = append(document.Blocks, block{section.GetName(), section.GetTier(), section.GetChars(), section.GetBytes()})
	}
	for _, tier := range size.GetTiers() {
		document.Sections = append(document.Sections, []any{tiers[tier.GetName()], tier.GetChars(), tier.GetBytes()})
	}
	for _, set := range size.GetToolsets() {
		document.ToolsetsBreakdown = append(document.ToolsetsBreakdown, toolset{set.GetName(), set.GetTools(), set.GetBytes()})
	}
	text, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(text))
	return err
}
