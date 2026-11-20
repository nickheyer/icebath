package manage

// Stops the gateway recorded in the profile's pid file, refusing a record stamped with another home
func StopGatewayProcess(profileDir string) { stopGatewayProcess(profileDir) }
