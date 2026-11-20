package commands

import (
	"context"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/spf13/cobra"
)

var bundlesPaths = []string{"", "list", "show", "create", "delete", "reload"}

// Bundles command runner over the active home, replaced where a caller swaps its line reader
var newBundlesCLI = func(ctx context.Context, out io.Writer) *skills.BundlesCLI {
	return skills.NewBundlesCLI(skills.NewScope(home.Dir(ctx), nil), out)
}

// Binds icebath bundles and its subcommands to the bundles command dispatcher
func (c *CLI) bindBundles() {
	for _, sub := range bundlesPaths {
		path := strings.TrimSpace("bundles " + sub)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runBundles(cmd, sub, args) })
	}
}

// Rescans the bundles dir as the parser registration does, then runs one bundles command line
func runBundles(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f := cmd.Flags()
	a := skills.BundlesArgs{Action: sub}
	switch sub {
	case "show", "delete":
		a.Name = args[0]
	case "create":
		a.Name = args[0]
		a.Skill, _ = f.GetStringArray("skill")
		a.Description, _ = f.GetString("description")
		a.Instruction, _ = f.GetString("instruction")
		a.Force, _ = f.GetBool("force")
	}
	x := newBundlesCLI(ctx, cmd.OutOrStdout())
	if _, err := x.Scope.ScanBundles(); err != nil {
		return err
	}
	code, err := x.BundlesCommand(ctx, a)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
