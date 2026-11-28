// The vault commands: add, list and remove items of the profile's own vault, the password managers it reads, and where its key lives
package cli

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	picked "github.com/nickheyer/icebath/internal/profile"
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
	key.AddCommand(v.move())
	root.AddCommand(v.add(), &cobra.Command{Use: "list", Short: "List what the vault holds",
		Long: "Lists every item of the vault with its handle, kind and website, but never a password or card number.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return v.list(cmd) }}, v.change(), v.remove(), v.managers(), v.turn(true), v.turn(false),
		v.unlock(), v.lock(), key)
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
	return strs.Capital(vault.FieldName(key))
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
		Long: "Asks for a login, card or address and saves it in the vault. You do not have to, because the agent asks you on the page when it " +
			"needs one. The password is encrypted and the agent never sees it.",
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
				if values["identifier"], err = v.insist(c, strs.Capital(idType)+": ", false); err != nil {
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
			added, err := v.service.Add(cmd.Context(), &vaultv1.AddRequest{Profile: picked.Picked(cmd.Context()), Kind: kind, Label: label, Origin: origin, Values: values})
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
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{Profile: picked.Picked(cmd.Context())})
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
			rows = append(rows, []string{c.Paint(item.GetHandle(), cli.Bold), item.GetSource(), strs.Capital(kind), item.GetLabel(),
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

func (v vaultCLI) remove() *cobra.Command {
	return &cobra.Command{Use: "remove <handle>", Aliases: []string{"rm"}, Short: "Remove an item from the vault",
		Long: "Removes the item with this handle, as vault list shows it. What it holds is gone for good.", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			removed, err := v.service.Remove(cmd.Context(), &vaultv1.RemoveRequest{Profile: picked.Picked(cmd.Context()), Handle: args[0]})
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

// Fields of an item a person names on the command line, by the word for each: its name, its site, and the fields of the kinds
var fieldNames = map[string]string{"name": "label", "site": "origin"}

func (v vaultCLI) change() *cobra.Command {
	return &cobra.Command{Use: "change <handle> <field>", Short: "Change one field of an item",
		Long: "Asks for the new value of one field of the item with this handle, hidden as you type for a secret one. The field is name, site, " +
			"or one of its kind: identifier_type, identifier, password, otp_secret, card_number, cardholder_name, exp_month, exp_year, cvc, " +
			"billing_postal_code, address_line1, address_line2, city, state, postal_code or country. Press enter on an empty answer to take an " +
			"optional field away.",
		Example: "  icebath vault change vault_0123456789ab password",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			handle, field := args[0], strings.ToLower(args[1])
			if named, ok := fieldNames[field]; ok {
				field = named
			}
			listed, err := v.service.Item(cmd.Context(), &vaultv1.ItemRequest{Profile: picked.Picked(cmd.Context()), Handle: handle})
			if err != nil {
				return shown(c, err)
			}
			secret := false
			for _, value := range listed.GetValues() {
				if value.GetField().GetKey() == field {
					secret = value.GetField().GetSecret()
				}
			}
			prompt := "New " + map[string]string{"label": "name", "origin": "site"}[field]
			if prompt == "New " {
				prompt += vault.FieldName(field)
			}
			if secret {
				prompt += ", hidden as you type"
			}
			value, err := v.ask(c, prompt+": ", secret)
			if err != nil {
				return err
			}
			changed, err := v.service.Change(cmd.Context(), &vaultv1.ChangeRequest{Profile: picked.Picked(cmd.Context()), Handle: handle, Field: field, Value: value})
			if err != nil {
				return shown(c, err)
			}
			c.Say(c.Tick() + " Changed " + strings.TrimPrefix(prompt, "New ") + " of " + changed.GetItem().GetLabel() + ".")
			return nil
		}}
}

// State of a password manager in the words the page says it in
func managerWords(c *cli.Console, source *vaultv1.Source) string {
	switch {
	case source.GetState() == "on" && source.GetLocked():
		return c.Paint("In use", cli.Green) + ", locked. The agent asks you to unlock it when it needs a login."
	case source.GetState() == "on":
		return c.Paint("In use", cli.Green)
	case source.GetState() == "off":
		return c.Paint("Off", cli.Dim)
	}
	return c.Paint("Not installed", cli.Dim)
}

func (v vaultCLI) managers() *cobra.Command {
	return &cobra.Command{Use: "managers", Aliases: []string{"sources"}, Short: "Show the password managers the vault reads logins from",
		Long: "Shows the password managers icebath knows, such as 1Password and Bitwarden, and whether each is in use. A password manager " +
			"whose program is installed is in use until you turn it off.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return shown(c, err)
			}
			width := 0
			for _, source := range listed.GetSources() {
				width = max(width, cli.Width(source.GetDisplayName()))
			}
			for _, source := range listed.GetSources() {
				c.Say("  " + cli.Pad(source.GetDisplayName(), width) + "  " + managerWords(c, source))
			}
			c.Say("icebath vault on and icebath vault off turn one on or off, by its name: "+names(listed.GetSources())+".", cli.Dim)
			return nil
		}}
}

// Names of the password managers, for a sentence
func names(sources []*vaultv1.Source) string {
	var out []string
	for _, source := range sources {
		out = append(out, source.GetName())
	}
	return strings.Join(out, ", ")
}

// Turns a password manager on or off, as its page does
func (v vaultCLI) turn(on bool) *cobra.Command {
	use, short := "on <manager>", "Let the agent use the logins of a password manager"
	if !on {
		use, short = "off <manager>", "Stop the agent using the logins of a password manager"
	}
	return &cobra.Command{Use: use, Short: short, Example: "  icebath vault " + strings.Fields(use)[0] + " bitwarden", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			set, err := v.service.SetSource(cmd.Context(), &vaultv1.SetSourceRequest{Profile: picked.Picked(cmd.Context()), Name: args[0], On: on})
			if err != nil {
				return shown(c, err)
			}
			if on {
				c.Say(c.Tick() + " " + set.GetDisplayName() + " is now In use.")
			} else {
				c.Say(c.Tick() + " " + set.GetDisplayName() + " is now Off.")
			}
			return nil
		}}
}

func (v vaultCLI) unlock() *cobra.Command {
	return &cobra.Command{Use: "unlock <manager>", Short: "Unlock a password manager with its master password",
		Long: "Asks for the master password of the password manager, hidden as you type, and unlocks it on the server that icebath connect " +
			"set, until that server sits idle. The server keeps the unlock in memory only. An icebath that runs its own server is unlocked " +
			"in the app, with /vault.",
		Example: "  icebath vault unlock bitwarden",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			// An unlock is kept in the memory of the server that made it, which for a command on its own ends with the command
			if cli.OwnServer(cmd.Context()) {
				return c.Fail("✗ An unlock lasts as long as the icebath that holds it, and this command runs on its own, so the unlock would end " +
					"with it. Unlock " + args[0] + " in the app with /vault, or reach your server with icebath connect first.")
			}
			if !c.Interactive() {
				return c.Fail("✗ There is no terminal to type the master password in.")
			}
			master, err := v.ask(c, "Master password, hidden as you type: ", true)
			if err != nil {
				return err
			}
			if _, err := v.service.Unlock(cmd.Context(), &vaultv1.UnlockRequest{Profile: picked.Picked(cmd.Context()), Name: args[0], MasterPassword: master}); err != nil {
				return shown(c, err)
			}
			c.Say(c.Tick() + " " + v.title(cmd, args[0]) + " is unlocked until the server sits idle.")
			return nil
		}}
}

// Name a password manager is shown by, the name given when the vault can't be read
func (v vaultCLI) title(cmd *cobra.Command, name string) string {
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{Profile: picked.Picked(cmd.Context()), Bare: true})
	if err != nil {
		return name
	}
	for _, source := range listed.GetSources() {
		if source.GetName() == name {
			return cmp.Or(source.GetDisplayName(), name)
		}
	}
	return name
}

func (v vaultCLI) lock() *cobra.Command {
	return &cobra.Command{Use: "lock [manager]", Short: "Lock a password manager, or every one",
		Long: "Forgets the unlock of the password manager, or of every one when none is named, on the server that icebath connect set, so the " +
			"agent asks you again when it needs a login. An icebath that runs its own server is locked in the app, with /vault.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			// A command on its own holds no unlock: each icebath keeps its own in memory
			if cli.OwnServer(cmd.Context()) {
				return c.Fail("✗ This command runs on its own and holds no unlock to forget. Lock a password manager in the app with /vault, or " +
					"reach your server with icebath connect first.")
			}
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if _, err := v.service.Lock(cmd.Context(), &vaultv1.LockRequest{Profile: picked.Picked(cmd.Context()), Name: name}); err != nil {
				return shown(c, err)
			}
			if name == "" {
				c.Say(c.Tick() + " Every password manager is locked.")
			} else {
				c.Say(c.Tick() + " " + v.title(cmd, name) + " is locked.")
			}
			return nil
		}}
}

func (v vaultCLI) key(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	listed, err := v.service.List(cmd.Context(), &vaultv1.ListRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return shown(c, err)
	}
	key := listed.GetKey()
	switch key.GetStorage() {
	case "file":
		c.Say("The vault key is in the file " + c.Paint(key.GetPath(), cli.Bold) + ", next to the encrypted vault. Only you can read it.")
	case "keychain":
		c.Say(fmt.Sprintf("The vault key is in the keychain, under %q for %q.", key.GetService(), key.GetAccount()))
	case "none":
		place := "a file"
		if key.GetConfigured() == "keychain" {
			place = "the keychain"
		}
		c.Say("There is no vault key yet. icebath makes one in " + place + " when you save the first item.")
	}
	c.Say("icebath vault key move --to keychain or --to file moves it.", cli.Dim)
	c.Say("Keychain of this computer: " + key.GetKeychain())
	return nil
}

func (v vaultCLI) move() *cobra.Command {
	var to string
	command := &cobra.Command{Use: "move", Aliases: []string{"migrate"}, Short: "Move the key of the vault to the keychain or a file",
		Long: "Moves the key that unlocks the vault into the keychain of your system, or back into the vault.key file. The old copy is " +
			"removed only once the new one unlocks the vault. Before there is a key, this says where the first one is made.",
		Example: "  icebath vault key move --to keychain",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			if to != "keychain" && to != "file" {
				return fmt.Errorf("--to takes keychain or file, not “%s”.", to)
			}
			key, err := v.service.MoveKey(cmd.Context(), &vaultv1.MoveKeyRequest{Profile: picked.Picked(cmd.Context()), To: to})
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
					"icebath vault key move --to file.", cli.Dim)
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
