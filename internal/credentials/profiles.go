package credentials

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Registrant whose sign-ins hold a refresh token that works once, so a copy of one in another profile breaks the sign-in it was copied from
// the first time either renews
type Rotating interface {
	Rotates(account Account) bool
}

// Whether the account holds a sign-in its refresh token can renew only once
func (r *Registry) Rotates(account Account) bool {
	for _, registrant := range r.registrants {
		if rotating, ok := registrant.(Rotating); ok && rotating.Rotates(account) {
			return true
		}
	}
	return false
}

// What the accounts of providers do as profiles are made: a copy of a profile drops the sign-ins whose refresh token works once, a copy made
// without credentials drops every account that kept a key or a sign-in of its own, and a fresh profile takes the providers and accounts of
// the profile it is made from without those
type Profiles struct {
	Credentials *Credentials
	Providers   *provider.Registry
	Config      *config.Config
}

// Accounts live in the profile's config.yaml, which goes with it
func (Profiles) Delete(string) error {
	return nil
}

// Removes the sign-ins of the profile that can't be shared with the one they were copied from: the providers whose sign-ins were dropped
func (p Profiles) dropRotating(cfg *config.Config) ([]string, error) {
	var dropped []string
	for _, instance := range p.Providers.Instances(cfg) {
		for _, name := range instance.Accounts {
			if !p.Credentials.Rotates(Account{Name: name, Provider: instance, Fields: instance.AccountFields(name)}) {
				continue
			}
			if err := instance.AccountFields(name).Remove(); err != nil {
				return dropped, err
			}
			if instance.Settings.GetString(providerconfig.Account) == name {
				if err := instance.Settings.Set(providerconfig.Account, nil); err != nil {
					return dropped, err
				}
			}
			if !slices.Contains(dropped, instance.Label()) {
				dropped = append(dropped, instance.Label())
			}
		}
	}
	return dropped, nil
}

// Removes the accounts of a copy made without credentials that kept a key or a sign-in of their own, which the copy left out: every account
// but those that read their key from a variable or use the sign-in of another program on this computer. So no account stands without the
// credential that makes it one
func (p Profiles) dropKept(cfg *config.Config) error {
	for _, instance := range p.Providers.Instances(cfg) {
		for _, name := range instance.Accounts {
			account := Of(instance, name)
			if strings.TrimSpace(account.Fields.GetString(providerconfig.KeyEnv)) != "" || account.Program() != "" {
				continue
			}
			if _, err := p.Providers.RemoveAccount(context.Background(), cfg, instance.ID, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p Profiles) Made(making profile.Making) ([]string, error) {
	target := p.Config.In(making.Profile)
	var lines []string
	switch {
	case making.Source != "" && making.Bare:
		return nil, p.dropKept(target)
	case making.Source != "":
	case making.Bare || making.Launch == "":
		return nil, nil
	default:
		held, _ := p.Config.In(making.Launch).Stored(providerconfig.Key).(map[string]any)
		if len(held) == 0 || target.View(providerconfig.Key).Exists() {
			return nil, nil
		}
		if err := target.Set(providerconfig.Key, maps.Clone(held)); err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("Copied the accounts of the profile %s.", making.Launch))
	}
	dropped, err := p.dropRotating(target)
	if len(dropped) > 0 {
		lines = append(lines, fmt.Sprintf("⚠ These accounts were not copied, because each can renew only once: %s. Sign them in again on the "+
			"profile %s.", strings.Join(dropped, ", "), making.Profile))
	}
	return lines, err
}
