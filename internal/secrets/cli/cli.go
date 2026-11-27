// The secrets command: the subcommands of each secret store, which the stores register, and the status of every store
package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
)

const about = "Pull API keys from an external secret manager at process startup instead of storing them in the profile's .env. " +
	"Supports Bitwarden Secrets Manager, 1Password and a helper command of your own. Every store's settings are in /settings under Secret stores."

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
	root := &cobra.Command{Use: "secrets", Short: "Manage external secret sources (Bitwarden, 1Password)", Long: about, Args: cobra.NoArgs}
	root.AddCommand(c.Registry.Commands(conn)...)
	root.AddCommand(status(secretsv1connect.NewSecretServiceClient(conn)))
	return []*cobra.Command{root}
}

// Prints every store, whether it is on and what its last fetch did, fetching them again when asked
func status(service secretsv1connect.SecretServiceClient) *cobra.Command {
	reload := false
	command := &cobra.Command{Use: "status", Short: "Show every secret store, whether it is on, and what it set as icebath started", Args: cobra.NoArgs,
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
				state := c.Paint("no", cli.Dim)
				if source.GetEnabled() {
					state = c.Paint("yes", cli.Green)
				}
				rows := [][2]string{{"Enabled", state}, {"Shape", source.GetShape()}}
				if n := len(source.GetApplied()); n > 0 {
					rows = append(rows, [2]string{"Set", fmt.Sprintf("%d: %s", n, strings.Join(source.GetApplied(), ", "))})
				}
				for _, kept := range []struct {
					label string
					names []string
				}{{"Kept, set already", source.GetExisting()}, {"Kept, an earlier store's", source.GetClaimed()},
					{"Kept, a store's credential", source.GetProtected()}, {"Not variable names", source.GetInvalid()}} {
					if len(kept.names) > 0 {
						rows = append(rows, [2]string{kept.label, strings.Join(kept.names, ", ")})
					}
				}
				if source.GetError() != "" {
					rows = append(rows, [2]string{"Failed", c.Paint(source.GetError(), cli.Red)})
					if source.GetRemedy() != "" {
						rows = append(rows, [2]string{"Fix", source.GetRemedy()})
					}
				}
				for _, warning := range source.GetWarnings() {
					rows = append(rows, [2]string{"Warning", c.Paint(warning, cli.Yellow)})
				}
				c.Pairs(source.GetLabel()+" ("+source.GetName()+")", rows)
			}
			for _, conflict := range response.GetConflicts() {
				c.Say("Secret sources: "+conflict, cli.Yellow)
			}
			if len(response.GetSources()) == 0 {
				c.Say("No secret stores are installed.", cli.Dim)
			}
			return nil
		}}
	command.Flags().BoolVar(&reload, "reload", false, "Fetch every store again first")
	return command
}
