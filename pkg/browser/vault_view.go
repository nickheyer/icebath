package browser

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/vault"
)

// VaultItemView is one item the agent may fill in, never its secret
type VaultItemView struct {
	Handle, Source, Kind, Label, Identifier, Origin string
	// Kept in the vault of this profile, so it can be removed here
	Local bool
}

// VaultSourceView is one password manager and whether the agent uses it: on, off or missing
type VaultSourceView struct{ Name, DisplayName, State string }

// VaultKeyView is where the key of the local vault lives
type VaultKeyView struct {
	// file, keychain, or none while no item was saved yet
	Storage                string
	Path, Service, Account string
	// Where the key of a new vault goes, file or keychain
	Configured string
	// Keychain of this computer
	Keychain string
	// Keychain copy that a move back to the file could not remove yet
	KeychainLeft string
}

// VaultFieldView is one field of an item of a kind besides its label and site
type VaultFieldView struct {
	Key              string
	Required, Secret bool
	// Only values the field takes, empty for free text
	Choices []string
}

// One listed item with the backend it comes from
type vaultListRow struct {
	source string
	local  bool
	meta   vault.VaultItemMeta
}

// Items of every enabled backend that is unlocked, local ones first, and the names of the managers that are locked
func vaultListing(ctx context.Context) ([]vaultListRow, []string, error) {
	backends, err := EnabledLoginBackends(ctx)
	if err != nil {
		return nil, nil, err
	}
	var rows []vaultListRow
	var locked []string
	for i, backend := range backends {
		if backend.NeedsUnlock() && !backend.IsUnlocked() {
			locked = append(locked, backend.DisplayName())
			continue
		}
		items, err := backend.ListItems()
		if err != nil {
			return nil, nil, err
		}
		for _, meta := range items {
			rows = append(rows, vaultListRow{source: backend.DisplayName(), local: i == 0, meta: meta})
		}
	}
	return rows, locked, nil
}

// VaultItems is every item the agent may fill in and the password managers that are locked
func VaultItems(ctx context.Context) ([]VaultItemView, []string, error) {
	rows, locked, err := vaultListing(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]VaultItemView, 0, len(rows))
	for _, row := range rows {
		out = append(out, VaultItemView{Handle: row.meta.ID, Source: row.source, Kind: row.meta.Kind, Label: row.meta.Label,
			Identifier: vaultText(row.meta.Identifier), Origin: vaultText(row.meta.Origin), Local: row.local})
	}
	return out, locked, nil
}

// VaultKindFields is the fields of an item of a kind in the order they are asked, nil for an unknown kind
func VaultKindFields(kind string) []VaultFieldView {
	if kind == "login" {
		return []VaultFieldView{{Key: "identifier_type", Required: true, Choices: slices.Clone(vault.LoginIdentifierTypes)}, {Key: "identifier", Required: true},
			{Key: "password", Required: true, Secret: true}, {Key: "otp_secret", Secret: true}}
	}
	fields := vault.AddressFields
	switch kind {
	case "payment":
		fields = vault.PaymentFields
	case "address":
	default:
		return nil
	}
	required := vault.RequiredFields(kind)
	out := make([]VaultFieldView, 0, len(fields.Keys))
	for _, key := range fields.Keys {
		out = append(out, VaultFieldView{Key: key, Required: slices.Contains(required, key), Secret: kind == "payment"})
	}
	return out
}

// Stores an item of a kind in the local vault from its values by field, fields left empty staying out
func addVaultItem(ctx context.Context, kind, label, origin string, values map[string]string) (vault.VaultItemMeta, error) {
	secret := pyjson.NewObject()
	for _, field := range VaultKindFields(kind) {
		if value := values[field.Key]; strings.TrimSpace(value) != "" {
			secret.Set(field.Key, value)
		}
	}
	return vault.GetVaultStore(ctx).AddItem(kind, label, secret, origin)
}

// AddVaultItem stores a login, card or address in the local vault from its values by field
func AddVaultItem(ctx context.Context, kind, label, origin string, values map[string]string) (VaultItemView, error) {
	if !slices.Contains(vault.VaultKinds, kind) {
		return VaultItemView{}, &vault.VaultError{Msg: "unknown kind of vault item: " + kind}
	}
	for _, field := range VaultKindFields(kind) {
		if field.Required && strings.TrimSpace(values[field.Key]) == "" {
			return VaultItemView{}, &vault.VaultError{Msg: "missing " + strings.ReplaceAll(field.Key, "_", " ")}
		}
	}
	meta, err := addVaultItem(ctx, kind, strings.TrimSpace(label), strings.TrimSpace(origin), values)
	if err != nil {
		return VaultItemView{}, err
	}
	return VaultItemView{Handle: meta.ID, Kind: meta.Kind, Label: meta.Label, Identifier: vaultText(meta.Identifier), Origin: vaultText(meta.Origin), Local: true}, nil
}

// RemoveVaultItem removes an item of the local vault by handle and reports whether there was one
func RemoveVaultItem(ctx context.Context, handle string) (bool, error) {
	return vault.GetVaultStore(ctx).RemoveItem(handle)
}

// VaultSources is every password manager icebath knows and whether the agent uses it
func VaultSources(ctx context.Context) ([]VaultSourceView, error) {
	backends, err := EnabledLoginBackends(ctx)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, b := range backends {
		enabled[b.Name()] = true
	}
	classes := externalBackendClasses()
	out := make([]VaultSourceView, 0, len(classes))
	for _, cls := range classes {
		view := VaultSourceView{Name: cls.name, DisplayName: cls.displayName, State: "on"}
		if !enabled[cls.name] {
			installed, err := IsLoginBackendInstalled(ctx, cls.name)
			if err != nil {
				return nil, err
			}
			view.State = "missing"
			if installed {
				view.State = "off"
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// Turns a password manager on or off for browser logins in the config of the home, on being the absence of the opt out
func setVaultSource(ctx context.Context, h, name string, on bool) error {
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	doc := map[string]any(cfg)
	section := ensureConfigDict(ensureConfigDict(doc, "vault"), name)
	if on {
		delete(section, "enabled")
	} else {
		section["enabled"] = false
	}
	return config.SaveConfig(ctx, h, doc, config.SaveOptions{})
}

// SetVaultSource turns a password manager icebath knows on or off for browser logins
func SetVaultSource(ctx context.Context, h, name string, on bool) error {
	if !slices.ContainsFunc(externalBackendClasses(), func(cls externalLoginBackend) bool { return cls.name == name }) {
		return &vault.VaultError{Msg: "unknown password manager: " + name}
	}
	return setVaultSource(ctx, h, name, on)
}

// Where the key lives, read from the location the store reports
func vaultKeyView(ctx context.Context, h string, where *pyjson.Object) VaultKeyView {
	view := VaultKeyView{Storage: keyLocationText(where, "storage"), Configured: vault.ConfiguredKeyStorage(ctx, h), Keychain: vault.KeychainBackendLabel()}
	for key, into := range map[string]*string{"path": &view.Path, "service": &view.Service, "account": &view.Account, "keychain_entry_left": &view.KeychainLeft} {
		if v, has := where.Get(key); has && pyrepr.Truthy(v) {
			*into = pyrepr.Str(v)
		}
	}
	return view
}

// VaultKey is where the key of the local vault lives
func VaultKey(ctx context.Context, h string) (VaultKeyView, error) {
	where, err := vault.GetVaultStore(ctx).KeyLocation()
	if err != nil {
		return VaultKeyView{}, err
	}
	return vaultKeyView(ctx, h, where), nil
}

// Moves the vault key to the keychain or back to its file, records the choice for new vaults and answers where the key lives after it
func moveVaultKey(ctx context.Context, h, target string) (*pyjson.Object, error) {
	store := vault.GetVaultStore(ctx)
	if _, err := store.MigrateKey(target); err != nil {
		return nil, err
	}
	if err := setVaultKeyStorage(ctx, h, target); err != nil {
		return nil, err
	}
	return store.KeyLocation()
}

// MoveVaultKey moves the vault key to the keychain or back to its file
func MoveVaultKey(ctx context.Context, h, target string) (VaultKeyView, error) {
	if target != "keychain" && target != "file" {
		return VaultKeyView{}, &vault.VaultError{Msg: "the vault key lives in the keychain or in a file, not in " + target}
	}
	where, err := moveVaultKey(ctx, h, target)
	if err != nil {
		return VaultKeyView{}, err
	}
	return vaultKeyView(ctx, h, where), nil
}
