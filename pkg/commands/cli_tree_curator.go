package commands

import (
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/spf13/cobra"
)

var curatorSubcommands = []string{"status", "usage", "run", "pause", "resume", "pin", "unpin", "lock", "unlock", "list-unmanaged",
	"adopt", "restore", "list-archived", "archive", "prune", "backup", "rollback", "ledger", "purge"}

// Binds every icebath curator subcommand to the curator CLI handlers
func (c *CLI) bindCurator() {
	for _, sub := range curatorSubcommands {
		c.Bind("curator "+sub, func(cmd *cobra.Command, args []string) error { return runCurator(cmd, sub, args) })
	}
}

// Maps one parsed curator command line into CuratorArgs and runs it, a nonzero status becoming an ExitError
func runCurator(cmd *cobra.Command, sub string, args []string) error {
	ctx := cmdContext(cmd)
	f := cmd.Flags()
	flagBool := func(name string) bool {
		v, _ := f.GetBool(name)
		return v
	}
	flagStr := func(name string) string {
		v, _ := f.GetString(name)
		return v
	}
	a := memory.CuratorArgs{}
	switch sub {
	case "pin", "unpin", "lock", "unlock", "restore", "archive":
		a.Skill = args[0]
	case "usage":
		a.Sort, a.Provenance, a.JSON = flagStr("sort"), flagStr("provenance"), flagBool("json")
	case "run":
		a.Synchronous, a.Background, a.DryRun, a.Consolidate = flagBool("sync"), flagBool("background"), flagBool("dry-run"), flagBool("consolidate")
	case "adopt":
		a.Skills, a.AllUnmanaged, a.DryRun, a.Yes = args, flagBool("all-unmanaged"), flagBool("dry-run"), flagBool("yes")
	case "prune", "purge":
		if f.Changed("days") {
			days, _ := f.GetInt("days")
			a.Days = &days
		}
		a.Yes, a.DryRun = flagBool("yes"), flagBool("dry-run")
	case "backup":
		a.Reason = flagStr("reason")
	case "rollback":
		if len(args) > 0 {
			a.EntryID = args[0]
		}
		a.List, a.BackupID, a.Yes = flagBool("list"), flagStr("id"), flagBool("yes")
	case "ledger":
		a.Skill, a.Compact = flagStr("skill"), flagBool("compact")
		a.Limit, _ = f.GetInt("limit")
	}
	code, err := memory.NewCuratorCLI(cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin()).Run(ctx, sub, a)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// Binds icebath insights to the terminal insights report
func (c *CLI) bindInsights() {
	c.Bind("insights", func(cmd *cobra.Command, args []string) error {
		days, _ := cmd.Flags().GetInt("days")
		source, _ := cmd.Flags().GetString("source")
		memory.CmdInsights(cmdContext(cmd), cmd.OutOrStdout(), days, source)
		return nil
	})
}
