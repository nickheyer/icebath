// The hooks commands: the shell hooks and webhooks of the profile, adding, changing, allowing and removing them, running hooks on sample
// events and checking every hook, each through the same function as the page of Hooks
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	shellhooks "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/hooks"
	picked "github.com/nickheyer/icebath/internal/profile"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

const long = "Hooks are your own commands that run when the agent acts, such as before it calls a tool. icebath asks you once before a new " +
	"hook first runs and keeps your answers in " + shellhooks.AllowlistFile + " in the profile's folder. A hook you add with icebath hooks add " +
	"runs once you allow it.\n\n" +
	"A hook's command gets the event as JSON on its standard input. Its tools are a pattern of tool names, for the events before and after " +
	"a tool runs. Its time limit is 60 seconds when not set and at most 5 minutes. A hook that blocks when it fails stops the tool call " +
	"when it fails or gives no answer.\n\n" +
	"A hook answers with a JSON object on its standard output. Before a tool runs, a hook blocks the call with {\"action\": \"block\", " +
	"\"message\": ...}, changes its arguments with {\"action\": \"modify\", \"args\": {...}} or approves it with {\"action\": " +
	"\"approve\", \"message\": ..., \"rule_key\": ...}. Exit status 2 blocks the call too. When code changes are checked, a hook answers " +
	"{\"action\": \"continue\", \"message\": ...}. Other events take {\"context\": ...}.\n\n" +
	"A webhook is a web address icebath posts events to, signed with a secret when it has one.\n\n" +
	"Where no one can answer, such as in CI, --accept-hooks or ICEBATH_ACCEPT_HOOKS=1 allows every new hook without asking, and so does " +
	shellconfig.AutoAccept + ": true in the settings file."

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	service := hooksv1connect.NewHooksServiceClient(conn)
	root := &cobra.Command{
		Use:     "hooks",
		GroupID: cli.GroupAbilities,
		Short:   "Add, test and check your hooks and webhooks",
		Long:    long,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) > 0 {
				fmt.Fprintf(out, "icebath hooks has no command “%s”.\n", args[0])
			}
			usage(out, cmd)
			fmt.Fprintln(out, "\nFor how hooks are written, run icebath hooks --help.")
			return nil
		},
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List your hooks and webhooks and how each stands",
		Long:    "Lists every hook by event with the tools it runs for, its time limit and whether it is allowed, then every webhook.",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listed(cmd, service) },
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
	test.Flags().StringVar(&tool, "for-tool", "", "Run only the hooks whose tools take in this tool, for tool events")
	test.Flags().StringVar(&file, "payload-file", "", "JSON file whose fields are added to the sample event")
	revoke := &cobra.Command{
		Use:   "revoke <command>",
		Short: "Stop allowing a hook's command",
		Long: "Takes the command off the hooks you allowed, so it no longer runs and icebath asks again before it does. The hook stays in " +
			"your settings.",
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
	root.AddCommand(list, add(service), edit(service), remove(service), allow(service), revoke, test, doctor, webhooks(service))
	return []*cobra.Command{root}
}

// Prints the usage of a command and its subcommands
func usage(out io.Writer, cmd *cobra.Command) {
	fmt.Fprintf(out, "Usage: %s [command]\n", cmd.CommandPath())
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
}

// Error of a change the server refused, as its message says
func failed(cmd *cobra.Command, err error) error {
	if failure, ok := errors.AsType[*connect.Error](err); ok {
		fmt.Fprintf(cmd.OutOrStdout(), "✗ %s\n", failure.Message())
		return cli.Exit(1)
	}
	return err
}

// Events a hook may be set on, as a person reads them, for a message
func eventList(listing *hooksv1.ListResponse) string {
	names := slices.Clone(listing.GetEvents())
	slices.SortStableFunc(names, func(a, b string) int { return shellconfig.Place(a) - shellconfig.Place(b) })
	return strings.Join(names, ", ")
}

func add(service hooksv1connect.HooksServiceClient) *cobra.Command {
	var tools string
	var limit int
	var block bool
	command := &cobra.Command{
		Use:   "add <event> <command>",
		Short: "Add a hook, then allow it",
		Long: "Adds a hook that runs the command on the event, then asks whether to allow it. A hook runs only once it is allowed, and runs " +
			"with your full rights.",
		Example: "  icebath hooks add pre_tool_call ~/hooks/check.sh --tools terminal\n  icebath hooks add session_start \"notify-send 'Session started'\"",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Verb: "add", Event: args[0], Command: args[1], Matcher: tools,
				Timeout: int32(limit), FailClosed: block})
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			if response.GetHooks().GetAcceptAll() {
				return nil
			}
			return consent(cmd, service, args[0], strings.TrimSpace(args[1]))
		},
	}
	command.Flags().StringVar(&tools, "tools", "", "Pattern of the tools it runs for, such as terminal|write_file")
	command.Flags().IntVar(&limit, "time-limit", 0, "Seconds it may run, 60 when not given")
	command.Flags().BoolVar(&block, "block-when-fails", false, "Block the tool call when it fails or runs out of time")
	return command
}

// A hook's command as one word of a command line that a shell hands over as it is, its ~ and its $ too, so the line names the hook as the
// settings keep it
func quoted(command string) string {
	return "'" + strings.ReplaceAll(command, "'", `'\''`) + "'"
}

// Asks at the terminal whether to allow a hook, as a launch asks; with no terminal to ask in, says how to allow it later
func consent(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, event, command string) error {
	c := cli.NewConsole(cmd)
	if !c.Interactive() {
		fmt.Fprintf(c.Out(), "To allow it, run icebath hooks allow %s %s in a terminal.\n", event, quoted(command))
		return nil
	}
	fmt.Fprintf(c.Out(), "\n%s runs with all the rights of your user account each time its event happens. Allow only commands you trust.\n", command)
	if !c.Confirm("Allow "+command+" to run on "+event+" from now on?", false) {
		fmt.Fprintln(c.Out(), "It stays in your settings and does not run until you allow it.")
		return nil
	}
	response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Verb: "allow", Event: event, Command: command})
	if err != nil {
		return failed(cmd, err)
	}
	fmt.Fprintf(c.Out(), "✓ %s\n", response.GetSaid())
	return nil
}

func edit(service hooksv1connect.HooksServiceClient) *cobra.Command {
	var to, tools string
	var limit int
	var block bool
	command := &cobra.Command{
		Use:   "edit <event> <command>",
		Short: "Change the command, tools, time limit or blocking of a hook",
		Long: "Changes what the flags give of the hook that runs the command on the event. An empty --tools runs it for every tool, and a " +
			"--time-limit of 0 puts back 60 seconds. A hook you allowed stays allowed with its new command.",
		Example: "  icebath hooks edit pre_tool_call ~/hooks/check.sh --tools 'terminal|write_file' --time-limit 30",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := &hooksv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Verb: "edit", Event: args[0], Command: args[1], To: to,
				Matcher: tools, Timeout: int32(limit), FailClosed: block}
			for flag, field := range map[string]string{"command": shellconfig.Command, "tools": shellconfig.Matcher, "time-limit": shellconfig.Timeout,
				"block-when-fails": shellconfig.Block} {
				if cmd.Flags().Changed(flag) {
					request.Fields = append(request.Fields, field)
				}
			}
			if len(request.Fields) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Say what to change with --command, --tools, --time-limit or --block-when-fails.")
				return cli.Exit(1)
			}
			slices.Sort(request.Fields)
			response, err := service.Change(cmd.Context(), request)
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
	command.Flags().StringVar(&to, "command", "", "The command it runs from now on")
	command.Flags().StringVar(&tools, "tools", "", "Pattern of the tools it runs for, empty for every tool")
	command.Flags().IntVar(&limit, "time-limit", 0, "Seconds it may run, 0 for 60")
	command.Flags().BoolVar(&block, "block-when-fails", false, "Block the tool call when it fails or runs out of time")
	return command
}

func remove(service hooksv1connect.HooksServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <event> <command>",
		Aliases: []string{"rm"},
		Short:   "Remove a hook",
		Long:    "Takes the hook that runs the command on the event out of your settings, with your consent to it. Its script stays on this computer.",
		Example: "  icebath hooks remove pre_tool_call ~/hooks/check.sh",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Verb: "remove", Event: args[0], Command: args[1]})
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
}

func allow(service hooksv1connect.HooksServiceClient) *cobra.Command {
	return &cobra.Command{
		Use:     "allow <event> <command>",
		Short:   "Allow a hook to run",
		Long:    "Asks at the terminal, then lets the hook that runs the command on the event run from now on, with your full rights.",
		Example: "  icebath hooks allow pre_tool_call ~/hooks/check.sh",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			if !c.Interactive() {
				fmt.Fprintln(c.Out(), "✗ Allowing a hook needs a terminal to ask you in. Run icebath hooks allow in your terminal.")
				return cli.Exit(1)
			}
			return consent(cmd, service, args[0], strings.TrimSpace(args[1]))
		},
	}
}

// Webhook the text names: its key, its name or its address; nil with a message when none does
func findWebhook(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, text string) (*hooksv1.Webhook, error) {
	listing, err := service.List(cmd.Context(), &hooksv1.ListRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return nil, err
	}
	for _, webhook := range listing.GetWebhooks() {
		if webhook.GetKey() == text || webhook.GetName() == text || webhook.GetUrl() == text {
			return webhook, nil
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✗ No webhook has the name or the address %s. icebath hooks list shows them.\n", text)
	return nil, cli.Exit(1)
}

// Fields of a webhook as the commands name them, by their names in the settings
var webhookFields = map[string]string{"address": shellconfig.URL, "events": shellconfig.Posted, "name": shellconfig.Name, "tools": shellconfig.Matcher,
	"time-limit": shellconfig.Timeout, "secret": shellconfig.Secret, "secret-variable": shellconfig.SecretEnv}

func webhooks(service hooksv1connect.HooksServiceClient) *cobra.Command {
	root := &cobra.Command{
		Use:   "webhooks",
		Short: "Add, change, test and remove webhooks",
		Long:  "A webhook is a web address icebath posts events to. A webhook is named by its name or its address.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			usage(cmd.OutOrStdout(), cmd)
			return nil
		},
	}
	var events []string
	added := &cobra.Command{
		Use:     "add <address>",
		Short:   "Add a webhook",
		Long:    "Adds a webhook that posts the events --event names to the address. Give --event once for each event.",
		Example: "  icebath hooks webhooks add https://example.com/hooks --event post_tool_call --event session_end",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := service.ChangeWebhook(cmd.Context(), &hooksv1.WebhookRequest{Profile: picked.Picked(cmd.Context()), Verb: "add", Url: args[0], Events: events})
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
	added.Flags().StringArrayVar(&events, "event", nil, "An event it posts, such as post_tool_call")
	set := &cobra.Command{
		Use:   "set <webhook> <field> <value>",
		Short: "Change the address, events, name, tools, time limit or secret of a webhook",
		Long: "Changes one field of the webhook: address, events (names joined by commas), name, tools (a pattern of tool names), time-limit " +
			"(seconds), secret or secret-variable. An empty value puts the default back.",
		Example: "  icebath hooks webhooks set https://example.com/hooks events post_tool_call,session_end\n" +
			"  icebath hooks webhooks set Build secret-variable WEBHOOK_SECRET",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			field, ok := webhookFields[args[1]]
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "✗ A webhook has no field %s. Its fields are address, events, name, tools, time-limit, secret and "+
					"secret-variable.\n", args[1])
				return cli.Exit(1)
			}
			webhook, err := findWebhook(cmd, service, args[0])
			if err != nil {
				return err
			}
			value, err := yamlValue(field, args[2])
			if err != nil {
				return err
			}
			response, err := service.ChangeWebhook(cmd.Context(), &hooksv1.WebhookRequest{Profile: picked.Picked(cmd.Context()), Verb: "edit", Key: webhook.GetKey(), Field: field, Value: value})
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
	removed := &cobra.Command{
		Use:     "remove <webhook>",
		Aliases: []string{"rm"},
		Short:   "Remove a webhook",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			webhook, err := findWebhook(cmd, service, args[0])
			if err != nil {
				return err
			}
			response, err := service.ChangeWebhook(cmd.Context(), &hooksv1.WebhookRequest{Profile: picked.Picked(cmd.Context()), Verb: "remove", Key: webhook.GetKey()})
			if err != nil {
				return failed(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
	test := &cobra.Command{
		Use:   "test <webhook>",
		Short: "Post a sample event to a webhook",
		Long:  "Posts a sample of the webhook's first event to its address now, signed as its posts are, and says how the address answered.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			webhook, err := findWebhook(cmd, service, args[0])
			if err != nil {
				return err
			}
			response, err := service.TestWebhook(cmd.Context(), &hooksv1.TestWebhookRequest{Profile: picked.Picked(cmd.Context()), Key: webhook.GetKey()})
			if err != nil {
				return failed(cmd, err)
			}
			if !response.GetOk() {
				fmt.Fprintf(cmd.OutOrStdout(), "✗ %s\n", response.GetSaid())
				return cli.Exit(1)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s\n", response.GetSaid())
			return nil
		},
	}
	root.AddCommand(added, set, removed, test)
	return root
}

// Value typed for a field of a webhook as YAML: events as a list, a time limit as a number, the rest as text, an empty value null
func yamlValue(field, typed string) (string, error) {
	typed = strings.TrimSpace(typed)
	switch {
	case typed == "":
		return "null", nil
	case field == shellconfig.Posted:
		var events []string
		for _, event := range strings.Split(typed, ",") {
			if event = strings.TrimSpace(event); event != "" {
				events = append(events, event)
			}
		}
		data, err := json.Marshal(events)
		return string(data), err
	case field == shellconfig.Timeout:
		if _, err := strconv.Atoi(typed); err != nil {
			return "", errors.New("The time limit is whole seconds, such as 10.")
		}
		return typed, nil
	}
	return strconv.Quote(typed), nil
}

// Facts of a hook or webhook after its command or address: the tools it runs for, its time limit and how it stands
func facts(matcher string, timeout int32, standing string) string {
	parts := []string{}
	if matcher != "" {
		parts = append(parts, "Runs for "+shellhooks.ToolWords(matcher)+".")
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

// Prints the hooks by event with their states, then the webhooks with theirs
func listed(cmd *cobra.Command, service hooksv1connect.HooksServiceClient) error {
	response, err := service.List(cmd.Context(), &hooksv1.ListRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	hooks, webhooks := response.GetHooks(), response.GetWebhooks()
	if ignored := response.GetIgnored(); len(ignored) > 0 {
		fmt.Fprintf(out, "⚠ The hooks under %s are left out, because icebath has no such event. The events are %s.\n\n", strings.Join(ignored, ", "),
			eventList(response))
	}
	if len(hooks) == 0 {
		fmt.Fprintln(out, "You have no hooks yet. Add one with icebath hooks add <event> <command>.")
		fmt.Fprintln(out, "The events are "+eventList(response)+".")
	} else {
		byEvent := map[string][]*hooksv1.Hook{}
		var events []string
		for _, hook := range hooks {
			if _, seen := byEvent[hook.GetEvent()]; !seen {
				events = append(events, hook.GetEvent())
			}
			byEvent[hook.GetEvent()] = append(byEvent[hook.GetEvent()], hook)
		}
		slices.SortStableFunc(events, func(a, b string) int { return shellconfig.Place(a) - shellconfig.Place(b) })
		fmt.Fprintf(out, "%s:\n\n", strs.Count(len(hooks), "hook"))
		for _, event := range events {
			fmt.Fprintf(out, "  %s (%s)\n", shellconfig.PointOf(event).Title, event)
			for _, hook := range byEvent[event] {
				standing := hook.GetState() + "."
				switch {
				case hook.GetUnasked():
					standing = "Allowed, because Run without asking lets every hook run."
				case hook.GetAllowed() && hook.GetApprovedAt() != "" && !hook.GetChanged():
					standing = "You allowed it on " + hook.GetApprovedAt() + "."
				case !hook.GetAllowed():
					standing = "Not allowed, so it does not run. icebath hooks allow lets it run."
				}
				if hook.GetFailClosed() {
					standing += " It blocks the tool call when it fails."
				}
				fmt.Fprintf(out, "    %s\n      %s\n", hook.GetCommand(), facts(hook.GetMatcher(), hook.GetTimeout(), standing))
				if hook.GetChanged() {
					fmt.Fprintf(out, "      ⚠ The script changed after you allowed it. Its time was %s then and is %s now. It still runs. Check the "+
						"change, then run icebath hooks allow again.\n", hook.GetChangedWas(), hook.GetChangedNow())
				}
			}
			fmt.Fprintln(out)
		}
	}
	if len(webhooks) == 0 {
		fmt.Fprintln(out, "\nYou have no webhooks. Add one with icebath hooks webhooks add <address> --event <event>.")
		return nil
	}
	fmt.Fprintf(out, "\n%s:\n\n", strs.Count(len(webhooks), "webhook"))
	for _, target := range webhooks {
		fmt.Fprintf(out, "  %s\n", target.GetLabel())
		if target.GetName() != "" {
			fmt.Fprintf(out, "      Address: %s\n", target.GetUrl())
		}
		state := target.GetState() + "."
		if why := target.GetUnsigned(); why != "" {
			state = "Not signed, because " + why + "."
		}
		fmt.Fprintf(out, "      Events: %s. %s\n", strings.Join(target.GetEvents(), ", "), facts(target.GetMatcher(), target.GetTimeout(), state))
	}
	return nil
}

// Fires every hook of the event once on its sample event, the payload file's fields over it, and prints how each run went
func tested(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, event, tool, file string) error {
	out := cmd.OutOrStdout()
	listing, err := service.List(cmd.Context(), &hooksv1.ListRequest{Profile: picked.Picked(cmd.Context())})
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
	response, err := service.Test(cmd.Context(), &hooksv1.TestRequest{Profile: picked.Picked(cmd.Context()), Event: event, Tool: tool, Payload: payload})
	if err != nil {
		return err
	}
	runs := response.GetRuns()
	if len(runs) == 0 {
		if tool != "" {
			fmt.Fprintf(out, "No hook runs on %s for the tool %s.\n", event, tool)
		} else {
			fmt.Fprintf(out, "No hook runs on %s.\n", event)
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
		fmt.Fprintf(out, "      ✗ %s\n", strs.Sentence(run.GetError()))
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

// Takes every allowlist entry of the command off and says how soon running icebaths stop running it
func revoked(cmd *cobra.Command, service hooksv1connect.HooksServiceClient, command string) error {
	response, err := service.Change(cmd.Context(), &hooksv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Verb: "revoke", Command: command})
	if err != nil {
		return failed(cmd, err)
	}
	out := cmd.OutOrStdout()
	if response.GetRemoved() == 0 {
		fmt.Fprintln(out, response.GetSaid())
		return nil
	}
	fmt.Fprintf(out, "✓ %s\n", response.GetSaid())
	fmt.Fprintln(out, "A running icebath stops running it at its next event. Run without asking, --accept-hooks and ICEBATH_ACCEPT_HOOKS each "+
		"let it run anyway.")
	return nil
}

// Checks every hook for its script, consent, drift and answer, and counts the problems
func checked(cmd *cobra.Command, service hooksv1connect.HooksServiceClient) error {
	response, err := service.Check(cmd.Context(), &hooksv1.CheckRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	checks := response.GetChecks()
	if len(checks) == 0 {
		fmt.Fprintln(out, "You have no hooks, so there is nothing to check. Add one with icebath hooks add <event> <command>.")
		return nil
	}
	fmt.Fprintf(out, "Checking %s…\n\n", strs.Count(len(checks), "hook"))
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
		fmt.Fprintln(out, "✓ Every hook works.")
	}
	return nil
}

// Prints what checking one hook found, the sample run kept for an allowed hook whose script can run
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
		fmt.Fprintln(out, "      ✓ It runs without asking you first, as Run without asking or --accept-hooks lets it.")
	default:
		fmt.Fprintf(out, "      ✗ You have not allowed it, so it does not run. Run icebath hooks allow %s %s.\n", check.GetEvent(),
			quoted(check.GetCommand()))
	}
	switch {
	case check.GetCompared() && check.GetChanged():
		fmt.Fprintf(out, "      ⚠ The script changed after you allowed it. Its time was %s then and is %s now. Check the change, then run icebath "+
			"hooks allow again.\n", check.GetChangedWas(), check.GetChangedNow())
	case check.GetCompared():
		fmt.Fprintln(out, "      ✓ The script has not changed since you allowed it.")
	}
	if !check.GetAllowed() {
		fmt.Fprintln(out, "      Its answer was not checked, because it is not allowed.")
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
		fmt.Fprintf(out, "      ✗ It could not run. %s\n", strs.Sentence(run.GetError()))
	case check.GetOutput() == "empty":
		fmt.Fprintf(out, "      ✓ It printed nothing, so it only watches and changes nothing. %s\n", ended(run))
	case check.GetOutput() == "json":
		fmt.Fprintf(out, "      ✓ It answered a made-up event with valid JSON. %s\n", ended(run))
	default:
		fmt.Fprintf(out, "      ✗ Its answer is not valid JSON. %s It printed: %s\n", ended(run), truncate(strings.TrimSpace(run.GetStdout()), 120))
	}
}
