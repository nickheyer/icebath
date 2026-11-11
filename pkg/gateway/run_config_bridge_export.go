package gateway

// Session config keys with the env variables the import bridge of the runner module exports them to
func SessionsEnvBridge() [][2]string { return sessionsEnvBridge }

// Exports every present key of a config section to its env variable as the Python str of the value
func BridgeSectionToEnv(section any, mapping [][2]string) { bridgeSectionToEnv(section, mapping) }

// Effective user config the presence sensitive env bridge reads, a bypass worker's frozen snapshot when set
func LoadBridgeConfig(configPath string) (map[string]any, error) { return loadBridgeConfig(configPath) }
