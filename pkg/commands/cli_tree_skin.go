package commands

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/spf13/cobra"
)

// Binds icebath skin and its list, use and set subcommands to the skin command
func (c *CLI) bindSkin() {
	for _, sub := range []string{"", "list", "use", "set"} {
		path := strings.TrimSpace("skin " + sub)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runSkin(cmd, sub, args) })
	}
}

// Parses one skin command line and runs it, a nonzero status becoming an ExitError
func runSkin(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	a := config.SkinArgs{Command: sub}
	switch sub {
	case "use":
		a.Name = args[0]
	case "set":
		a.Key, a.Value = args[0], args[1]
		a.Skin, _ = cmd.Flags().GetString("skin")
	}
	code, err := config.SkinCommand(ctx, a, configCommandSources(home.Dir(ctx)), cmd.OutOrStdout(), cmd.ErrOrStderr())
	var exit *config.ConfigExit
	if errors.As(err, &exit) {
		return &ExitError{Code: exit.Code}
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
