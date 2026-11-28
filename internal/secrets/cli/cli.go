// The secrets command: the subcommands of each secret store, which the stores register, and what every store shares: how each stands,
// turning one on or off, and the order they are asked in
package cli

import (
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

const about = "Secret stores load your API keys each time icebath starts, so you do not have to keep them in the keys file. icebath works " +
	"with Bitwarden Secrets Manager, 1Password and a helper command of your own. In the app, /secrets shows them."

// Commands of the secret stores, under icebath secrets
type Commands struct {
	cli.Registry
}

func New() *Commands {
	return &Commands{}
}

func Register(host *cli.CLI, commands *Commands) {
	host.Register(commands)
}

func (c *Commands) Commands(conn *connect.Client) []*cobra.Command {
	service := secretsv1connect.NewSecretServiceClient(conn)
	root := &cobra.Command{Use: "secrets", GroupID: cli.GroupSecrets, Short: "Load API keys from Bitwarden, 1Password or a command", Long: about, Args: cobra.NoArgs}
	root.AddCommand(c.Registry.Commands(conn)...)
	root.AddCommand(status(service), turn(service, true), turn(service, false), order(service))
	return []*cobra.Command{root}
}

// State of a store in the words the page says it in: On with how many variables it fills, Off, or what it needs
func standing(source *secretsv1.Source) string {
	switch {
	case !source.GetEnabled():
		return "Off"
	case source.GetNeeds() != "":
		return source.GetNeeds()
	}
	return "On · " + strs.Count(int(source.GetFills()), "variable")
}

// Prints every store, how it stands and what its last fetch did, fetching them again when asked
func status(service secretsv1connect.SecretServiceClient) *cobra.Command {
	reload := false
	command := &cobra.Command{Use: "status", Short: "Show each secret store and what it loaded",
		Long: "Shows every secret store in the order they are asked, how each stands and which variables it set. Variables a store left alone " +
			"are listed with the reason. With --reload, every store loads again first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			var response *secretsv1.SourcesResponse
			var err error
			if reload {
				response, err = service.Reload(cmd.Context(), &secretsv1.ReloadRequest{Profile: picked.Picked(cmd.Context())})
			} else {
				response, err = service.Sources(cmd.Context(), &secretsv1.SourcesRequest{Profile: picked.Picked(cmd.Context())})
			}
			if err != nil {
				return err
			}
			for _, source := range response.GetSources() {
				loads := "Every variable the store holds"
				if source.GetShape() == secrets.Mapped {
					loads = "Each variable you set from it"
				}
				rows := [][2]string{{"State", standing(source)}, {"Loads", loads}, {"Command", "icebath secrets " + source.GetName()}}
				if lacks := source.GetLacks(); lacks != "" {
					rows = append(rows, [2]string{"What it lacks", c.Paint(strs.Sentence(lacks), cli.Yellow)})
				}
				if n := len(source.GetApplied()); n > 0 {
					rows = append(rows, [2]string{"Set", strs.Count(n, "variable") + ": " + strings.Join(source.GetApplied(), ", ")})
				}
				for _, kept := range []struct {
					label string
					names []string
				}{{"Left as you set them", source.GetExisting()}, {"Set by a store before it", source.GetClaimed()},
					{"Hold the token of a store", source.GetProtected()}, {"Not variable names", source.GetInvalid()}} {
					if len(kept.names) > 0 {
						rows = append(rows, [2]string{kept.label, strings.Join(kept.names, ", ")})
					}
				}
				if source.GetError() != "" {
					rows = append(rows, [2]string{"Last fetch failed", c.Paint(strs.Sentence(source.GetError()), cli.Red)})
					if source.GetRemedy() != "" {
						rows = append(rows, [2]string{"What to do", strs.Sentence(source.GetRemedy())})
					}
				}
				for _, warning := range source.GetWarnings() {
					rows = append(rows, [2]string{"Warning", c.Paint(strs.Sentence(warning), cli.Yellow)})
				}
				c.Pairs(source.GetLabel(), rows)
			}
			for _, conflict := range response.GetConflicts() {
				c.Say("⚠ "+conflict, cli.Yellow)
			}
			if len(response.GetSources()) == 0 {
				c.Say("No secret stores are installed in this build of icebath.", cli.Dim)
			}
			return nil
		}}
	command.Flags().BoolVar(&reload, "reload", false, "Load every store again first")
	return command
}

// Turns a store on or off, as its page does
func turn(service secretsv1connect.SecretServiceClient, on bool) *cobra.Command {
	use, short, long := "on <store>", "Turn a secret store on", "Turns the store on, so it fills its variables from now on and each time icebath starts."
	if !on {
		use, short, long = "off <store>", "Turn a secret store off", "Turns the store off. Its settings and token stay for when you turn it on."
	}
	return &cobra.Command{Use: use, Short: short, Long: long, Example: "  icebath secrets " + strings.Fields(use)[0] + " onepassword", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			done, err := service.Switch(cmd.Context(), &secretsv1.SwitchRequest{Profile: picked.Picked(cmd.Context()), Name: args[0], On: on})
			if err != nil {
				return c.Fail("✗ " + strs.Sentence(tui.Reason(err)))
			}
			c.Say(c.Tick() + " " + done.GetNote())
			return nil
		}}
}

// Shows the order the stores are asked in, or asks them in the order named
func order(service secretsv1connect.SecretServiceClient) *cobra.Command {
	return &cobra.Command{Use: "order [store...]", Short: "Show or change the order the stores are asked in",
		Long: "With no store named, shows the order the stores are asked in. The first store to give a variable wins it. Naming stores asks " +
			"them first in that order, and the others after them.",
		Example: "  icebath secrets order bitwarden onepassword",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			if len(args) > 0 {
				done, err := service.Reorder(cmd.Context(), &secretsv1.ReorderRequest{Profile: picked.Picked(cmd.Context()), Names: args})
				if err != nil {
					return c.Fail("✗ " + strs.Sentence(tui.Reason(err)))
				}
				c.Say(c.Tick() + " " + done.GetNote())
				return nil
			}
			response, err := service.Sources(cmd.Context(), &secretsv1.SourcesRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			var rows [][]string
			for i, source := range response.GetSources() {
				rows = append(rows, []string{strs.Number(float64(i + 1)), source.GetLabel(), source.GetName(), standing(source)})
			}
			c.Table([]string{"Place", "Store", "Name", "State"}, rows)
			return nil
		}}
}
