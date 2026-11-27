// 1Password as a store of a profile's secrets: variables bound to op:// references, resolved through the op program
package secrets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	"github.com/nickheyer/icebath/internal/secrets"
)

const (
	missingHint = "Install the 1Password CLI (" + onepasswordconfig.Docs + ") or set secrets.onepassword.binary_path."
	authRemedy  = "Run `icebath secrets onepassword token` to paste a fresh service-account token ({token_env}), or `op signin` for an interactive session."
)

func Register(registry *secrets.Registry) *Store {
	store := &Store{registry: registry, cache: &secrets.Cache{Name: cacheName}}
	registry.Register(secrets.Sources{store})
	return store
}

// Host the store's source is registered in
func (s *Store) Host() *secrets.Secrets {
	return s.registry.Host()
}

func (s *Store) Spec() secrets.Spec {
	return secrets.Spec{Name: onepasswordconfig.Name, Label: "1Password", Short: "1Password", Shape: secrets.Mapped, Scheme: "op",
		TokenSetting: onepasswordconfig.TokenSetting, TokenEnv: onepasswordconfig.DefaultTokenEnv,
		Hints: map[secrets.Failure]string{secrets.AuthFailed: authRemedy, secrets.AuthExpired: authRemedy, secrets.BinaryMissing: missingHint}}
}

// Variable holding the service account token for the profile
func (s *Store) TokenEnv(cfg *config.Config) string {
	return s.Host().TokenEnv(cfg, s.Spec())
}

// Why the op program the settings name is not there
func Missing(pinned string) string {
	if pinned != "" {
		return fmt.Sprintf("secrets.onepassword.binary_path (%q) is not an executable op binary.", pinned)
	}
	return "secrets.onepassword.enabled is true but the op CLI was not found on PATH.  Install it (" + onepasswordconfig.Docs +
		") or set secrets.onepassword.binary_path."
}

// Seconds values are reused for by the settings
func TTL(cfg *config.Config) time.Duration {
	return time.Duration(max(cfg.GetInt(onepasswordconfig.CacheTTL), 0)) * time.Second
}

func (s *Store) Fetch(ctx context.Context, f secrets.Fetch) secrets.Result {
	cfg := f.Config
	valid, warnings := Validate(References(cfg))
	if len(valid) == 0 {
		if len(warnings) == 0 {
			return secrets.Failed("secrets.onepassword.enabled is true but the env: map is empty.  Add ENV_VAR: op://vault/item/field entries.", secrets.NotConfigured)
		}
		return secrets.Result{Secrets: map[string]string{}, Warnings: warnings}
	}
	pinned := strings.TrimSpace(cfg.GetString(onepasswordconfig.Binary))
	op := Find(pinned)
	if op == "" {
		result := secrets.Failed(Missing(pinned), secrets.BinaryMissing)
		result.Warnings = warnings
		return result
	}
	got, more, err := s.Pull(ctx, Pull{References: valid, Account: strings.TrimSpace(cfg.GetString(onepasswordconfig.Account)), TokenEnv: s.TokenEnv(cfg),
		Binary: op, Dir: cfg.ProfileDir(cfg.Profile()), TTL: TTL(cfg), Cache: !f.Fresh, Env: f.Env})
	if err != nil {
		result := secrets.Failed(err.Error(), secrets.Classify(err.Error(), rules))
		result.Warnings, result.Binary = warnings, op
		return result
	}
	return secrets.Result{Secrets: got, Warnings: append(warnings, more...), Binary: op}
}
