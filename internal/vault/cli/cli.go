// The vault commands: add, list and remove items of the profile's own vault, the password managers it reads, and where its key lives
package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/vault"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1/vaultv1connect"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	v := vaultCLI{service: vaultv1connect.NewVaultServiceClient(conn)}
	root := &cobra.Command{Use: "vault", Short: "Manage the local encrypted autofill vault (add/list/rm credentials)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.list(cmd) }}
	key := &cobra.Command{Use: "key", Short: "Show where the local vault key is stored (vault.key file or OS keychain)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.key(cmd) }}
	key.AddCommand(v.migrate())
	root.AddCommand(v.add(), &cobra.Command{Use: "list", Short: "List vault items (metadata only, never values)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.list(cmd) }}, v.rm(), v.sources(), key)
	return []*cobra.Command{root}
}

type vaultCLI struct {
	service vaultv1connect.VaultServiceClient
}

// A failure of the vault said as it is, the command still ending well; any other failure as it comes
func shown(c *cli.Console, err error) error {
	if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeFailedPrecondition {
		c.Say(c.Paint("Error:", cli.Red) + " " + failure.Message())
		return nil
	}
	return err
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// Line read for the wizard, stripped; an ended input ends the command with status 1
func (v vaultCLI) ask(c *cli.Console, prompt string, hidden bool) (string, error) {
	read := c.Input
	if hidden {
		read = c.Secret
	}
	value, ok := read(prompt)
	if !ok {
		return "", cli.Exit(1)
	}
	if hidden {
		return value, nil
	}
	return strings.TrimSpace(value), nil
}

// Asks until the answer is not empty
func (v vaultCLI) insist(c *cli.Console, prompt string, hidden bool) (string, error) {
	for {
		value, err := v.ask(c, prompt, hidden)
		if err != nil || value != "" {
			return value, err
		}
	}
}

func (v vaultCLI) add() *cobra.Command {
	var kind string
	command := &cobra.Command{Use: "add", Short: "Save a login, card or address ahead of time (optional: the agent asks you on the page when it needs one)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			c.Say(c.Paint("Add a vault item", cli.Bold) + " (the password is encrypted at rest and the agent never sees it; the identifier is visible " +
				"metadata the agent can type itself)")
			kind = strings.ToLower(strings.TrimSpace(kind))
			for !slices.Contains(vault.Kinds, kind) {
				typed, err := v.ask(c, fmt.Sprintf("Kind (%s) [login]: ", strings.Join(vault.Kinds, "/")), false)
				if err != nil {
					return err
				}
				if kind = strings.ToLower(typed); kind == "" {
					kind = "login"
				}
				if !slices.Contains(vault.Kinds, kind) {
					c.Say(fmt.Sprintf("Unknown kind %q", kind), cli.Red)
					kind = ""
				}
			}
			label, err := v.insist(c, "Label (e.g. 'GitHub work account'): ", false)
			if err != nil {
				return err
			}
			values := map[string]string{}
			var origin string
			if kind == "login" {
				if origin, err = v.insist(c, "Site origin (e.g. https://github.com): ", false); err != nil {
					return err
				}
				idType := ""
				for !slices.Contains(vault.IdentifierTypes, idType) {
					typed, err := v.ask(c, fmt.Sprintf("Identifier type (%s) [email]: ", strings.Join(vault.IdentifierTypes, "/")), false)
					if err != nil {
						return err
					}
					if idType = strings.ToLower(typed); idType == "" {
						idType = "email"
					}
				}
				values["identifier_type"] = idType
				if values["identifier"], err = v.insist(c, strings.ToUpper(idType[:1])+idType[1:]+": ", false); err != nil {
					return err
				}
				if values["password"], err = v.insist(c, "Password (hidden): ", true); err != nil {
					return err
				}
				if values["otp_secret"], err = v.ask(c, "Authenticator key (optional, hidden; the 2FA \"setup key\" or otpauth:// link — Enter to skip): ", true); err != nil {
					return err
				}
			} else {
				if origin, err = v.insist(c, "Site origin the item may be filled on (e.g. https://shop.example.com): ", false); err != nil {
					return err
				}
				c.Say(kind+" fields are filled only on that origin; card values are read hidden.", cli.Dim)
				for _, field := range vault.KindFields(kind) {
					prompt := strings.ReplaceAll(field.Key, "_", " ")
					if !field.Required {
						prompt += " (optional)"
					}
					prompt += ": "
					value, err := v.ask(c, prompt, field.Secret)
					for err == nil && field.Required && strings.TrimSpace(value) == "" {
						value, err = v.ask(c, prompt, field.Secret)
					}
					if err != nil {
						return err
					}
					values[field.Key] = strings.TrimSpace(value)
				}
			}
			added, err := v.service.Add(cmd.Context(), &vaultv1.AddRequest{Kind: kind, Label: label, Origin: origin, Values: values})
			if err != nil {
				return shown(c, err)
			}
			item := added.GetItem()
			c.Say(fmt.Sprintf("%s handle=%s kind=%s origin=%s", c.Paint("Stored.", cli.Green), c.Paint(item.GetHandle(), cli.Bold), item.GetKind(),
				dash(item.GetOrigin())))
			return nil
		}}
	command.Flags().StringVar(&kind, "kind", "", "login, payment or address")
	return command
}

func (v vaultCLI) list(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{})
	if err != nil {
		return shown(c, err)
	}
	if len(listed.GetItems()) == 0 && len(listed.GetLocked()) == 0 {
		c.Say("Vault is empty. Add an item with `icebath vault add`.", cli.Dim)
		return nil
	}
	if items := listed.GetItems(); len(items) > 0 {
		c.Say(fmt.Sprintf("Vault items (%d)", len(items)), cli.Bold)
		var rows [][]string
		for _, item := range items {
			rows = append(rows, []string{c.Paint(item.GetHandle(), cli.Bold), item.GetSource(), item.GetKind(), item.GetLabel(), dash(item.GetIdentifier()),
				dash(item.GetOrigin())})
		}
		c.Table([]string{"Handle", "Source", "Kind", "Label", "Identifier", "Origin"}, rows)
		c.Say("Passwords are never shown; the agent fills them server-side from the handle.", cli.Dim)
	}
	for _, name := range listed.GetLocked() {
		c.Say(c.Paint(name, cli.Yellow) + " is enabled but locked — the agent will ask you to unlock it when it needs a login.")
	}
	return nil
}

func (v vaultCLI) rm() *cobra.Command {
	return &cobra.Command{Use: "rm <handle>", Short: "Remove a vault item by handle", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			removed, err := v.service.Remove(cmd.Context(), &vaultv1.RemoveRequest{Handle: args[0]})
			if err != nil {
				return shown(c, err)
			}
			if removed.GetRemoved() {
				c.Say(c.Paint("Removed", cli.Green) + " " + args[0])
			} else {
				c.Say(fmt.Sprintf("No vault item with handle %q", args[0]), cli.Red)
			}
			return nil
		}}
}

func (v vaultCLI) sources() *cobra.Command {
	var enable, disable string
	command := &cobra.Command{Use: "sources", Short: "Show detected password managers (1Password, Bitwarden); they are on automatically", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			if enable != "" || disable != "" {
				name, on := disable, false
				if enable != "" {
					name, on = enable, true
				}
				set, err := v.service.SetSource(cmd.Context(), &vaultv1.SetSourceRequest{Name: name, On: on})
				if err != nil {
					if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeFailedPrecondition {
						c.Say(failure.Message(), cli.Red)
						return nil
					}
					return err
				}
				state := "off"
				if on {
					state = "on"
				}
				c.Say(c.Paint(set.GetDisplayName()+" "+state, cli.Green) + " for browser logins.")
				return nil
			}
			listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{})
			if err != nil {
				return shown(c, err)
			}
			for _, source := range listed.GetSources() {
				status := c.Paint("not installed", cli.Dim)
				switch source.GetState() {
				case "on":
					status = c.Paint("detected", cli.Green) + " · the agent asks you to unlock it when it needs a login"
				case "off":
					status = c.Paint("turned off", cli.Dim) + fmt.Sprintf(" (`icebath vault sources --enable %s` to use it)", source.GetName())
				}
				c.Say(fmt.Sprintf("  %-10s %s", source.GetDisplayName(), status))
			}
			c.Say("Managers are picked up automatically when their CLI is installed and signed in.", cli.Dim)
			return nil
		}}
	command.Flags().StringVar(&disable, "disable", "", "Stop reading logins from the password manager of the name")
	command.Flags().StringVar(&enable, "enable", "", "Read logins from the password manager of the name again")
	command.MarkFlagsMutuallyExclusive("enable", "disable")
	return command
}

func (v vaultCLI) key(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{})
	if err != nil {
		return shown(c, err)
	}
	key := listed.GetKey()
	switch key.GetStorage() {
	case "file":
		c.Say("Vault key: " + c.Paint("file", cli.Bold) + " " + key.GetPath() + " (0600, beside the encrypted vault)")
	case "keychain":
		c.Say(fmt.Sprintf("Vault key: %s service %q, account %q", c.Paint("OS keychain", cli.Bold), key.GetService(), key.GetAccount()))
	default:
		c.Say("Vault key: " + c.Paint("none yet", cli.Dim) + " (created with the first saved item)")
	}
	c.Say("New vaults use: vault.key_storage = " + key.GetConfigured())
	c.Say("OS keychain here: " + key.GetKeychain())
	return nil
}

func (v vaultCLI) migrate() *cobra.Command {
	var to string
	command := &cobra.Command{Use: "migrate", Short: "Move the vault key to the OS keychain or back to vault.key (verified before the old copy is removed)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			if to != "keychain" && to != "file" {
				return fmt.Errorf("argument --to: invalid choice: %q (choose from 'keychain', 'file')", to)
			}
			key, err := v.service.MoveKey(cmd.Context(), &vaultv1.MoveKeyRequest{To: to})
			if err != nil {
				return shown(c, err)
			}
			switch {
			case key.GetStorage() == "none":
				place := "vault.key"
				if to == "keychain" {
					place = "the OS keychain"
				}
				c.Say(fmt.Sprintf("No vault key exists yet. The first saved item creates one in %s (vault.key_storage = %s).", place, to))
			case to == "keychain":
				c.Say(c.Paint("Vault key moved to the OS keychain", cli.Green) + " (" + key.GetKeychain() + "); vault.key was removed after the keychain copy " +
					"decrypted the vault.")
				c.Say("Backups of this profile now hold the encrypted vault without its key; `icebath vault key migrate --to file` before moving the "+
					"profile to another machine.", cli.Dim)
			default:
				c.Say(c.Paint("Vault key moved to", cli.Green) + " " + key.GetPath() + " (0600).")
				if left := key.GetKeychainLeft(); left != "" {
					c.Say(c.Paint("The keychain copy ("+left+") could not be removed yet;", cli.Yellow) + " run this command again when the keychain is reachable.")
				} else {
					c.Say("Keychain entry removed.")
				}
			}
			return nil
		}}
	command.Flags().StringVar(&to, "to", "", "keychain or file")
	command.MarkFlagRequired("to")
	return command
}
