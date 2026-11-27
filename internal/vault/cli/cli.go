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
	"github.com/nickheyer/icebath/pkg/strs"
)

type commands struct{}

func Register(host *cli.CLI) {
	host.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	v := vaultCLI{service: vaultv1connect.NewVaultServiceClient(conn)}
	root := &cobra.Command{Use: "vault", GroupID: cli.GroupSecrets, Short: "Manage the logins and cards the agent may fill in",
		Long: "The autofill vault keeps logins, cards and addresses that the agent may fill into web pages for you. The vault is encrypted " +
			"on this computer, and the agent never sees a password. The vault can also read logins from 1Password or Bitwarden. With no " +
			"subcommand, vault lists what it holds.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.list(cmd) }}
	key := &cobra.Command{Use: "key", Short: "Show where the key of the vault is kept",
		Long: "Shows whether the key that unlocks the vault is in the vault.key file or in the keychain of your system.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.key(cmd) }}
	key.AddCommand(v.migrate())
	root.AddCommand(v.add(), &cobra.Command{Use: "list", Short: "List what the vault holds",
		Long: "Lists every item of the vault with its handle, kind and website, but never a password or card number.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.list(cmd) }}, v.rm(), v.sources(), key)
	return []*cobra.Command{root}
}

type vaultCLI struct {
	service vaultv1connect.VaultServiceClient
}

// A failure of the vault said as it is, the command still ending well; any other failure as it comes
func shown(c *cli.Console, err error) error {
	if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeFailedPrecondition {
		c.Say("✗ "+failure.Message(), cli.Red)
		return nil
	}
	return err
}

func dash(value string) string {
	if value == "" {
		return "None"
	}
	return value
}

// Kind of an item as people read it
func kindWord(kind string) string {
	switch kind {
	case "login":
		return "login"
	case "payment":
		return "payment card"
	}
	return kind
}

// Name of a field as a prompt starts it
func fieldPrompt(key string) string {
	name := vault.FieldName(key)
	return strings.ToUpper(name[:1]) + name[1:]
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
	command := &cobra.Command{Use: "add", Short: "Save a login, card or address ahead of time",
		Long: "Asks for a login, card or address and saves it in the vault. You do not have to, because the agent asks you on the page when " +
			"asks you there. The password is encrypted and the agent never sees it.",
		Example: "  icebath vault add --kind login",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			c.Say("Add an item to the vault", cli.Bold)
			c.Say("The password is kept encrypted, and the agent never sees it. The agent does see what you sign in with, such as your "+
				"email, and may type it itself.", cli.Dim)
			kind = strings.ToLower(strings.TrimSpace(kind))
			for !slices.Contains(vault.Kinds, kind) {
				typed, err := v.ask(c, "Kind of item: login, payment or address [login]: ", false)
				if err != nil {
					return err
				}
				if kind = strings.ToLower(typed); kind == "" {
					kind = "login"
				}
				if !slices.Contains(vault.Kinds, kind) {
					c.Say(fmt.Sprintf("✗ “%s” is not login, payment or address.", kind), cli.Red)
					kind = ""
				}
			}
			label, err := v.insist(c, "Name, such as GitHub work account: ", false)
			if err != nil {
				return err
			}
			values := map[string]string{}
			var origin string
			if kind == "login" {
				if origin, err = v.insist(c, "Address of the site, such as https://github.com: ", false); err != nil {
					return err
				}
				idType := ""
				for !slices.Contains(vault.IdentifierTypes, idType) {
					typed, err := v.ask(c, "You sign in with your email, phone or username [email]: ", false)
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
				if values["password"], err = v.insist(c, "Password, hidden as you type: ", true); err != nil {
					return err
				}
				c.Say("An authenticator key is the setup key a site shows for two-step sign-in, or an otpauth:// link. It is hidden as you type.",
					cli.Dim)
				if values["otp_secret"], err = v.ask(c, "Authenticator key, or press enter to skip: ", true); err != nil {
					return err
				}
			} else {
				if origin, err = v.insist(c, "Address of the site it may be filled on, such as https://shop.example.com: ", false); err != nil {
					return err
				}
				if kind == "payment" {
					c.Say("The card is filled only on that site, and its values are hidden as you type.", cli.Dim)
				} else {
					c.Say("The address is filled only on that site.", cli.Dim)
				}
				for _, field := range vault.KindFields(kind) {
					prompt := fieldPrompt(field.Key)
					if !field.Required {
						prompt += ", or press enter to skip"
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
			line := fmt.Sprintf("%s Saved %s, a %s, as %s", c.Tick(), item.GetLabel(), kindWord(item.GetKind()), c.Paint(item.GetHandle(), cli.Bold))
			if origin := item.GetOrigin(); origin != "" {
				line += " for " + origin
			}
			c.Say(line + ".")
			return nil
		}}
	command.Flags().StringVar(&kind, "kind", "", "What to save: login, payment or address")
	return command
}

func (v vaultCLI) list(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{})
	if err != nil {
		return shown(c, err)
	}
	if len(listed.GetItems()) == 0 && len(listed.GetLocked()) == 0 {
		c.Say("The vault is empty. Add a login, card or address with icebath vault add, or let the agent ask you on the page.", cli.Dim)
		return nil
	}
	if items := listed.GetItems(); len(items) > 0 {
		c.Say(strs.Count(len(items), "item")+" in the vault", cli.Bold)
		var rows [][]string
		for _, item := range items {
			kind := kindWord(item.GetKind())
			rows = append(rows, []string{c.Paint(item.GetHandle(), cli.Bold), item.GetSource(), strings.ToUpper(kind[:1]) + kind[1:], item.GetLabel(),
				dash(item.GetIdentifier()), dash(item.GetOrigin())})
		}
		c.Table([]string{"Handle", "Kept in", "Kind", "Name", "Signs in as", "Site"}, rows)
		c.Say("Passwords are never shown. The agent names an item by its handle, and icebath fills in the password itself.", cli.Dim)
	}
	for _, name := range listed.GetLocked() {
		c.Say(c.Paint(name, cli.Yellow) + " is on but locked. The agent asks you to unlock it when it needs a login.")
	}
	return nil
}

func (v vaultCLI) rm() *cobra.Command {
	return &cobra.Command{Use: "rm <handle>", Short: "Remove an item from the vault",
		Long: "Removes the item with this handle, as vault list shows it.", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			removed, err := v.service.Remove(cmd.Context(), &vaultv1.RemoveRequest{Handle: args[0]})
			if err != nil {
				return shown(c, err)
			}
			if removed.GetRemoved() {
				c.Say(c.Tick() + " Removed " + args[0] + " from the vault.")
			} else {
				c.Say(fmt.Sprintf("✗ The vault has no item with the handle %q. Run icebath vault list to see the handles.", args[0]), cli.Red)
			}
			return nil
		}}
}

func (v vaultCLI) sources() *cobra.Command {
	var enable, disable string
	command := &cobra.Command{Use: "sources", Short: "Show the password managers the vault reads logins from",
		Long: "Shows the password managers icebath found, such as 1Password and Bitwarden. A password manager it finds is used at once. " +
			"--disable and --enable turn one off or on again.",
		Example: "  icebath vault sources --disable bitwarden",
		Args:    cobra.NoArgs,
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
						c.Say("✗ "+failure.Message(), cli.Red)
						return nil
					}
					return err
				}
				if on {
					c.Say(c.Tick() + " The agent can use logins from " + set.GetDisplayName() + " again.")
				} else {
					c.Say(c.Tick() + " The agent no longer uses logins from " + set.GetDisplayName() + ".")
				}
				return nil
			}
			listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{})
			if err != nil {
				return shown(c, err)
			}
			width := 0
			for _, source := range listed.GetSources() {
				width = max(width, cli.Width(source.GetDisplayName()))
			}
			for _, source := range listed.GetSources() {
				status := c.Paint("Not installed", cli.Dim)
				switch source.GetState() {
				case "on":
					status = c.Paint("On.", cli.Green) + " The agent asks you to unlock it when it needs a login."
				case "off":
					status = c.Paint("Off.", cli.Dim) + " Run icebath vault sources --enable " + source.GetName() + " to use it."
				}
				c.Say("  " + cli.Pad(source.GetDisplayName(), width) + "  " + status)
			}
			c.Say("icebath uses a password manager by itself once its command line tool is installed and signed in.", cli.Dim)
			return nil
		}}
	command.Flags().StringVar(&disable, "disable", "", "Password manager to stop reading logins from")
	command.Flags().StringVar(&enable, "enable", "", "Password manager to read logins from again")
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
		c.Say("The vault key is in the file " + c.Paint(key.GetPath(), cli.Bold) + ", next to the encrypted vault. Only you can read it.")
	case "keychain":
		c.Say(fmt.Sprintf("The vault key is in the keychain, under %q for %q.", key.GetService(), key.GetAccount()))
	default:
		c.Say("There is no vault key yet. icebath makes one when you save the first item.")
	}
	place := "a file"
	if key.GetConfigured() == "keychain" {
		place = "the keychain"
	}
	c.Say("New vaults keep their key in " + place + ". Key storage in /settings under Autofill vault changes that.")
	c.Say("Keychain of this computer: " + key.GetKeychain())
	return nil
}

func (v vaultCLI) migrate() *cobra.Command {
	var to string
	command := &cobra.Command{Use: "migrate", Short: "Move the key of the vault to the keychain or a file",
		Long: "Moves the key that unlocks the vault into the keychain of your system, or back into the vault.key file. The old copy is " +
			"removed only once the new one unlocks the vault.",
		Example: "  icebath vault key migrate --to keychain",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			if to != "keychain" && to != "file" {
				return fmt.Errorf("--to takes keychain or file, not “%s”.", to)
			}
			key, err := v.service.MoveKey(cmd.Context(), &vaultv1.MoveKeyRequest{To: to})
			if err != nil {
				return shown(c, err)
			}
			switch {
			case key.GetStorage() == "none":
				place := "the file vault.key"
				if to == "keychain" {
					place = "the keychain"
				}
				c.Say("There is no vault key yet. icebath makes one in " + place + " when you save the first item.")
			case to == "keychain":
				c.Say(c.Tick() + " The vault key is now in " + key.GetKeychain() + ". vault.key was deleted once the copy in the keychain opened the vault.")
				c.Say("Backups of this profile now hold the encrypted vault without its key. Before you move the profile to another computer, run "+
					"icebath vault key migrate --to file.", cli.Dim)
			default:
				c.Say(c.Tick() + " The vault key is now in " + key.GetPath() + ", which only you can read.")
				if left := key.GetKeychainLeft(); left != "" {
					c.Say(c.Paint("⚠ The copy in the keychain, "+left+", could not be removed yet.", cli.Yellow) + " Run this command again when the " +
						"keychain can be reached.")
				} else {
					c.Say("The keychain entry was removed.")
				}
			}
			return nil
		}}
	command.Flags().StringVar(&to, "to", "", "Where to keep the key: keychain or file")
	command.MarkFlagRequired("to")
	return command
}
