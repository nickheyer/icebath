package credentials

import (
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

// What the accounts of providers do as profiles are made: a copy of a profile drops the sign-ins whose refresh token works once, and a fresh
// profile takes the providers and accounts of the profile it is made from without those
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

func (p Profiles) Made(making profile.Making) ([]string, error) {
	target := p.Config.In(making.Profile)
	var lines []string
	switch {
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
		lines = append(lines, fmt.Sprintf("Copied logins from '%s'.", making.Launch))
	}
	dropped, err := p.dropRotating(target)
	if len(dropped) > 0 {
		lines = append(lines, fmt.Sprintf("⚠ Single-use logins not carried over (%s): each renews once, so sign them in on %s from /providers.",
			strings.Join(dropped, ", "), making.Profile))
	}
	return lines, err
}
