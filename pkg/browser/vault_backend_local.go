package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/vault"
)

// The always on local Fernet vault as a login backend
type LocalLoginBackend struct {
	LoginBackendDefaults
	ctx context.Context
}

// Local backend reading the vault store of the ctx home
func NewLocalLoginBackend(ctx context.Context) *LocalLoginBackend {
	b := &LocalLoginBackend{ctx: ctx}
	b.Self = b
	return b
}

// Vault store opener bound at call time, the facade hermes tests patch
var localVaultStore = func(ctx context.Context) *vault.VaultStore { return vault.GetVaultStore(ctx) }

func (*LocalLoginBackend) Name() string        { return "local" }
func (*LocalLoginBackend) DisplayName() string { return "Icebath vault" }
func (*LocalLoginBackend) Prefix() string      { return "vault_" }

func (b *LocalLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	return localVaultStore(b.ctx).ListItems()
}

func (b *LocalLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	return localVaultStore(b.ctx).GetMeta(handle)
}

func (b *LocalLoginBackend) ResolvePassword(handle string) (string, error) {
	secret, err := localVaultStore(b.ctx).ResolveSecret(handle)
	if err != nil {
		return "", err
	}
	v, _ := secret.Get("password")
	return vaultText(v), nil
}

// Current code of a login storing a TOTP seed, absent without a seed
func (b *LocalLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	secret, err := localVaultStore(b.ctx).ResolveSecret(handle)
	if err != nil {
		return "", false, err
	}
	v, _ := secret.Get("otp_secret")
	seed := vaultText(v)
	if seed == "" {
		return "", false, nil
	}
	code, err := vault.TOTPNow(seed)
	if err != nil {
		return "", false, err
	}
	return code, true, nil
}

// Every field of the item payload as text
func (b *LocalLoginBackend) ResolveSecret(handle string) (map[string]string, error) {
	secret, err := localVaultStore(b.ctx).ResolveSecret(handle)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, secret.Len())
	for _, k := range secret.Keys {
		out[k] = pyrepr.Str(secret.Vals[k])
	}
	return out, nil
}
