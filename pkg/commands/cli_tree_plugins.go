package commands

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/spf13/cobra"
)

var pluginsVerbs = []string{"", "install", "search", "browse", "validate", "update", "adopt", "trust-update-url", "check-updates",
	"remove", "list", "enable", "disable", "capabilities", "doctor", "pack", "pack install", "pack export", "pack show", "show"}

// Binds icebath plugins, every plugins verb and the pack subcommands to the plugins command
func (c *CLI) bindPlugins() {
	for _, verb := range pluginsVerbs {
		path := strings.TrimSpace("plugins " + verb)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runPlugins(cmd, verb, args) })
	}
}

// Tri state of a --x and --no-x pair, nil when neither was given
func triStateFlag(cmd *cobra.Command, yes, no string) *bool {
	if v, _ := cmd.Flags().GetBool(yes); v {
		return &v
	}
	if v, _ := cmd.Flags().GetBool(no); v {
		f := false
		return &f
	}
	return nil
}

// Runs one plugins action the way plugins_command dispatches it, a CommandExit becoming an ExitError
func runPlugins(cmd *cobra.Command, verb string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	err := pluginsAction(ctx, cmd, verb, args)
	var exit *plugins.CommandExit
	if errors.As(err, &exit) {
		return &ExitError{Code: exit.Code}
	}
	return err
}

// Prints the refusal and ends the command with exit 1
func pluginsFail(cmd *cobra.Command, message string) error {
	plugins.NewConsole(cmd.OutOrStdout()).Print(message)
	return &ExitError{Code: 1}
}

func pluginsAction(ctx context.Context, cmd *cobra.Command, verb string, args []string) error {
	out, errOut, flags := cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.Flags()
	flag := func(name string) bool { v, _ := flags.GetBool(name); return v }
	arg := func(def string) string {
		if len(args) > 0 {
			return args[0]
		}
		return def
	}
	switch verb {
	case "":
		return plugins.CmdToggle(ctx, out)
	case "install":
		ref, _ := flags.GetString("ref")
		return plugins.CmdInstall(ctx, plugins.InstallCommandOptions{Identifier: arg(""), Force: flag("force"), Enable: triStateFlag(cmd, "enable", "no-enable"),
			Ref: ref, AllowRemoved: flag("allow-removed"), NoDeps: flag("no-deps"), YesDeps: flag("yes-deps")}, out)
	case "search":
		return plugins.CmdSearch(ctx, arg(""), flag("json"), out)
	case "browse":
		return plugins.CmdSearch(ctx, "", false, out)
	case "validate":
		return plugins.CmdValidate(ctx, arg(""), flag("json"), flag("install-deps"), out, errOut)
	case "update":
		if name := arg(""); name != "" {
			if flag("yes") {
				return pluginsFail(cmd, "[red]Error:[/red] --yes only applies to `icebath plugins update` with no name.")
			}
			return plugins.CmdUpdate(ctx, name, true, flag("force"), out)
		}
		if flag("force") {
			return pluginsFail(cmd, "[red]Error:[/red] --force applies to one named plugin: `icebath plugins update <name> --force`.")
		}
		return plugins.CmdUpdateAll(ctx, flag("yes"), out)
	case "adopt":
		return plugins.CmdAdopt(ctx, arg(""), out)
	case "trust-update-url":
		return plugins.CmdTrustUpdateURL(ctx, arg(""), out)
	case "check-updates":
		return plugins.CmdCheckUpdates(ctx, flag("json"), out)
	case "remove":
		return plugins.CmdRemove(ctx, arg(""), out)
	case "list":
		return plugins.CmdList(ctx, plugins.ListOptions{NoBundled: flag("no-bundled"), User: flag("user"), Enabled: flag("enabled"), Plain: flag("plain"), JSON: flag("json")}, out)
	case "enable":
		return plugins.CmdEnable(ctx, arg(""), triStateFlag(cmd, "allow-tool-override", "no-allow-tool-override"), out)
	case "disable":
		return plugins.CmdDisable(ctx, arg(""), out)
	case "capabilities":
		if len(args) == 0 {
			return plugins.CmdCapabilities(ctx, nil, out)
		}
		return plugins.CmdCapabilities(ctx, &args[0], out)
	case "doctor":
		return plugins.CmdPluginDoctor(ctx, arg("."), flag("ci"), out)
	case "show":
		if cmd.CalledAs() == "info" {
			return plugins.CmdInfo(ctx, arg(""), out)
		}
		return plugins.CmdShow(ctx, arg(""), out)
	}
	pack := plugins.PackArgs{Action: strings.TrimPrefix(strings.TrimPrefix(verb, "pack"), " ")}
	switch pack.Action {
	case "install":
		pack.Source, pack.Force = arg(""), flag("force")
	case "export":
		pack.EnabledOnly = flag("enabled-only")
		pack.Name, _ = flags.GetString("name")
	case "show":
		pack.Source = arg("")
	}
	return plugins.PackCommand(ctx, pack, out, errOut)
}
