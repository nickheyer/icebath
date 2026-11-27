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
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

const long = "Hooks are your own commands that run when the agent acts, such as before it calls a tool. You set them under " +
	shellconfig.Events + " in the settings file. icebath asks you once before a new hook first runs and keeps your answers in " +
	shellhooks.AllowlistFile + " in the profile's folder. Use these commands to list your hooks, try them on a sample event, take back an " +
	"answer and check them all.\n\n" +
	"Each event holds a list of hooks. command is the command to run, and it gets the event as JSON on its standard input. matcher is a " +
	"pattern of tool names, for pre_tool_call and post_tool_call. timeout is the time limit in seconds, 60 when not set and at most 300. " +
	"With fail_closed, a pre_tool_call hook that fails or gives no answer blocks the call.\n\n" +
	"A hook answers with a JSON object on its standard output. A pre_tool_call hook blocks the call with {\"action\": \"block\", " +
	"\"message\": ...}, changes its arguments with {\"action\": \"modify\", \"args\": {...}} or approves it with {\"action\": " +
	"\"approve\", \"message\": ..., \"rule_key\": ...}. Exit status 2 blocks the call too. A pre_verify hook answers {\"action\": " +
	"\"continue\", \"message\": ...}. pre_llm_call and the other events answer {\"context\": ...}. icebath also posts events to the " +
	"addresses under " + shellconfig.Outbound + ".\n\n" +
	"Where no one can answer, such as in CI, --accept-hooks, ICEBATH_ACCEPT_HOOKS=1 or " + shellconfig.AutoAccept + ": true in the " +
	"settings file allows every new hook without asking."

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := hooksv1connect.NewHooksServiceClient(conn)
	root := &cobra.Command{
		Use:     "hooks",
		GroupID: cli.GroupAbilities,
		Short:   "List, test and check your hooks",
		Long:    long,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) > 0 {
				fmt.Fprintf(out, "icebath hooks has no command “%s”.\n", args[0])
			}
			fmt.Fprintln(out, "Usage: icebath hooks [command]")
			fmt.Fprintln(out, "\nCommands:")
			width := 0
			for _, sub := range cmd.Commands() {
				width = max(width, cli.Width(sub.Name()))
			}
			for _, sub := range cmd.Commands() {
				if sub.IsAvailableCommand() {
					fmt.Fprintf(out, "  %s  %s\n", cli.Pad(sub.Name(), width), sub.Short)
				}
			}
			fmt.Fprintln(out, "\nFor how hooks are written, run icebath hooks --help.")
			return nil
		},
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your hooks and whether each may run",
		Long: "Lists every hook by event with the tools it matches, its time limit and whether you allowed it, then the addresses " +
			"icebath posts events to.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listed(cmd, service) },
	}
	var tool, file string
	test := &cobra.Command{
		Use:   "test <event>",
		Short: "Run the hooks of an event on a sample event",
		Long: "Runs every hook set on the event once, with a made-up event as its input, and shows what each one answers. Use test to try a " +
			"hook before the agent runs it for real.",
		Example: "  icebath hooks test pre_tool_call --for-tool terminal\n  icebath hooks test pre_llm_call --payload-file event.json",
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return tested(cmd, service, args[0], tool, file) },
	}
	test.Flags().StringVar(&tool, "for-tool", "", "Run only the hooks whose matcher matches this tool, for tool events")
	test.Flags().StringVar(&file, "payload-file", "", "JSON file whose fields are added to the sample event")
	revoke := &cobra.Command{
		Use:     "revoke <command>",
		Aliases: []string{"remove", "rm"},
		Short:   "Take back your consent to a hook's command",
		Long: "Removes the command from the hooks you allowed, so icebath asks again before it runs the command. A running icebath stops " +
			"running the command at its next event.",
		Example: "  icebath hooks revoke ~/.config/icebath/hooks/lint.sh",
		Args:    cobra.ExactArgs(1),
		RunE:    func(cmd *cobra.Command, args []string) error { return revoked(cmd, service, args[0]) },
	}
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Check every hook for problems",
		Long: "Checks each hook: that its file may run, that you allowed it, that it has not changed since, that it answers with valid JSON " +
			"and how long a sample run takes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return checked(cmd, service) },
	}
	root.AddCommand(list, test, revoke, doctor)
	return []*cobra.Command{root}
}

// Facts of a hook or webhook after its command or address: the tools it runs for, its time limit and how it stands
func facts(matcher string, timeout int32, standing string) string {
	parts := []string{}
	if matcher != "" {
		parts = append(parts, "Runs for tools that match "+matcher+".")
	}
	parts = append(parts, "Time limit: "+strs.Span(time.Duration(timeout)*time.Second)+".")
	if standing != "" {
		parts = append(parts, standing)
	}
	return strings.Join(parts, " ")
}

// Length of a run in words
func span(seconds float64) string {
	return strs.Span(time.Duration(seconds * float64(time.Second)).Round(time.Millisecond))
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
		fmt.Fprintln(out, "You have no shell hooks or webhooks yet. Add them in /settings under Hooks › Shell hooks, or under "+shellconfig.Events+
			" and "+shellconfig.Outbound+" in the settings file.")
		fmt.Fprintln(out, "For how a hook is written, run icebath hooks --help.")
		return nil
	}
	if len(hooks) == 0 {
		fmt.Fprintln(out, "You have no shell hooks yet. Add them in /settings under Hooks › Shell hooks › Commands, or under "+shellconfig.Events+
			" in the settings file.")
		fmt.Fprintln(out)
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
		fmt.Fprintf(out, "%s:\n\n", strs.Count(len(hooks), "shell hook"))
		for _, event := range events {
			fmt.Fprintf(out, "  On %s\n", event)
			for _, hook := range byEvent[event] {
				standing := "Not allowed yet, so it does not run."
				switch {
				case hook.GetAllowed() && hook.GetApprovedAt() != "":
					standing = "You allowed it on " + hook.GetApprovedAt() + "."
				case hook.GetAllowed():
					standing = "Allowed."
				}
				fmt.Fprintf(out, "    %s\n      %s\n", hook.GetCommand(), facts(hook.GetMatcher(), hook.GetTimeout(), standing))
				if hook.GetApprovedAt() != "" && hook.GetChanged() {
					fmt.Fprintf(out, "      ⚠ The script changed after you allowed it. Its time was %s then and is %s now. Run icebath hooks doctor to "+
						"check it again.\n", hook.GetChangedWas(), hook.GetChangedNow())
				}
			}
			fmt.Fprintln(out)
		}
	}
	if len(webhooks) > 0 {
		fmt.Fprintf(out, "%s:\n\n", strs.Count(len(webhooks), "webhook"))
		for _, target := range webhooks {
			signed := "Not signed."
			if target.GetSigned() {
				signed = "Signed."
			}
			fmt.Fprintf(out, "  %s\n", target.GetLabel())
			fmt.Fprintf(out, "      Address: %s\n", target.GetUrl())
			fmt.Fprintf(out, "      Events: %s. %s\n", strings.Join(target.GetEvents(), ", "), facts(target.GetMatcher(), target.GetTimeout(), signed))
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
		fmt.Fprintf(out, "✗ icebath has no event “%s”.\n", event)
		fmt.Fprintf(out, "The events are %s.\n", strings.Join(listing.GetPoints(), ", "))
		return nil
	}
	payload := ""
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(out, "✗ The sample event file can't be read: %s.\n", err)
			return nil
		}
		data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
		if !utf8.Valid(data) {
			fmt.Fprintf(out, "✗ The sample event file %s can't be read, because it is not text.\n", file)
			return nil
		}
		var custom any
		if err := json.Unmarshal(data, &custom); err != nil {
			fmt.Fprintf(out, "✗ The sample event file %s is not valid JSON: %s.\n", file, err)
			return nil
		}
		if object, ok := custom.(map[string]any); ok {
			encoded, err := json.Marshal(object)
			if err != nil {
				return err
			}
			payload = string(encoded)
		} else {
			fmt.Fprintf(out, "⚠ %s does not hold a JSON object, so the made-up event is used as it is.\n", file)
		}
	}
	response, err := service.Test(cmd.Context(), &hooksv1.TestRequest{Event: event, Tool: tool, Payload: payload})
	if err != nil {
		return err
	}
	runs := response.GetRuns()
	if len(runs) == 0 {
		if tool != "" {
			fmt.Fprintf(out, "No shell hook runs on %s for the tool %s.\n", event, tool)
		} else {
			fmt.Fprintf(out, "No shell hook runs on %s.\n", event)
		}
		return nil
	}
	fmt.Fprintf(out, "Running %s on a made-up %s event:\n\n", strs.Count(len(runs), "hook"), event)
	for _, tested := range runs {
		fmt.Fprintf(out, "  %s\n", tested.GetCommand())
		printRun(out, tested.GetRun())
		fmt.Fprintln(out)
	}
	return nil
}

// How a run ended and how long it took
func ended(run *hooksv1.Run) string {
	if !run.GetExited() {
		return "It was stopped after " + span(run.GetSeconds()) + ", with no exit code."
	}
	return fmt.Sprintf("It ended with exit code %d after %s.", run.GetExitCode(), span(run.GetSeconds()))
}

// Prints how one run went and the directive it answered with, even after an error or a timeout, so failing closed and open differ
func printRun(out io.Writer, run *hooksv1.Run) {
	switch {
	case run.GetError() != "":
		fmt.Fprintf(out, "      ✗ %s\n", sentence(run.GetError()))
	case run.GetTimedOut():
		fmt.Fprintf(out, "      ✗ It did not finish within %s.\n", span(run.GetSeconds()))
	default:
		fmt.Fprintf(out, "      %s\n", ended(run))
		for _, stream := range []struct{ name, text string }{{"Output", run.GetStdout()}, {"Errors", run.GetStderr()}} {
			if text := strings.TrimSpace(stream.text); text != "" {
				fmt.Fprintf(out, "      %s: %s\n", stream.name, truncate(text, 400))
			}
		}
	}
	if run.GetDirective() != "" {
		fmt.Fprintf(out, "      icebath reads its answer as %s\n", run.GetDirective())
	} else {
		fmt.Fprintln(out, "      icebath reads no answer from it, so it changes nothing.")
	}
}

// Text cut to n characters with an ellipsis when longer
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// Text that ends as a sentence: trimmed, with a period added when it ends in no mark of its own
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasSuffix(text, ".") || strings.HasSuffix(text, "!") || strings.HasSuffix(text, "?") {
		return text
	}
	return text + "."
}

// Takes every allowlist entry of the command off and says how soon running icebaths stop running it
func revoked(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, command string) error {
	response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Verb: "revoke", Command: command})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if response.GetRemoved() == 0 {
		fmt.Fprintf(out, "You had not allowed %s, so there was nothing to take back.\n", command)
		return nil
	}
	fmt.Fprintf(out, "✓ Removed %s that allowed %s.\n", strs.Count(int(response.GetRemoved()), "entry"), command)
	fmt.Fprintln(out, "A running icebath stops running it at its next event, unless --accept-hooks, ICEBATH_ACCEPT_HOOKS or Run without asking in "+
		"/settings under Hooks › Shell hooks allows every hook there.")
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
		fmt.Fprintln(out, "You have no shell hooks, so there is nothing to check. Add them in /settings under Hooks › Shell hooks › Commands.")
		return nil
	}
	fmt.Fprintf(out, "Checking %s…\n\n", strs.Count(len(checks), "shell hook"))
	problems := 0
	for _, check := range checks {
		fmt.Fprintf(out, "  %s, on %s\n", check.GetCommand(), check.GetEvent())
		doctorOne(out, check)
		problems += int(check.GetProblems())
		fmt.Fprintln(out)
	}
	if problems > 0 {
		fmt.Fprintf(out, "✗ Found %s. Fix them before you rely on these hooks.\n", strs.Count(problems, "problem"))
	} else {
		fmt.Fprintln(out, "✓ Every shell hook works.")
	}
	return nil
}

// Prints what checking one hook found, the smoke run kept for an allowed hook whose script can run
func doctorOne(out io.Writer, check *hooksv1.Checked) {
	if check.GetRunnable() {
		fmt.Fprintln(out, "      ✓ The script exists and may run.")
	} else {
		fmt.Fprintln(out, "      ✗ The script is missing or may not run. Fix the path, or make the file runnable with chmod +x.")
	}
	switch {
	case check.GetAllowed() && check.GetApprovedAt() != "":
		fmt.Fprintf(out, "      ✓ You allowed it on %s.\n", check.GetApprovedAt())
	case check.GetAllowed():
		fmt.Fprintln(out, "      ✓ You allowed it.")
	default:
		fmt.Fprintln(out, "      ✗ You have not allowed it, so it does not run. Allow it when icebath asks, or start icebath once with --accept-hooks.")
	}
	switch {
	case check.GetCompared() && check.GetChanged():
		fmt.Fprintf(out, "      ⚠ The script changed after you allowed it. Its time was %s then and is %s now. Check the change, then run icebath "+
			"hooks revoke and allow it again.\n", check.GetChangedWas(), check.GetChangedNow())
	case check.GetCompared():
		fmt.Fprintln(out, "      ✓ The script has not changed since you allowed it.")
	}
	if !check.GetAllowed() {
		fmt.Fprintln(out, "      Its answer was not checked, because you have not allowed it. Allow it first, then run icebath hooks doctor again.")
		return
	}
	run := check.GetRun()
	if run == nil {
		return
	}
	switch {
	case run.GetTimedOut():
		fmt.Fprintf(out, "      ✗ It did not finish a made-up event within %s. Its time limit is %s.\n", span(run.GetSeconds()),
			strs.Span(time.Duration(check.GetTimeout())*time.Second))
	case run.GetError() != "":
		fmt.Fprintf(out, "      ✗ It could not run. %s\n", sentence(run.GetError()))
	case check.GetOutput() == "empty":
		fmt.Fprintf(out, "      ✓ It printed nothing, so it only watches and changes nothing. %s\n", ended(run))
	case check.GetOutput() == "json":
		fmt.Fprintf(out, "      ✓ It answered a made-up event with valid JSON. %s\n", ended(run))
	default:
		fmt.Fprintf(out, "      ✗ Its answer is not valid JSON. %s It printed: %s\n", ended(run), truncate(strings.TrimSpace(run.GetStdout()), 120))
	}
}
