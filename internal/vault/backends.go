package vault

import (
	"context"
	"strings"
	"time"
)

// Names and handle prefixes of the built in login backends
const (
	localName       = "local"
	localPrefix     = "vault_"
	onePasswordName = "onepassword"
	onePasswordPfx  = "op:"
	bitwardenName   = "bitwarden"
	bitwardenPrefix = "bw:"
)

// Login source the browser fills from, listing metadata only and resolving one secret at fill time
type LoginBackend interface {
	Name() string
	DisplayName() string
	Prefix() string
	NeedsUnlock() bool
	Owns(handle string) bool
	Unlocked(ctx context.Context) bool
	List(ctx context.Context) ([]ItemMeta, error)
	Meta(ctx context.Context, handle string) (ItemMeta, bool, error)
	ResolvePassword(ctx context.Context, handle string) (Secret, error)
	ResolveOTP(ctx context.Context, handle string) (Secret, error)
	ResolveSecret(ctx context.Context, handle string) (Secret, error)
	Fill(ctx context.Context, fill func(context.Context) error) error
	Rereads() bool
}

// Login backend the user unlocks for the session with a master password the manager CLI consumes
type Unlocker interface {
	Unlock(ctx context.Context, masterPassword string) error
}

// Error a locked password manager returns until the user unlocks it for this session
type UnlockRequiredError struct {
	Backend     string
	DisplayName string
}

// Names the locked manager
func (e *UnlockRequiredError) Error() string { return e.DisplayName + " is locked" }

// Status row of one login source for the settings surfaces
type Source struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	NeedsUnlock bool   `json:"needs_unlock"`
	Unlocked    bool   `json:"unlocked"`
	Installed   bool   `json:"installed"`
}

// Local backend first, then every installed manager not turned off, then the plugin backends the profile enabled
func (v *Vault) Backends(ctx context.Context, opts Options) ([]LoginBackend, error) {
	store, err := v.Store(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := []LoginBackend{&localBackend{store: store}}
	if !opts.OnePassword.Disabled && managerInstalled(onePasswordName, opts.OnePassword.BinaryPath) {
		out = append(out, v.newOnePassword(ctx, opts.OnePassword))
	}
	if !opts.Bitwarden.Disabled && managerInstalled(bitwardenName, opts.Bitwarden.BinaryPath) {
		out = append(out, &bitwarden{vault: v, opts: opts.Bitwarden})
	}
	plugins, err := v.pluginBackends(ctx, opts)
	if err != nil {
		return nil, err
	}
	return append(out, plugins...), nil
}

// Enabled backend owning handle, false when none does
func (v *Vault) Backend(ctx context.Context, opts Options, handle string) (LoginBackend, bool, error) {
	backends, err := v.Backends(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	for _, b := range backends {
		if b.Owns(handle) {
			return b, true, nil
		}
	}
	return nil, false, nil
}

// Status of the local vault and of each password manager whether or not it is enabled
func (v *Vault) Sources(ctx context.Context, opts Options) ([]Source, error) {
	backends, err := v.Backends(ctx, opts)
	if err != nil {
		return nil, err
	}
	live := map[string]LoginBackend{}
	for _, b := range backends {
		live[b.Name()] = b
	}
	rows := []Source{{Name: localName, DisplayName: localDisplayName, Enabled: true, Unlocked: true, Installed: true}}
	for _, m := range []struct{ name, display, binary string }{
		{onePasswordName, onePasswordDisplay, opts.OnePassword.BinaryPath},
		{bitwardenName, bitwardenDisplay, opts.Bitwarden.BinaryPath},
	} {
		b, enabled := live[m.name]
		rows = append(rows, Source{Name: m.name, DisplayName: m.display, Enabled: enabled, NeedsUnlock: true,
			Unlocked: enabled && b.Unlocked(ctx), Installed: managerInstalled(m.name, m.binary)})
	}
	return rows, nil
}

// Name the local vault backend is shown by
const localDisplayName = "icebath vault"

// Local encrypted vault as the always on login backend
type localBackend struct {
	store *Store
}

// Config key of the local backend
func (*localBackend) Name() string { return localName }

// Name shown for the local vault
func (*localBackend) DisplayName() string { return localDisplayName }

// Handle prefix of local items
func (*localBackend) Prefix() string { return localPrefix }

// Reports false, the local vault never asks for a master password
func (*localBackend) NeedsUnlock() bool { return false }

// Reports whether handle names a local item
func (*localBackend) Owns(handle string) bool { return strings.HasPrefix(handle, localPrefix) }

// Reports true, the local vault is always open
func (*localBackend) Unlocked(context.Context) bool { return true }

// Metadata of every local item
func (b *localBackend) List(ctx context.Context) ([]ItemMeta, error) { return b.store.list(ctx) }

// Metadata of the local item carrying handle
func (b *localBackend) Meta(ctx context.Context, handle string) (ItemMeta, bool, error) {
	return b.store.meta(ctx, handle)
}

// Password of the local item, empty when it holds none
func (b *localBackend) ResolvePassword(ctx context.Context, handle string) (Secret, error) {
	secret, err := b.store.secret(ctx, handle)
	if err != nil {
		return Secret{}, err
	}
	return secretOf(secretField{PasswordField, truthyText(lookup(secret, PasswordField))}), nil
}

// Current code from the stored authenticator key, no code when the item has none
func (b *localBackend) ResolveOTP(ctx context.Context, handle string) (Secret, error) {
	secret, err := b.store.secret(ctx, handle)
	if err != nil {
		return Secret{}, err
	}
	seed := truthyText(lookup(secret, "otp_secret"))
	if seed == "" {
		return Secret{}, nil
	}
	code, err := totp(seed, time.Now())
	if err != nil {
		return Secret{}, err
	}
	return secretOf(secretField{CodeField, code}), nil
}

// Every stored field of the local item as text
func (b *localBackend) ResolveSecret(ctx context.Context, handle string) (Secret, error) {
	secret, err := b.store.secret(ctx, handle)
	if err != nil {
		return Secret{}, err
	}
	fields := make([]secretField, len(secret))
	for i, m := range secret {
		fields[i] = secretField{m.name, valueText(m.value)}
	}
	return secretOf(fields...), nil
}

// Secret carrying a manager's one time code, no code unless the output is all digits
func codeSecret(code string) Secret {
	if !allDigits(code) {
		return Secret{}
	}
	return secretOf(secretField{CodeField, code})
}

// Runs fill directly, the local vault holding no context of its own
func (*localBackend) Fill(ctx context.Context, fill func(context.Context) error) error {
	return fill(ctx)
}

// Reports false, the local vault freezes nothing a fill must re-read
func (*localBackend) Rereads() bool { return false }
