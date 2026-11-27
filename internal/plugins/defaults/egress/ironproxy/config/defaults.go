package config

import "github.com/nickheyer/icebath/internal/config"

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Egress proxy",
		About: "iron-proxy: sandbox traffic goes through a local TLS intercepting proxy that swaps its own tokens for the real provider keys, so an agent " +
			"in a sandbox never holds them. Applies to Docker sandboxes; set it up with icebath egress setup",
		Settings: []config.Setting{
			{Key: Enabled, Title: "Egress proxy", About: "Route sandbox network traffic through the key-hiding proxy. Run the egress setup command first; new sessions follow it",
				Default: false},
			{Key: Enforce, Title: "Require proxy", About: "Refuse to start a Docker sandbox if the proxy is not ready; when off, the sandbox starts without the proxy and warns",
				Default: true},
			{Key: Source, Title: "Proxy key source", About: "Where the egress proxy gets the real API keys it injects", Default: FromEnv,
				Options: config.Options(
					config.Option{Value: FromEnv, Title: "Environment", About: "Read keys from this machine's environment"},
					config.Option{Value: FromBitwarden, Title: "Bitwarden", About: "Fetch keys from Bitwarden Secrets Manager"},
				)},
			{Key: Fallback, Title: "Allow key fallback", About: "Use keys from the environment if the secret store is off or misses some; only matters when the key source is Bitwarden",
				Default: false},
			{Key: AutoInstall, Title: "Auto-install proxy", About: "Download the egress proxy program when it is missing", Default: true},
			{Key: TunnelPort, Title: "Proxy port", About: "Local port the egress proxy listens on, the plain http listener on the next one; written into the proxy when egress setup runs",
				Default: DefaultTunnelPort},
			{Key: ExtraHosts, Title: "Extra allowed hosts", About: "Extra sites the egress proxy lets sandboxes reach; written into the proxy when egress setup runs",
				Default: []string{}},
			{Key: DenyCIDRs, Title: "Blocked networks", About: "Network ranges the egress proxy refuses to connect to. Leave unset for the built-in private network list; " +
				"an empty list blocks nothing. Written into the proxy when egress setup runs", Default: []string{}},
		},
	}})
}
