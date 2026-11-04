package mcp

// Fails the build until every listed module exists, each name is the Go file still to write
func incompletePort() {
	sessionPort()
	configPort()
	securityPort()
	costsPort()
	schemaPort()
	contentPort()
	schemaCachePort()
	oauthPort()
	oauthDevicePort()
	oauthManagerPort()
	dashboardOAuthPort()
	serverLifecyclePort()
	registrationPort()
	handlersPort()
	samplingPort()
	managerPort()
	catalogPort()
	startupPort()
	eventsPort()
	livenessPort()
	nodeABIPort()
	pickerPort()
	cliConfigPort()
	agentSnapshotPort()
}
