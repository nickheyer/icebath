package terminal

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/secrets"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools/redact"
)

// Secret store the proxy fetches its keys from when the settings say bitwarden, by the name it registers with the secrets host
const Bitwarden = ironconfig.FromBitwarden

// The proxy as the terminal host reads it: no backends of its own, the egress every sandbox of the profile goes through; and as the
// profiles read it, a daemon that stops with its profile
type registrant struct {
	egress *Egress
	config *config.Config
}

func Register(registry *terminal.Registry, profiles *profile.Registry, cfg *config.Config, pk *packages.Registry, sources *secrets.Registry) *Egress {
	e := newEgress(pk, sources)
	r := registrant{egress: e, config: cfg}
	registry.Register(r)
	profiles.Register(r)
	return e
}

func (registrant) Backends() []terminal.Backend { return nil }

// Stops the daemon of the deleted profile, whose folder goes with it
func (r registrant) Delete(name string) error {
	_, err := r.egress.Stop(r.config.In(name))
	return err
}

// Stops the daemon of a profile before its folder moves to a new name, where it starts again as a sandbox next needs it
func (r registrant) Stop(name string) error {
	return r.Delete(name)
}

// The profile's proxy: whether it is on and enforced, set up, running and where, its authority and the keys it swaps
func (r registrant) Proxy(cfg *config.Config) terminal.Proxy {
	proxy := terminal.Proxy{Enabled: cfg.GetBool(ironconfig.Enabled), Enforce: cfg.GetBool(ironconfig.Enforce)}
	if !proxy.Enabled {
		return proxy
	}
	status := r.egress.Status(context.Background(), cfg)
	proxy.Configured, proxy.Running, proxy.Port, proxy.CA = status.Configured(), status.PID != 0 && status.Listening, status.Port, status.Authority
	for _, m := range r.egress.Mappings(cfg) {
		proxy.Tokens = append(proxy.Tokens, terminal.ProxyToken{Name: m.Env, Aliases: m.Aliases, Token: m.Token})
	}
	return proxy
}

// Status of the profile's proxy as plain lines, the tokens masked unless asked to show them: what /egress shows
func (e *Egress) Text(ctx context.Context, cfg *config.Config, showTokens bool) string {
	status := e.Status(ctx, cfg)
	yes := func(on bool) string {
		if on {
			return "yes"
		}
		return "no"
	}
	or := func(value, missing string) string {
		if value == "" {
			return missing
		}
		return value
	}
	process := "(stopped)"
	if status.PID != 0 {
		process = fmt.Sprintf("pid %d", status.PID)
	}
	enabled := cfg.GetBool(ironconfig.Enabled)
	lines := []string{"Egress proxy status", "",
		"Enabled: " + yes(enabled),
		"Binary: " + or(status.Binary, "(missing)"),
		"Binary version: " + or(status.Version, "(unknown)"),
		"Config: " + or(status.Config, "(not generated)"),
		"CA cert: " + or(status.Authority, "(not generated)"),
		fmt.Sprintf("Tunnel port: %d", status.Port),
		"Process: " + process,
		"Listening: " + yes(status.Listening),
		"Credential src: " + cfg.GetString(ironconfig.Source),
		"Docker enforce: " + yes(cfg.GetBool(ironconfig.Enforce)),
		"Scope: Docker backend only in this release",
	}
	if mappings := e.Mappings(cfg); len(mappings) > 0 {
		lines = append(lines, "", "Token mappings:")
		for _, m := range mappings {
			token := m.Token
			if !showTokens {
				token = redact.Ends(m.Token)
			}
			lines = append(lines, fmt.Sprintf("  - %s: %s (%s)", m.Env, token, strings.Join(m.Hosts, ", ")))
		}
	}
	if uncovered := Uncovered(cfg, nil); len(uncovered) > 0 {
		lines = append(lines, "", "Uncovered providers (real credentials still visible inside the sandbox):")
		for _, name := range uncovered {
			lines = append(lines, "  - "+name)
		}
	}
	switch {
	case enabled && !status.Configured():
		lines = append(lines, "", "Next: run `icebath egress setup` to mint tokens and write proxy.yaml.")
	case enabled && !(status.PID != 0 && status.Listening):
		lines = append(lines, "", "Next: run `icebath egress start` before launching Docker sandboxes.")
	}
	return strings.Join(lines, "\n")
}
