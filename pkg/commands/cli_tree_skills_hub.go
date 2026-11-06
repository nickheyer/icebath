package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/spf13/cobra"
)

var skillsHubPaths = []string{"", "browse", "search", "install", "inspect", "list", "check", "update", "audit", "uninstall",
	"reset", "list-modified", "diff", "opt-out", "opt-in", "repair-official", "publish", "snapshot", "snapshot export",
	"snapshot import", "tap", "tap list", "tap add", "tap remove"}

// Hub command runner over the active home, replaced where a caller swaps hub collaborators
var newSkillsHubCLI = func(ctx context.Context) *skills.HubCLI {
	return skills.NewHubCLI(skills.NewScope(home.Dir(ctx), nil))
}

// Binds icebath skills and its hub subcommands to the skills hub router
func (c *CLI) bindSkillsHub() {
	for _, sub := range skillsHubPaths {
		path := strings.TrimSpace("skills " + sub)
		c.Bind(path, func(cmd *cobra.Command, args []string) error { return runSkillsHub(cmd, sub, args) })
	}
	c.Bind("skills config", runSkillsConfig)
	for _, action := range []string{"trust", "untrust"} {
		c.Bind("skills "+action, func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) > 0 {
				path = args[0]
			}
			return skills.SkillsTrustCommand(cmdContext(cmd), home.Dir(cmdContext(cmd)), action, path, cmd.OutOrStdout())
		})
	}
}

// Stdin terminal check the interactive skills config gate reads
var skillsConfigStdinTTY = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }

// Refuses a piped stdin with exit status one, then runs the interactive skill enable and disable picker
func runSkillsConfig(cmd *cobra.Command, args []string) error {
	if !skillsConfigStdinTTY() {
		fmt.Fprint(cmd.ErrOrStderr(), "Error: 'icebath skills config' requires an interactive terminal.\nIt cannot be run through a pipe or non-interactive subprocess.\nRun it directly in your terminal instead.\n")
		return &ExitError{Code: 1}
	}
	ctx := cmdContext(cmd)
	return skills.SkillsCommand(ctx, cliui.Default(), home.Dir(ctx))
}

// Parses one skills hub command line into its argparse namespace and runs it
func runSkillsHub(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f := cmd.Flags()
	str := func(name string) string { v, _ := f.GetString(name); return v }
	boolean := func(name string) bool { v, _ := f.GetBool(name); return v }
	integer := func(name string) int { v, _ := f.GetInt(name); return v }
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	action, rest, _ := strings.Cut(sub, " ")
	a := skills.SkillsArgs{Action: action}
	switch action {
	case "browse":
		a.Page, a.Size, a.Source = integer("page"), integer("size"), str("source")
	case "search":
		a.Query, a.Source, a.Limit, a.JSON = arg(0), str("source"), integer("limit"), boolean("json")
	case "install":
		a.Identifier, a.Category, a.Name, a.Force, a.Yes = arg(0), str("category"), str("name"), boolean("force"), boolean("yes")
	case "inspect":
		a.Identifier = arg(0)
	case "list":
		a.Source, a.EnabledOnly = str("source"), boolean("enabled-only")
	case "check", "diff":
		a.Name = arg(0)
	case "update":
		a.Name, a.Force = arg(0), boolean("force")
	case "audit":
		a.Name, a.Deep = arg(0), boolean("deep")
	case "uninstall":
		a.Name, a.Yes = arg(0), boolean("yes")
	case "reset", "repair-official":
		a.Name, a.Restore, a.Yes = arg(0), boolean("restore"), boolean("yes")
	case "list-modified":
		a.JSON = boolean("json")
	case "opt-out":
		a.Remove, a.Yes = boolean("remove"), boolean("yes")
	case "opt-in":
		a.Sync = boolean("sync")
	case "publish":
		a.SkillPath, a.To, a.Repo = arg(0), str("to"), str("repo")
	case "snapshot":
		a.SnapshotAction = rest
		switch rest {
		case "export":
			a.Output = arg(0)
		case "import":
			a.Input, a.Force = arg(0), boolean("force")
		}
	case "tap":
		a.Tap = rest
		switch rest {
		case "add":
			a.Repo = arg(0)
		case "remove":
			a.Name = arg(0)
		}
	}
	x := newSkillsHubCLI(ctx)
	x.C, x.Stdout = pyrich.NewConsole(cmd.OutOrStdout()), cmd.OutOrStdout()
	code, err := x.SkillsCommand(ctx, a)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
