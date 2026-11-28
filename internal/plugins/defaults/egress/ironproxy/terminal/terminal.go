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
	on := func(on bool) string {
		if on {
			return "On"
		}
		return "Off"
	}
	yes := func(yes bool) string {
		if yes {
			return "Yes"
		}
		return "No"
	}
	or := func(value, missing string) string {
		if value == "" {
			return missing
		}
		return value
	}
	process := "Not running"
	if status.PID != 0 {
		process = fmt.Sprintf("Running as process %d", status.PID)
	}
	source := "Environment"
	if cfg.GetString(ironconfig.Source) == ironconfig.FromBitwarden {
		source = "Bitwarden"
	}
	enabled := cfg.GetBool(ironconfig.Enabled)
	lines := []string{"Key proxy", "",
		"Hide your keys: " + on(enabled),
		"Program: " + or(status.Binary, "Not installed"),
		"Version: " + or(strings.TrimPrefix(status.Version, "v"), "Not known"),
		"Settings file: " + or(status.Config, "Not written yet"),
		"CA certificate: " + or(status.Authority, "Not made yet"),
		fmt.Sprintf("Port: %d", status.Port),
		"Process: " + process,
		"Listening: " + yes(status.Listening),
		"Key source: " + source,
		"Required for Docker: " + on(cfg.GetBool(ironconfig.Enforce)),
		"Covers: Docker sandboxes only, in this release",
	}
	if mappings := e.Mappings(cfg); len(mappings) > 0 {
		lines = append(lines, "", "Stand-in tokens:")
		for _, m := range mappings {
			token := m.Token
			if !showTokens {
				token = redact.Ends(m.Token)
			}
			lines = append(lines, fmt.Sprintf("  - %s: %s, for %s", m.Env, token, strings.Join(m.Hosts, ", ")))
		}
	}
	if uncovered := Uncovered(cfg, nil); len(uncovered) > 0 {
		lines = append(lines, "", "Providers the proxy does not cover. Their real keys are still visible inside the sandbox:")
		for _, name := range uncovered {
			lines = append(lines, "  - "+name)
		}
	}
	switch {
	case enabled && !status.Configured():
		lines = append(lines, "", "It is not set up yet. Turn Hide your keys off and on again in Key proxy, in /sandbox, or run `icebath egress setup`.")
	case enabled && !(status.PID != 0 && status.Listening):
		lines = append(lines, "", "It is not running. Start it in Key proxy, in /sandbox, or with `icebath egress start`, before you start Docker sandboxes.")
	}
	return strings.Join(lines, "\n")
}
