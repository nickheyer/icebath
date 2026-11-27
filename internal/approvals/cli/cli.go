package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	approvalsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/approvals/v1/approvalsv1connect"
)

// Exit code a command ends with, which main hands to the shell without printing anything
type exit int

func (e exit) Error() string { return "" }
func (e exit) ExitCode() int { return int(e) }

const usage = "usage: icebath approvals <subcommand>\n\nsubcommands:\n  suggest    Mine past approval decisions into a proposed\n" +
	"             command_allowlist (dry by default; --apply N,M to merge)\n  test       Dry-run the approval verdict for a command without\n" +
	"             executing it (exit 0 allow / 2 ask / 3 deny)\n\nRun `icebath approvals <subcommand> -h` for details.\n"

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := approvalsv1connect.NewApprovalsServiceClient(conn)
	root := &cobra.Command{
		Use:   "approvals",
		Short: "Approval-prompt tools (mine history into allowlist proposals)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.OutOrStdout(), usage)
			return exit(1)
		},
	}
	root.AddCommand(suggest(service), test(service))
	return []*cobra.Command{root}
}

func suggest(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	var apply, store string
	var asJSON bool
	var days, least, limit int
	cmd := &cobra.Command{
		Use:   "suggest",
		Short: "Propose command_allowlist entries from past approvals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			d := int32(days)
			response, err := service.Suggest(cmd.Context(), &approvalsv1.SuggestRequest{Days: &d, MinCount: int32(least), Limit: int32(limit), Store: store})
			if err != nil {
				if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeNotFound {
					fmt.Fprintf(out, "Session store not found: %s\n", failure.Message())
					return exit(1)
				}
				return err
			}
			proposals := response.GetProposals()
			if apply != "" {
				indices, err := selection(apply, len(proposals))
				if err != nil {
					fmt.Fprintf(out, "--apply error: %s\n", err)
					return exit(1)
				}
				var patterns []string
				for _, i := range indices {
					patterns = append(patterns, proposals[i].GetPattern())
				}
				allowed, err := service.Allow(cmd.Context(), &approvalsv1.AllowRequest{Patterns: patterns})
				if err != nil {
					return err
				}
				if asJSON {
					fmt.Fprintf(out, "{\"applied\": %s, \"allowlist_size\": %d}\n", spacedList(patterns), allowed.GetSize())
					return nil
				}
				fmt.Fprintln(out, "Added to command_allowlist:")
				for _, pattern := range patterns {
					fmt.Fprintf(out, "  + %s\n", pattern)
				}
				fmt.Fprintf(out, "\ncommand_allowlist now has %d entries (approvals.allowlist of this profile).\n", allowed.GetSize())
				return nil
			}
			if asJSON {
				return listJSON(out, store, int(response.GetDays()), proposals)
			}
			render(out, proposals, int(response.GetDays()))
			return nil
		},
	}
	cmd.Flags().StringVar(&apply, "apply", "", "merge the numbered proposals into the allowlist, such as 1,3")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().IntVar(&days, "days", 90, "days of history to read (0 for all of it)")
	cmd.Flags().IntVar(&least, "min-count", 2, "approvals a pattern needs before it is proposed")
	cmd.Flags().IntVar(&limit, "limit", 20, "most proposals to show")
	cmd.Flags().StringVar(&store, "db", "", "session store to read, by name (default: this profile's)")
	return cmd
}

// Numbers like 1,3 as indexes counted from 0, each in range and once
func selection(spec string, total int) ([]int, error) {
	var indices []int
	for _, part := range strings.Split(spec, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid selection '%s' — expected numbers like 1,3", part)
		}
		if n < 1 || n > total {
			return nil, fmt.Errorf("selection %d out of range (1..%d)", n, total)
		}
		if !slices.Contains(indices, n-1) {
			indices = append(indices, n-1)
		}
	}
	if len(indices) == 0 {
		return nil, errors.New("no valid selections in --apply")
	}
	return indices, nil
}

func window(days int) string {
	if days > 0 {
		return fmt.Sprintf("last %d days", days)
	}
	return "all history"
}

func render(out io.Writer, proposals []*approvalsv1.Proposal, days int) {
	if len(proposals) == 0 {
		fmt.Fprintf(out, "No allowlist candidates found in approval history (%s).\nEither nothing dangerous was approved often enough (see "+
			"--min-count/--days), or the approved classes are excluded for safety.\n", window(days))
		return
	}
	fmt.Fprintf(out, "Proposed command_allowlist additions (from approval history, %s):\n\n", window(days))
	for i, p := range proposals {
		kind := ""
		if p.GetKind() == "class" {
			kind = " (class key)"
		}
		fmt.Fprintf(out, "  %d. %s    — approved %dx%s\n", i+1, p.GetPattern(), p.GetCount(), kind)
		for _, class := range p.GetClasses() {
			fmt.Fprintf(out, "       class: %s\n", class)
		}
		for _, example := range p.GetExamples() {
			fmt.Fprintf(out, "       e.g. %s\n", example)
		}
	}
	fmt.Fprint(out, "\nNothing has been changed. Apply selected entries with:\n  icebath approvals suggest --apply 1,3\nEntries are merged into "+
		"approvals.allowlist of this profile.\n")
}

func listJSON(out io.Writer, store string, days int, proposals []*approvalsv1.Proposal) error {
	type row struct {
		N        int      `json:"n"`
		Pattern  string   `json:"pattern"`
		Kind     string   `json:"kind"`
		Count    int32    `json:"count"`
		Classes  []string `json:"classes"`
		Examples []string `json:"examples"`
	}
	rows := []row{}
	for i, p := range proposals {
		rows = append(rows, row{i + 1, p.GetPattern(), p.GetKind(), p.GetCount(), nonNil(p.GetClasses()), nonNil(p.GetExamples())})
	}
	return printJSON(out, struct {
		DB        string `json:"db"`
		Days      int    `json:"days"`
		Proposals []row  `json:"proposals"`
	}{store, days, rows})
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// JSON indented by two with every character past ASCII escaped
func printJSON(out io.Writer, value any) error {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	_, err := io.WriteString(out, ascii(b.String()))
	return err
}

func ascii(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// List as JSON with a space after each comma
func spacedList(items []string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		encoded, _ := json.Marshal(item)
		parts[i] = ascii(string(encoded))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func test(service approvalsv1connect.ApprovalsServiceClient) *cobra.Command {
	var backend string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "test [--env-type TYPE] [--json] -- <command...>",
		Short: "Dry-run the approval verdict for a command (never executes it)",
		RunE: func(cmd *cobra.Command, words []string) error {
			out := cmd.OutOrStdout()
			if len(words) > 0 && words[0] == "--" {
				words = words[1:]
			}
			if len(words) == 0 {
				fmt.Fprintln(out, "usage: icebath approvals test [--env-type TYPE] [--json] -- <command...>")
				return exit(1)
			}
			v, err := service.Test(cmd.Context(), &approvalsv1.TestRequest{Command: strings.Join(words, " "), Backend: backend})
			if err != nil {
				return err
			}
			if asJSON {
				var rule, detail *string
				if v.GetRule() != "" {
					rule = &v.Rule
				}
				if v.GetDetail() != "" {
					detail = &v.Detail
				}
				if err := printJSON(out, struct {
					Command  string   `json:"command"`
					EnvType  string   `json:"env_type"`
					Verdict  string   `json:"verdict"`
					ExitCode int32    `json:"exit_code"`
					Rule     *string  `json:"rule"`
					Detail   *string  `json:"detail"`
					Variants []string `json:"normalized_variants"`
				}{v.GetCommand(), v.GetBackend(), v.GetVerdict(), v.GetExitCode(), rule, detail, nonNil(v.GetVariants())}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "command : %s\nenv-type: %s\nverdict : %s  (exit %d)\n", v.GetCommand(), v.GetBackend(), v.GetVerdict(), v.GetExitCode())
				if v.GetRule() != "" {
					fmt.Fprintf(out, "rule    : %s\n", v.GetRule())
				}
				if v.GetDetail() != "" {
					fmt.Fprintf(out, "detail  : %s\n", v.GetDetail())
				}
				fmt.Fprintln(out, "normalized trace (variants the detectors evaluated):")
				for _, variant := range v.GetVariants() {
					fmt.Fprintf(out, "  - %s\n", variant)
				}
			}
			if v.GetExitCode() != 0 {
				return exit(v.GetExitCode())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&backend, "env-type", "", "terminal backend the command would run on (default: this profile's)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
