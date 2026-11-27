// The hooks commands: the shell hooks and outbound webhooks of the profile, running hooks on sample events, taking a hook off the allowlist,
// and checking every hook
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

const long = "Inspect the shell-script hooks declared under " + shellconfig.Events + " in the profile's config.yaml, test them against synthetic payloads, " +
	"and manage the first-use consent allowlist, " + shellhooks.AllowlistFile + " in the profile's folder.\n\n" +
	"Each event of " + shellconfig.Events + " holds a list of hooks: command (run with the event as JSON on its standard input), matcher (a pattern " +
	"of tool names, for pre_tool_call and post_tool_call), timeout (seconds, at most 300, 60 when unset) and fail_closed (pre_tool_call only: a " +
	"hook that fails or gives no answer blocks the call). A hook answers with a JSON object on its standard output: {\"action\": \"block\", " +
	"\"message\": ...}, {\"action\": \"modify\", \"args\": {...}} or {\"action\": \"approve\", \"message\": ..., \"rule_key\": ...} for pre_tool_call " +
	"(exit code 2 blocks too), {\"action\": \"continue\", \"message\": ...} for pre_verify, {\"context\": ...} for pre_llm_call and the others. " +
	shellconfig.Outbound + " lists addresses events are posted to."

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := hooksv1connect.NewHooksServiceClient(conn)
	root := &cobra.Command{
		Use:   "hooks",
		Short: "Inspect and manage shell-script hooks",
		Long:  long,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) > 0 {
				fmt.Fprintf(out, "Unknown hooks subcommand: %s\n", args[0])
				return nil
			}
			fmt.Fprintln(out, "Usage: icebath hooks {list|test|revoke|doctor}")
			fmt.Fprintln(out, "Run 'icebath hooks --help' for details.")
			return nil
		},
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured hooks with matcher, timeout, and consent status",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listed(cmd, service) },
	}
	var tool, file string
	test := &cobra.Command{
		Use:   "test <event>",
		Short: "Fire every hook matching <event> against a synthetic payload",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return tested(cmd, service, args[0], tool, file) },
	}
	test.Flags().StringVar(&tool, "for-tool", "", "Only fire hooks whose matcher matches this tool name (used for pre_tool_call / post_tool_call)")
	test.Flags().StringVar(&file, "payload-file", "", "Path to a JSON file whose contents are merged into the synthetic payload before execution")
	revoke := &cobra.Command{
		Use:     "revoke <command>",
		Aliases: []string{"remove", "rm"},
		Short:   "Remove a command's allowlist entries (a running icebath stops running it at its next event)",
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return revoked(cmd, service, args[0]) },
	}
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Check each configured hook: exec bit, allowlist, mtime drift, JSON validity, and synthetic run timing",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return checked(cmd, service) },
	}
	root.AddCommand(list, test, revoke, doctor)
	return []*cobra.Command{root}
}

// Matcher part of a line of the list, empty without a matcher
func matcherPart(matcher string) string {
	if matcher == "" {
		return ""
	}
	return " matcher='" + matcher + "'"
}

// Prints the configured shell hooks by event with their consent and drift, then the outbound webhooks
func listed(cmd *cobra.Command, service hooksv1connect.HooksServiceClient) error {
	response, err := service.List(cmd.Context(), &hooksv1.ListRequest{})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	hooks, webhooks := response.GetHooks(), response.GetWebhooks()
	if len(hooks) == 0 && len(webhooks) == 0 {
		fmt.Fprintf(out, "No shell hooks or outbound webhooks configured in the profile's config.yaml (%s, %s).\n", shellconfig.Events, shellconfig.Outbound)
		fmt.Fprintln(out, "See `icebath hooks --help` for the config schema.")
		return nil
	}
	if len(hooks) == 0 {
		fmt.Fprintf(out, "No shell hooks configured in the profile's config.yaml (%s).\n", shellconfig.Events)
	} else {
		byEvent := map[string][]*hooksv1.Hook{}
		var events []string
		for _, hook := range hooks {
			if _, seen := byEvent[hook.GetEvent()]; !seen {
				events = append(events, hook.GetEvent())
			}
			byEvent[hook.GetEvent()] = append(byEvent[hook.GetEvent()], hook)
		}
		slices.Sort(events)
		fmt.Fprintf(out, "Configured shell hooks (%d total):\n\n", len(hooks))
		for _, event := range events {
			fmt.Fprintf(out, "  [%s]\n", event)
			for _, hook := range byEvent[event] {
				status := "✗ not allowlisted"
				if hook.GetAllowed() {
					status = "✓ allowed"
				}
				fmt.Fprintf(out, "    - %s%s (timeout=%ds, %s)\n", hook.GetCommand(), matcherPart(hook.GetMatcher()), hook.GetTimeout(), status)
				if hook.GetApprovedAt() != "" {
					fmt.Fprintf(out, "      approved_at: %s\n", hook.GetApprovedAt())
					if hook.GetChanged() {
						fmt.Fprintf(out, "      ⚠ script modified since approval (was %s, now %s) — run `icebath hooks doctor` to re-validate\n", hook.GetChangedWas(),
							hook.GetChangedNow())
					}
				}
			}
			fmt.Fprintln(out)
		}
	}
	if len(webhooks) > 0 {
		fmt.Fprintf(out, "Configured outbound webhooks (%d total):\n\n", len(webhooks))
		for _, target := range webhooks {
			signed := "UNSIGNED"
			if target.GetSigned() {
				signed = "signed"
			}
			fmt.Fprintf(out, "  - %s\n", target.GetLabel())
			fmt.Fprintf(out, "      url:     %s\n", target.GetUrl())
			fmt.Fprintf(out, "      events:  %s%s (timeout=%ds, %s)\n", strings.Join(target.GetEvents(), ", "), matcherPart(target.GetMatcher()), target.GetTimeout(),
				signed)
		}
		fmt.Fprintln(out)
	}
	return nil
}

// Fires every hook of the event once on its synthetic payload, the payload file's fields over it, and prints how each run went
func tested(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, event, tool, file string) error {
	out := cmd.OutOrStdout()
	listing, err := service.List(cmd.Context(), &hooksv1.ListRequest{})
	if err != nil {
		return err
	}
	if !slices.Contains(listing.GetPoints(), event) {
		fmt.Fprintf(out, "Unknown event: '%s'\n", event)
		fmt.Fprintf(out, "Valid events: %s\n", strings.Join(listing.GetPoints(), ", "))
		return nil
	}
	payload := ""
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(out, "Error reading payload file: %s\n", err)
			return nil
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(data) {
			fmt.Fprintf(out, "Error reading payload file: %s is not UTF-8 text\n", file)
			return nil
		}
		var custom any
		if err := json.Unmarshal(data, &custom); err != nil {
			fmt.Fprintf(out, "Error reading payload file: %s\n", err)
			return nil
		}
		if object, ok := custom.(map[string]any); ok {
			encoded, err := json.Marshal(object)
			if err != nil {
				return err
			}
			payload = string(encoded)
		} else {
			fmt.Fprintf(out, "Warning: %s is not a JSON object; ignoring\n", file)
		}
	}
	response, err := service.Test(cmd.Context(), &hooksv1.TestRequest{Event: event, Tool: tool, Payload: payload})
	if err != nil {
		return err
	}
	runs := response.GetRuns()
	if len(runs) == 0 {
		fmt.Fprintf(out, "No shell hooks configured for event: %s\n", event)
		if tool != "" {
			fmt.Fprintf(out, "(with matcher filter --for-tool=%s)\n", tool)
		}
		return nil
	}
	fmt.Fprintf(out, "Firing %d hook(s) for event '%s':\n\n", len(runs), event)
	for _, tested := range runs {
		fmt.Fprintf(out, "  → %s\n", tested.GetCommand())
		printRun(out, tested.GetRun())
		fmt.Fprintln(out)
	}
	return nil
}

// Seconds as the commands print them, as short as they write
func seconds(s float64) string { return strconv.FormatFloat(s, 'f', -1, 64) }

// Exit code of a run, None for one that did not run to its end
func exitCode(run *hooksv1.Run) string {
	if !run.GetExited() {
		return "None"
	}
	return strconv.Itoa(int(run.GetExitCode()))
}

// Prints how one run went and the directive it answered with, even after an error or a timeout, so failing closed and open differ
func printRun(out io.Writer, run *hooksv1.Run) {
	switch {
	case run.GetError() != "":
		fmt.Fprintf(out, "      ✗ error: %s\n", run.GetError())
	case run.GetTimedOut():
		fmt.Fprintf(out, "      ✗ timed out after %ss\n", seconds(run.GetSeconds()))
	default:
		fmt.Fprintf(out, "      exit=%s  elapsed=%ss\n", exitCode(run), seconds(run.GetSeconds()))
		for _, stream := range []struct{ name, text string }{{"stdout", run.GetStdout()}, {"stderr", run.GetStderr()}} {
			if text := strings.TrimSpace(stream.text); text != "" {
				fmt.Fprintf(out, "      %s: %s\n", stream.name, truncate(text, 400))
			}
		}
	}
	if run.GetDirective() != "" {
		fmt.Fprintf(out, "      parsed (Icebath wire shape): %s\n", run.GetDirective())
	} else {
		fmt.Fprintln(out, "      parsed: <none — hook contributed nothing to the dispatcher>")
	}
}

// Text cut to n characters with an ellipsis when longer
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// Takes every allowlist entry of the command off and says how soon running icebaths stop running it
func revoked(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, command string) error {
	response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Verb: "revoke", Command: command})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if response.GetRemoved() == 0 {
		fmt.Fprintf(out, "No allowlist entry found for command: %s\n", command)
		return nil
	}
	fmt.Fprintf(out, "Removed %d allowlist entry/entries for: %s\n", response.GetRemoved(), command)
	fmt.Fprintf(out, "Note: a running icebath stops running it at its next event (unless --accept-hooks / ICEBATH_ACCEPT_HOOKS / %s approves every hook "+
		"there).\n", shellconfig.AutoAccept)
	return nil
}

// Checks every configured hook for its script, consent, drift and answer, and counts the problems
func checked(cmd *cobra.Command, service hooksv1connect.HooksServiceClient) error {
	response, err := service.Check(cmd.Context(), &hooksv1.CheckRequest{})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	checks := response.GetChecks()
	if len(checks) == 0 {
		fmt.Fprintln(out, "No shell hooks configured — nothing to check.")
		return nil
	}
	fmt.Fprintf(out, "Checking %d configured shell hook(s)...\n\n", len(checks))
	problems := 0
	for _, check := range checks {
		fmt.Fprintf(out, "  [%s] %s\n", check.GetEvent(), check.GetCommand())
		doctorOne(out, check)
		problems += int(check.GetProblems())
		fmt.Fprintln(out)
	}
	if problems > 0 {
		fmt.Fprintf(out, "%d issue(s) found.  Fix before relying on these hooks.\n", problems)
	} else {
		fmt.Fprintln(out, "All shell hooks look healthy.")
	}
	return nil
}

// Prints what checking one hook found, the smoke run kept for an allowed hook whose script can run
func doctorOne(out io.Writer, check *hooksv1.Checked) {
	if check.GetRunnable() {
		fmt.Fprintln(out, "      ✓ script exists and is executable")
	} else {
		fmt.Fprintln(out, "      ✗ script missing or not executable (chmod +x the file, or fix the path)")
	}
	if check.GetAllowed() {
		fmt.Fprintf(out, "      ✓ allowlisted (approved %s)\n", check.GetApprovedAt())
	} else {
		fmt.Fprintln(out, "      ✗ not allowlisted — hook will NOT fire at runtime (run with --accept-hooks once, or confirm at the TTY prompt)")
	}
	switch {
	case check.GetCompared() && check.GetChanged():
		fmt.Fprintf(out, "      ⚠ script modified since approval (was %s, now %s) — review changes, then `icebath hooks revoke` + re-approve to refresh\n",
			check.GetChangedWas(), check.GetChangedNow())
	case check.GetCompared():
		fmt.Fprintln(out, "      ✓ script unchanged since approval")
	}
	if !check.GetAllowed() {
		fmt.Fprintln(out, "      ℹ skipped JSON smoke test — not allowlisted yet. Approve the hook first (via TTY prompt or --accept-hooks), then re-run "+
			"`icebath hooks doctor`.")
		return
	}
	run := check.GetRun()
	if run == nil {
		return
	}
	elapsed, code := seconds(run.GetSeconds()), exitCode(run)
	switch {
	case run.GetTimedOut():
		fmt.Fprintf(out, "      ✗ timed out after %ss on synthetic payload (timeout=%ds)\n", elapsed, check.GetTimeout())
	case run.GetError() != "":
		fmt.Fprintf(out, "      ✗ execution error: %s\n", run.GetError())
	case check.GetOutput() == "empty":
		fmt.Fprintf(out, "      ✓ ran clean with empty stdout (exit=%s, %ss) — hook is observer-only\n", code, elapsed)
	case check.GetOutput() == "json":
		fmt.Fprintf(out, "      ✓ produced valid JSON on synthetic payload (exit=%s, %ss)\n", code, elapsed)
	default:
		fmt.Fprintf(out, "      ✗ stdout was not valid JSON (exit=%s, %ss): %s\n", code, elapsed, truncate(strings.TrimSpace(run.GetStdout()), 120))
	}
}
