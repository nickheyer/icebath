// Consent to the shell hooks a launch runs with: --accept-hooks allows every hook of the profile, else each hook not allowed yet is asked
// about at the terminal before the app starts
package launch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/launch"
	hooksv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/hooks/v1/hooksv1connect"
)

type flags struct{}

func Register(registry *launch.Registry) {
	registry.Register(flags{})
}

func (flags) Flags() []launch.Flag {
	return []launch.Flag{{Names: []string{"--accept-hooks"}, Help: "Allow new shell hooks without asking, for runs with no terminal to ask in"}}
}

// Takes consent for the profile's hooks, then runs the app; a launch that leaves the profile's config out has no hooks to consent to
func (flags) Around(ctx context.Context, conn *connect.Client, given launch.Given, run launch.Run) error {
	bare := given.Has("--safe-mode") || given.Has("--ignore-user-config") || config.Yes(os.Getenv("ICEBATH_SAFE_MODE")) ||
		config.Yes(os.Getenv("ICEBATH_IGNORE_USER_CONFIG"))
	if !bare {
		if err := consent(ctx, hooksv1connect.NewHooksServiceClient(conn), given.Has("--accept-hooks")); err != nil {
			slog.Debug(fmt.Sprintf("shell-hook registration failed at CLI startup: %v", err))
		}
	}
	return run(ctx)
}

// Allows every hook when the launch or the profile accepts them all, else asks at the terminal about each hook not allowed yet
func consent(ctx context.Context, service hooksv1connect.HooksServiceClient, accept bool) error {
	listing, err := service.List(ctx, &hooksv1.ListRequest{})
	if err != nil {
		return err
	}
	if accept || listing.GetAcceptAll() {
		if len(listing.GetHooks()) == 0 {
			return nil
		}
		_, err := service.Accept(ctx, &hooksv1.AcceptRequest{})
		return err
	}
	console := cli.Terminal(ctx)
	for _, hook := range listing.GetHooks() {
		if hook.GetAllowed() || !console.Interactive() {
			continue
		}
		fmt.Fprintf(console.Out(), "\n⚠ A shell hook wants to run a command for you each time its event happens.\n\n    Event:   %s\n    Command: %s\n\n"+
			"  The command runs with all the rights of your user account. Allow only commands you trust.\n", hook.GetEvent(), hook.GetCommand())
		line, ok := console.Input(fmt.Sprintf("Allow %s to run on %s from now on? [y/N]: ", hook.GetCommand(), hook.GetEvent()))
		if !ok {
			fmt.Fprintln(console.Out())
			continue
		}
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			continue
		}
		if _, err := service.Change(ctx, &hooksv1.ChangeRequest{Verb: "allow", Event: hook.GetEvent(), Command: hook.GetCommand()}); err != nil {
			return err
		}
	}
	return nil
}
