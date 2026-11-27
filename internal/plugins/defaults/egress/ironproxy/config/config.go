package config

const (
	Key = "proxy"
	// Sandbox traffic goes through the proxy
	Enabled = Key + ".enabled"
	// A Docker sandbox refuses to start while the proxy is not ready, and its settings may not undo the proxy
	Enforce = Key + ".enforce_on_docker"
	// Where the proxy gets the real keys it puts in: env or bitwarden
	Source = Key + ".credential_source"
	// With the key source Bitwarden, keys missing there come from this machine's environment instead of the start failing
	Fallback    = Key + ".allow_env_fallback"
	AutoInstall = Key + ".auto_install"
	TunnelPort  = Key + ".tunnel_port"
	ExtraHosts  = Key + ".extra_allowed_hosts"
	// Networks the proxy refuses to connect to, the built-in private ranges while it holds no list
	DenyCIDRs = Key + ".upstream_deny_cidrs"
)

// Key sources of the proxy
const (
	FromEnv       = "env"
	FromBitwarden = "bitwarden"
)

// Port of the tunnel unless the settings name another; the plain http one is after it, the management one after that
const DefaultTunnelPort = 9090
