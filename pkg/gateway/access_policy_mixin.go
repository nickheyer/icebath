package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Values of an allow all users opt in that open DMs
var accessOptinTruthy = map[string]bool{"true": true, "1": true, "yes": true}

// Config driven DM and group intake policy of an adapter that gates access itself
type OwnAccessPolicyMixin struct {
	// AllowAllEnvPrefix is the platform prefix of the allow all users env name
	AllowAllEnvPrefix string
	DMPolicy          string
	GroupPolicy       string
	AllowFrom         map[string]bool
	GroupAllowFrom    map[string]bool
	// EntryMatches is the platform allowlist match, plain membership when nil
	EntryMatches func(ctx context.Context, entries map[string]bool, target string) (bool, error)
	// LiveDMAllowFrom is the allowlist enforced right now, the construction allowlist when nil
	LiveDMAllowFrom func(ctx context.Context) (map[string]bool, error)
}

// Policy of a host, refused like a class with no platform prefix is at creation
func NewOwnAccessPolicyMixin(host, prefix string) (*OwnAccessPolicyMixin, error) {
	if pystr.Strip(prefix) == "" {
		return nil, fmt.Errorf("TypeError: %s mixes in OwnAccessPolicyMixin but sets no ALLOW_ALL_ENV_PREFIX", host)
	}
	return &OwnAccessPolicyMixin{AllowAllEnvPrefix: prefix, AllowFrom: map[string]bool{}, GroupAllowFrom: map[string]bool{}}, nil
}

// The adapter enforces its own access policy
func (p *OwnAccessPolicyMixin) EnforcesOwnAccessPolicy() bool { return true }

// The global and platform allow all users env names
func (p *OwnAccessPolicyMixin) AllowAllEnvNames() []string {
	return []string{"GATEWAY_ALLOW_ALL_USERS", p.AllowAllEnvPrefix + "_ALLOW_ALL_USERS"}
}

// Whether a profile scoped allow all users opt in is set, failing closed
func (p *OwnAccessPolicyMixin) OpenDMOptedIn(ctx context.Context) (bool, error) {
	for _, name := range p.AllowAllEnvNames() {
		value, err := GetScopedSecret(ctx, name, "", false)
		if err != nil {
			return false, err
		}
		if accessOptinTruthy[strings.ToLower(pystr.Strip(value))] {
			return true, nil
		}
	}
	return false, nil
}

// Platform match of the target against the entries
func (p *OwnAccessPolicyMixin) entryMatches(ctx context.Context, entries map[string]bool, target string) (bool, error) {
	if p.EntryMatches != nil {
		return p.EntryMatches(ctx, entries, target)
	}
	return entries[target], nil
}

// Allowlist enforced for DM intake right now
func (p *OwnAccessPolicyMixin) liveDMAllowFrom(ctx context.Context) (map[string]bool, error) {
	if p.LiveDMAllowFrom != nil {
		return p.LiveDMAllowFrom(ctx)
	}
	return p.AllowFrom, nil
}

// Strict DM authorization, pairing never implying access
func (p *OwnAccessPolicyMixin) IsDMAllowed(ctx context.Context, senderID string) (bool, error) {
	if p.DMPolicy == "allowlist" {
		live, err := p.liveDMAllowFrom(ctx)
		if err != nil {
			return false, err
		}
		return p.entryMatches(ctx, live, senderID)
	}
	if p.DMPolicy != "open" {
		return false, nil
	}
	return p.OpenDMOptedIn(ctx)
}

// Whether a DM may reach gateway intake, pairing admitting any principal and a blank one never
func (p *OwnAccessPolicyMixin) IsDMIntakeAllowed(ctx context.Context, senderID string) (bool, error) {
	principal := pystr.Strip(senderID)
	if principal == "" {
		return false, nil
	}
	if p.DMPolicy == "pairing" {
		return true, nil
	}
	return p.IsDMAllowed(ctx, principal)
}

// Whether group traffic reaches intake, disabled, pairing and unknown policies never forwarding it
func (p *OwnAccessPolicyMixin) IsGroupAllowed(ctx context.Context, chatID, senderID string) (bool, error) {
	if p.GroupPolicy == "allowlist" {
		return p.entryMatches(ctx, p.GroupAllowFrom, chatID)
	}
	return p.GroupPolicy == "open", nil
}
