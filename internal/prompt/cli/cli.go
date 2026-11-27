package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
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
		Use:   "prompt-size",
		Short: "Show a byte breakdown of the system prompt and tool schemas",
		Args:  cobra.NoArgs,
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
			_, err = fmt.Fprintln(out, "Could not compute prompt-size breakdown: "+tui.Reason(err))
			return err
		},
	}
	size.Flags().StringVar(&platform, "platform", "tui", "surface whose prompt to measure, such as tui, cli or cron")
	size.Flags().BoolVar(&sizeJSON, "json", false, "print the breakdown as JSON")
	return []*cobra.Command{size}
}

// Byte count right aligned in eight columns with thousands separators
func grouped(n int64) string {
	return fmt.Sprintf("%8s", strs.Thousands(n))
}

func kilobytes(n int64) string {
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
}

func breakdown(size *promptv1.SizeResponse) string {
	model := size.GetModel()
	if model == "" {
		model = "unset"
	}
	lines := []string{
		"Prompt-size breakdown (platform=" + size.GetPlatform() + ", model=" + model + ")", "",
		"  System prompt total : " + grouped(size.GetBytes()) + " B  (" + kilobytes(size.GetBytes()) + ", " + strs.Thousands(size.GetChars()) + " chars)",
	}
	if sections := size.GetSections(); len(sections) > 0 {
		lines = append(lines, "", "  Sections:")
		for _, section := range sections {
			lines = append(lines, "    "+fmt.Sprintf("%-34s", section.GetName()+" ("+section.GetTier()+")")+": "+grouped(section.GetBytes())+" B  ("+kilobytes(section.GetBytes())+")")
		}
	}
	lines = append(lines, "", "  Prompt tiers:")
	for _, tier := range size.GetTiers() {
		lines = append(lines, "    "+fmt.Sprintf("%-34s", tiers[tier.GetName()])+": "+grouped(tier.GetBytes())+" B  ("+kilobytes(tier.GetBytes())+")")
	}
	lines = append(lines, "", "  Tool schemas         : "+grouped(size.GetToolBytes())+" B  ("+kilobytes(size.GetToolBytes())+", "+strconv.FormatInt(size.GetTools(), 10)+" tools)")
	if toolsets := size.GetToolsets(); len(toolsets) > 0 {
		lines = append(lines, "", "  Toolsets by size (tool-schema JSON, largest first):",
			"    "+fmt.Sprintf("%-22s", "toolset")+" "+fmt.Sprintf("%5s", "tools")+"  "+fmt.Sprintf("%10s", "schema"))
		for _, toolset := range toolsets {
			lines = append(lines, "    "+fmt.Sprintf("%-22s", toolset.GetName())+" "+fmt.Sprintf("%5d", toolset.GetTools())+"  "+grouped(toolset.GetBytes())+" B  ("+kilobytes(toolset.GetBytes())+")")
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
