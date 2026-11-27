// Bitwarden Secrets Manager as a store of a profile's secrets: every secret of one project, pulled through bws
package secrets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	"github.com/nickheyer/icebath/internal/secrets"
)

const authRemedy = "Make a new access token in the Bitwarden web app under Secrets Manager › Machine accounts › Access tokens, then run icebath " +
	"secrets bitwarden token. If your account is in another region, run icebath secrets bitwarden setup and pick that region."

// The Bitwarden store, which the source, the service and the commands of the plugin share
type Store struct {
	Bws
	registry *secrets.Registry
}

func Register(registry *secrets.Registry, pk *packages.Registry) *Store {
	store := &Store{Bws: Bws{packages: pk, cache: &secrets.Cache{Name: cacheName}}, registry: registry}
	registry.Register(secrets.Sources{store})
	return store
}

// Host the store's source is registered in
func (s *Store) Host() *secrets.Secrets {
	return s.registry.Host()
}

func (s *Store) Spec() secrets.Spec {
	return secrets.Spec{Name: bitwardenconfig.Name, Label: "Bitwarden Secrets Manager", Short: "Bitwarden", Shape: secrets.Bulk, Scheme: "bws",
		TokenSetting: bitwardenconfig.TokenSetting, TokenEnv: bitwardenconfig.DefaultTokenEnv,
		Hints: map[secrets.Failure]string{secrets.AuthFailed: authRemedy, secrets.AuthExpired: authRemedy}}
}

// The project secrets are pulled from, which a fetch needs, when the profile names none
func (s *Store) Unconfigured(cfg *config.Config) string {
	if strings.TrimSpace(cfg.GetString(bitwardenconfig.ProjectID)) == "" {
		return bitwardenconfig.ProjectID
	}
	return ""
}

// Variable holding the access token for the profile
func (s *Store) TokenEnv(cfg *config.Config) string {
	return s.Host().TokenEnv(cfg, s.Spec())
}

// Seconds a setting holds as a duration
func seconds(cfg *config.Config, key string) time.Duration {
	return time.Duration(max(cfg.GetInt(key), 0)) * time.Second
}

func (s *Store) Fetch(ctx context.Context, f secrets.Fetch) secrets.Result {
	cfg := f.Config
	tokenEnv := s.TokenEnv(cfg)
	token := strings.TrimSpace(f.Env[tokenEnv])
	if token == "" {
		return secrets.Failed(fmt.Sprintf("Bitwarden is on, but %s, the variable for its access token, is not set.", tokenEnv), secrets.NotConfigured)
	}
	if missing := s.Unconfigured(cfg); missing != "" {
		return secrets.Failed("Bitwarden is on, but no project is chosen.", secrets.NotConfigured)
	}
	project := strings.TrimSpace(cfg.GetString(bitwardenconfig.ProjectID))
	binary := s.Find(ctx, cfg, cfg.GetBool(bitwardenconfig.AutoInstall))
	if binary == "" {
		result := secrets.Failed("bws is not installed, and Install bws is off in /settings under Secret stores › Bitwarden.", secrets.BinaryMissing)
		return result
	}
	got, warnings, err := s.Pull(ctx, cfg, Pull{Token: token, Project: project, Server: strings.TrimSpace(cfg.GetString(bitwardenconfig.ServerURL)),
		Binary: binary, Dir: cfg.ProfileDir(cfg.Profile()), TTL: seconds(cfg, bitwardenconfig.CacheTTL), Cache: !f.Fresh,
		Encrypted: cfg.GetBool(bitwardenconfig.EncryptedOn), MaxStale: seconds(cfg, bitwardenconfig.EncryptedStale), Env: f.Env})
	if err != nil {
		result := secrets.Failed(err.Error(), secrets.Classify(err.Error(), rules))
		result.Binary = binary
		if result.Kind == secrets.AuthFailed {
			result.Error = fmt.Sprintf("Bitwarden refused the access token in %s, which may be revoked, expired or for another region. Bitwarden said: %s",
				tokenEnv, secrets.Sentence(err.Error()))
		}
		return result
	}
	return secrets.Result{Secrets: got, Warnings: warnings, Binary: binary}
}
