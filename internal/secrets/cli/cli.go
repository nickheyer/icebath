// The secrets command: the subcommands of each secret store, which the stores register, and the status of every store
package cli

import (
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/secrets"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
)

const about = "Secret stores load your API keys each time icebath starts, so you do not have to keep them in the keys file. icebath works " +
	"with Bitwarden Secrets Manager, 1Password and a helper command of your own. Each store has its settings in /settings under Secret stores."

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
	root := &cobra.Command{Use: "secrets", GroupID: cli.GroupSecrets, Short: "Load API keys from Bitwarden, 1Password or a command", Long: about, Args: cobra.NoArgs}
	root.AddCommand(c.Registry.Commands(conn)...)
	root.AddCommand(status(secretsv1connect.NewSecretServiceClient(conn)))
	return []*cobra.Command{root}
}

// Prints every store, whether it is on and what its last fetch did, fetching them again when asked
func status(service secretsv1connect.SecretServiceClient) *cobra.Command {
	reload := false
	command := &cobra.Command{Use: "status", Short: "Show each secret store and what it loaded",
		Long: "Shows every secret store, whether it is on and which variables it set when icebath started. Variables a store left alone are " +
			"listed with the reason. With --reload, every store loads again first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			var response *secretsv1.SourcesResponse
			var err error
			if reload {
				response, err = service.Reload(cmd.Context(), &secretsv1.ReloadRequest{})
			} else {
				response, err = service.Sources(cmd.Context(), &secretsv1.SourcesRequest{})
			}
			if err != nil {
				return err
			}
			for _, source := range response.GetSources() {
				loads := "Every variable the store holds"
				if source.GetShape() == secrets.Mapped {
					loads = "Each variable you set from it"
				}
				rows := [][2]string{{"Turned on", c.OnOff(source.GetEnabled())}, {"Loads", loads}, {"Command", "icebath secrets " + source.GetName()}}
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
					rows = append(rows, [2]string{"Last fetch failed", c.Paint(secrets.Sentence(source.GetError()), cli.Red)})
					if source.GetRemedy() != "" {
						rows = append(rows, [2]string{"What to do", secrets.Sentence(source.GetRemedy())})
					}
				}
				for _, warning := range source.GetWarnings() {
					rows = append(rows, [2]string{"Warning", c.Paint(secrets.Sentence(warning), cli.Yellow)})
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
