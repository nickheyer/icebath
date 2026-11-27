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
	// Fix of an op program that is not there
	InstallHint = "Install the 1Password command line tool from " + onepasswordconfig.Docs + ", or set op program in /settings under Secret " +
		"stores › 1Password to where it is."
	authRemedy = "Run icebath secrets onepassword token to paste a new service account token for {token_env}, or run op signin to sign in."
	emptyHint  = "Add one with icebath secrets onepassword set, such as icebath secrets onepassword set OPENAI_API_KEY op://Private/OpenAI/credential."
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
		Hints: map[secrets.Failure]string{secrets.AuthFailed: authRemedy, secrets.AuthExpired: authRemedy, secrets.BinaryMissing: InstallHint,
			secrets.NotConfigured: emptyHint}}
}

// Variable holding the service account token for the profile
func (s *Store) TokenEnv(cfg *config.Config) string {
	return s.Host().TokenEnv(cfg, s.Spec())
}

// Why the op program the settings name is not there
func Missing(pinned string) string {
	if pinned != "" {
		return fmt.Sprintf("%s, the op program set in /settings under Secret stores › 1Password, is not a program icebath can run.", pinned)
	}
	return "1Password is on, but op, the 1Password command line tool, is not installed."
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
			return secrets.Failed("1Password is on, but no variable is set from it yet.", secrets.NotConfigured)
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
