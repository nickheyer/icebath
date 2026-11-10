package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Line and hidden readers of the vault add wizard, swapped by tests
var (
	vaultReadLine   = func(prompt string) (string, error) { return cliui.Default().Input(prompt) }
	vaultReadSecret = func(prompt string) (string, error) { return cliui.Default().MaskedSecret(prompt) }
)

// Reads a stripped line, Ctrl-C or a closed stdin ending the command with exit 1
func vaultInput(prompt string) string {
	value, err := vaultReadLine(prompt)
	if err != nil {
		panic(vaultExit{1})
	}
	return strings.TrimSpace(value)
}

// Reads a hidden value, Ctrl-C or a closed stdin ending the command with exit 1
func vaultGetpass(prompt string) string {
	value, err := vaultReadSecret(prompt)
	if err != nil {
		panic(vaultExit{1})
	}
	return value
}

// The sys.exit of an interrupted prompt
type vaultExit struct{ code int }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func orDash(v any) string {
	if pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return "-"
}

// Interactive wizard storing one login, card or address in the local vault
func vaultCmdAdd(ctx context.Context, c *pyrich.Console, ns map[string]any) error {
	c.Print("[bold]Add a vault item[/] (the password is encrypted at rest and the agent never sees it; the identifier is visible metadata the agent can type itself)")
	kind := strings.ToLower(strings.TrimSpace(pyrepr.Str(ns["kind"])))
	if !pyrepr.Truthy(ns["kind"]) {
		kind = ""
	}
	for !slices.Contains(vault.VaultKinds, kind) {
		kind = strings.ToLower(vaultInput(fmt.Sprintf("Kind (%s) [login]: ", strings.Join(vault.VaultKinds, "/"))))
		if kind == "" {
			kind = "login"
		}
		if !slices.Contains(vault.VaultKinds, kind) {
			c.Print(fmt.Sprintf("[red]Unknown kind %s[/]", pyrepr.Repr(kind)))
			kind = ""
		}
	}
	label := ""
	for label == "" {
		label = vaultInput("Label (e.g. 'GitHub work account'): ")
	}
	var meta vault.VaultItemMeta
	var err error
	if kind == "login" {
		origin := ""
		for origin == "" {
			origin = vaultInput("Site origin (e.g. https://github.com): ")
		}
		idType := ""
		for !slices.Contains(vault.LoginIdentifierTypes, idType) {
			idType = strings.ToLower(vaultInput(fmt.Sprintf("Identifier type (%s) [email]: ", strings.Join(vault.LoginIdentifierTypes, "/"))))
			if idType == "" {
				idType = "email"
			}
		}
		identifier := ""
		for identifier == "" {
			identifier = vaultInput(capitalize(idType) + ": ")
		}
		password := ""
		for password == "" {
			password = vaultGetpass("Password (hidden): ")
		}
		otpSecret := vaultGetpass("Authenticator key (optional, hidden; the 2FA \"setup key\" or otpauth:// link — Enter to skip): ")
		secret := pyjson.ObjectOf("identifier_type", idType, "identifier", identifier, "password", password)
		if strings.TrimSpace(otpSecret) != "" {
			secret.Set("otp_secret", otpSecret)
		}
		meta, err = vault.GetVaultStore(ctx).AddItem("login", label, secret, origin)
	} else {
		fields := vault.AddressFields
		if kind == "payment" {
			fields = vault.PaymentFields
		}
		origin := ""
		for origin == "" {
			origin = vaultInput("Site origin the item may be filled on (e.g. https://shop.example.com): ")
		}
		c.Print(fmt.Sprintf("[dim]%s fields are filled only on that origin; card values are read hidden.[/]", kind))
		required := map[string]bool{}
		for _, f := range vault.RequiredFields(kind) {
			required[f] = true
		}
		secret := pyjson.NewObject()
		for _, field := range fields.Keys {
			prompt := strings.ReplaceAll(field, "_", " ")
			if !required[field] {
				prompt += " (optional)"
			}
			prompt += ": "
			read := vaultInput
			if kind == "payment" {
				read = func(p string) string { return strings.TrimSpace(vaultGetpass(p)) }
			}
			value := read(prompt)
			for required[field] && value == "" {
				value = read(prompt)
			}
			if value != "" {
				secret.Set(field, value)
			}
		}
		meta, err = vault.GetVaultStore(ctx).AddItem(kind, label, secret, origin)
	}
	var verr *vault.VaultError
	if errors.As(err, &verr) {
		c.Print(fmt.Sprintf("[red]Error:[/] %s", verr.Msg))
		return nil
	}
	if err != nil {
		return err
	}
	c.Print(fmt.Sprintf("[green]Stored.[/] handle=[bold]%s[/] kind=%s origin=%s", meta.ID, meta.Kind, orDash(meta.Origin)))
	return nil
}

// Lists local items and the unlocked external managers, locked ones named for the session
func vaultCmdList(ctx context.Context, c *pyrich.Console) error {
	backends, err := EnabledLoginBackends(ctx)
	if err != nil {
		return err
	}
	type row struct {
		source string
		meta   vault.VaultItemMeta
	}
	var rows []row
	var locked []string
	for _, backend := range backends {
		if backend.NeedsUnlock() && !backend.IsUnlocked() {
			locked = append(locked, backend.DisplayName())
			continue
		}
		items, err := backend.ListItems()
		if err != nil {
			return err
		}
		for _, meta := range items {
			rows = append(rows, row{backend.DisplayName(), meta})
		}
	}
	if len(rows) == 0 && len(locked) == 0 {
		c.Print("[dim]Vault is empty. Add an item with `icebath vault add`.[/]")
		return nil
	}
	if len(rows) > 0 {
		table := &pyrich.Table{Title: fmt.Sprintf("Vault items (%d)", len(rows))}
		for _, col := range []string{"Handle", "Source", "Kind", "Label", "Identifier", "Origin"} {
			style := ""
			if col == "Handle" {
				style = "bold"
			}
			table.Columns = append(table.Columns, pyrich.Column{Header: col, Style: style})
		}
		for _, r := range rows {
			table.AddRow(r.meta.ID, r.source, r.meta.Kind, r.meta.Label, orDash(r.meta.Identifier), orDash(r.meta.Origin))
		}
		c.PrintTable(table)
		c.Print("[dim]Passwords are never shown; the agent fills them server-side from the handle.[/]")
	}
	for _, name := range locked {
		c.Print(fmt.Sprintf("[yellow]%s[/] is enabled but locked — the agent will ask you to unlock it when it needs a login.", name))
	}
	return nil
}

// Nested mapping of a config slot, a missing or non mapping value replaced by an empty one
func ensureConfigDict(parent map[string]any, key string) map[string]any {
	switch child := parent[key].(type) {
	case map[string]any:
		return child
	case config.Tree:
		return map[string]any(child)
	}
	child := map[string]any{}
	parent[key] = child
	return child
}

// Shows the detected password managers, --enable and --disable flipping the opt out under vault.<name>.enabled
func vaultCmdSources(ctx context.Context, h string, c *pyrich.Console, ns map[string]any) error {
	classes := externalBackendClasses()
	names := make([]string, 0, len(classes))
	display := map[string]string{}
	for _, cls := range classes {
		names = append(names, cls.name)
		display[cls.name] = cls.displayName
	}
	enable, disable := pyrepr.Truthy(ns["enable"]), pyrepr.Truthy(ns["disable"])
	if enable || disable {
		name := pyrepr.Str(ns["disable"])
		if enable {
			name = pyrepr.Str(ns["enable"])
		}
		if _, known := display[name]; !known {
			c.Print(fmt.Sprintf("[red]Unknown password manager %s[/] (expected one of %s)", pyrepr.Repr(name), strings.Join(names, ", ")))
			return nil
		}
		cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
		if err != nil {
			return err
		}
		doc := map[string]any(cfg)
		section := ensureConfigDict(ensureConfigDict(doc, "vault"), name)
		if enable {
			delete(section, "enabled")
		} else {
			section["enabled"] = false
		}
		if err := config.SaveConfig(ctx, h, doc, config.SaveOptions{}); err != nil {
			return err
		}
		state := "off"
		if enable {
			state = "on"
		}
		c.Print(fmt.Sprintf("[green]%s %s[/] for browser logins.", display[name], state))
		return nil
	}
	backends, err := EnabledLoginBackends(ctx)
	if err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, b := range backends {
		enabled[b.Name()] = true
	}
	for _, cls := range classes {
		var status string
		switch {
		case enabled[cls.name]:
			status = "[green]detected[/] · the agent asks you to unlock it when it needs a login"
		default:
			installed, err := IsLoginBackendInstalled(ctx, cls.name)
			if err != nil {
				return err
			}
			if installed {
				status = fmt.Sprintf("[dim]turned off[/] (`icebath vault sources --enable %s` to use it)", cls.name)
			} else {
				status = "[dim]not installed[/]"
			}
		}
		c.Print(fmt.Sprintf("  %-10s %s", cls.displayName, status))
	}
	c.Print("[dim]Managers are picked up automatically when their CLI is installed and signed in.[/]")
	return nil
}

// Removes a local item by handle
func vaultCmdRm(ctx context.Context, c *pyrich.Console, ns map[string]any) error {
	handle := pyrepr.Str(ns["handle"])
	removed, err := vault.GetVaultStore(ctx).RemoveItem(handle)
	if err != nil {
		return err
	}
	if removed {
		c.Print(fmt.Sprintf("[green]Removed[/] %s", handle))
	} else {
		c.Print(fmt.Sprintf("[red]No vault item with handle %s[/]", pyrepr.Repr(handle)))
	}
	return nil
}

// Records vault.key_storage in config.yaml
func setVaultKeyStorage(ctx context.Context, h, value string) error {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	doc := map[string]any(cfg)
	ensureConfigDict(doc, "vault")["key_storage"] = value
	return config.SaveConfig(ctx, h, doc, config.SaveOptions{})
}

func keyLocationText(where *pyjson.Object, key string) string {
	v, _ := where.Get(key)
	return pyrepr.Str(v)
}

// Shows where the local vault key lives, or moves it between vault.key and the OS keychain
func vaultCmdKey(ctx context.Context, h string, c *pyrich.Console, ns map[string]any) error {
	store := vault.GetVaultStore(ctx)
	target := ""
	if pyrepr.Truthy(ns["to"]) {
		target = pyrepr.Str(ns["to"])
	}
	if target != "" {
		if _, err := store.MigrateKey(target); err != nil {
			return err
		}
		if err := setVaultKeyStorage(ctx, h, target); err != nil {
			return err
		}
		where, err := store.KeyLocation()
		if err != nil {
			return err
		}
		switch {
		case keyLocationText(where, "storage") == "none":
			place := "vault.key"
			if target == "keychain" {
				place = "the OS keychain"
			}
			c.Print(fmt.Sprintf("No vault key exists yet. The first saved item creates one in %s (vault.key_storage = %s).", place, target))
		case target == "keychain":
			c.Print(fmt.Sprintf("[green]Vault key moved to the OS keychain[/] (%s); vault.key was removed after the keychain copy decrypted the vault.", vault.KeychainBackendLabel()))
			c.Print("[dim]Backups of this profile now hold the encrypted vault without its key; `icebath vault key migrate --to file` before moving the profile to another machine.[/]")
		default:
			c.Print(fmt.Sprintf("[green]Vault key moved to[/] %s (0600).", keyLocationText(where, "path")))
			if left, has := where.Get("keychain_entry_left"); has && pyrepr.Truthy(left) {
				c.Print(fmt.Sprintf("[yellow]The keychain copy (%s) could not be removed yet;[/] run this command again when the keychain is reachable.", pyrepr.Str(left)))
			} else {
				c.Print("Keychain entry removed.")
			}
		}
		return nil
	}
	where, err := store.KeyLocation()
	if err != nil {
		return err
	}
	switch keyLocationText(where, "storage") {
	case "file":
		c.Print(fmt.Sprintf("Vault key: [bold]file[/] %s (0600, beside the encrypted vault)", keyLocationText(where, "path")))
	case "keychain":
		service, _ := where.Get("service")
		account, _ := where.Get("account")
		c.Print(fmt.Sprintf("Vault key: [bold]OS keychain[/] service %s, account %s", pyrepr.Repr(service), pyrepr.Repr(account)))
	default:
		c.Print("Vault key: [dim]none yet[/] (created with the first saved item)")
	}
	c.Print(fmt.Sprintf("New vaults use: vault.key_storage = %s", vault.ConfiguredKeyStorage(ctx, h)))
	c.Print(fmt.Sprintf("OS keychain here: %s", vault.KeychainBackendLabel()))
	return nil
}

// VaultCommand runs icebath vault, hermes_cli/vault.py vault_command dispatching add, list, rm, sources and key
func VaultCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (code int, err error) {
	c := pyrich.NewConsole(w)
	defer func() {
		if r := recover(); r != nil {
			if exit, ok := r.(vaultExit); ok {
				code = exit.code
				return
			}
			panic(r)
		}
	}()
	action := ""
	if pyrepr.Truthy(ns["vault_action"]) {
		action = pyrepr.Str(ns["vault_action"])
	}
	switch action {
	case "", "list":
		err = vaultCmdList(ctx, c)
	case "add":
		err = vaultCmdAdd(ctx, c, ns)
	case "rm":
		err = vaultCmdRm(ctx, c, ns)
	case "sources":
		err = vaultCmdSources(ctx, h, c, ns)
	case "key":
		err = vaultCmdKey(ctx, h, c, ns)
	default:
		return 2, fmt.Errorf("unknown vault action: %s", action)
	}
	var verr *vault.VaultError
	if errors.As(err, &verr) {
		c.Print(fmt.Sprintf("[red]Error:[/] %s", verr.Msg))
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
