package main

import "github.com/nickheyer/icebath/pkg/desktop/connections"

// GetConnectionConfig handles getConnectionConfig
func (b *ConnectionsService) GetConnectionConfig(profile any) connections.ConnectionConfigView {
	return b.svc.GetConnectionConfig(profile)
}

// ProbeConnectionConfig handles probeConnectionConfig
func (b *ConnectionsService) ProbeConnectionConfig(remoteURL any) (connections.AuthModeProbe, error) {
	return b.svc.ProbeConnectionConfig(remoteURL)
}

// CloudStatus handles cloud.status
func (b *ConnectionsService) CloudStatus() connections.CloudStatusResult {
	return b.svc.CloudStatus()
}

// CloudLogin handles cloud.login
func (b *ConnectionsService) CloudLogin() (connections.CloudSessionResult, error) {
	return b.svc.CloudLogin()
}

// CloudLogout handles cloud.logout
func (b *ConnectionsService) CloudLogout() connections.CloudSessionResult {
	return b.svc.CloudLogout()
}

// CloudDiscover handles cloud.discover
func (b *ConnectionsService) CloudDiscover(org any) (any, error) {
	return b.svc.CloudDiscover(org)
}

// CloudAgentSignIn handles cloud.agentSignIn
func (b *ConnectionsService) CloudAgentSignIn(dashboardURL any) (connections.CloudAgentSignIn, error) {
	return b.svc.CloudAgentSignIn(dashboardURL)
}
