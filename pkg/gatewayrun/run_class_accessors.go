package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

var _ gateway.SlashAccessRunner = (*GatewayRunner)(nil)

// Launch profile's gateway config, the runner config attribute
func (r *GatewayRunner) RunnerConfig() *gateway.GatewayConfig { return r.config }

// Launch profile's gateway config the session key derivation reads
func (r *GatewayRunner) GatewayConfig() *gateway.GatewayConfig { return r.config }

// Profile that owns the primary adapter map, default under multiplexing
func (r *GatewayRunner) PrimaryProfileName() string { return r.primaryProfileName }

// Gateway config of every served profile as loaded once at startup
func (r *GatewayRunner) ProfileConfigs() map[string]*gateway.GatewayConfig { return r.profileConfigs }
