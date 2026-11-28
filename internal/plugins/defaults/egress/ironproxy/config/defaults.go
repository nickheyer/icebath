package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Key proxy",
		About: "Keeps your API keys out of Docker sandboxes",
		Into:  terminalconfig.Key,
		Settings: []config.Setting{
			{Key: Enabled, Title: "Key proxy", About: "Send sandbox traffic through a proxy that puts in your real API keys on the way out.",
				Help: "A Docker sandbox then never holds your keys. Turning it on sets the proxy up first.", Default: false, Picks: Proxy},
			{Key: Enforce, Title: "Required for Docker", About: "Refuse to start a Docker sandbox while the proxy is not ready.",
				Help: "When this is off, the sandbox starts without the proxy and warns you.", Default: true},
			{Key: Source, Title: "Key source", About: "Where the proxy gets the real API keys it puts in.", Default: FromEnv,
				Options: config.Options(
					config.Option{Value: FromEnv, Title: "Environment", About: "The environment variables of this computer"},
					config.Option{Value: FromBitwarden, Title: "Bitwarden", About: "Bitwarden Secrets Manager"},
				)},
			{Key: Fallback, Title: "Fall back to environment", About: "Use keys from the environment when Bitwarden is off or lacks some.",
				Help: "This matters only when the key source is Bitwarden.", Default: false},
			{Key: AutoInstall, Title: "Install when missing", About: "Download the proxy program when it is missing.", Default: true},
			{Key: TunnelPort, Title: "Port", About: "The port on this computer that the proxy listens on.",
				Help: "Plain HTTP uses the next port. A change takes effect when the proxy is set up again.", Default: DefaultTunnelPort, Counts: config.Plain,
				Least: config.Num(1), Most: config.Num(65534), Advanced: true},
			{Key: ExtraHosts, Title: "Extra allowed sites", About: "Sites beyond the built-in ones that sandboxes may reach through the proxy.",
				Help: "A change takes effect when the proxy is set up again.", Default: []string{}, Entry: &config.Entry{Noun: "site"}, Advanced: true},
			{Key: DenyCIDRs, Title: "Blocked networks", About: "Network ranges that the proxy never connects to.",
				Default: []string{}, Help: "A list you save, even an empty one, replaces the built-in private ranges. A change takes effect when the proxy is set up again.",
				Zero: "Private ranges", Entry: &config.Entry{Noun: "network range"}, Advanced: true},
		},
	}})
}
