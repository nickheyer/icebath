package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
	"github.com/nickheyer/icebath/pkg/desktop/safestorage"
)

// ConnectionsService binds the connections preload functions
type ConnectionsService struct {
	shell    *Shell
	svc      *connections.Service
	accounts *connections.Accounts
	auth     *authHostShell
}

func newConnectionsService(s *Shell) *ConnectionsService {
	store := connections.ResolveLinuxPasswordStore(os.Getenv, electronPlatform())
	if store.Warning != "" {
		fmt.Fprintln(os.Stderr, "[icebath] "+store.Warning)
	}
	if store.Store != "" {
		fmt.Println("[icebath] using password-store backend: " + store.Store)
	}
	safe := safestorage.New(safestorage.Options{AppName: appName, UserData: s.userData, PasswordStore: store.Store})
	b := &ConnectionsService{shell: s, accounts: connections.NewAccounts(), auth: &authHostShell{shell: s}, svc: connections.NewService(connections.ServiceOptions{
		Platform: electronPlatform(), IcebathHome: s.HermesHome, UserData: s.userData,
		Safe: safe, PasswordStoreSwitch: store.Store, Log: s.DesktopLog.Remember,
		Broadcast: func(event string, payload any) {
			for _, w := range s.windowsList() {
				w.Send(event, payload)
			}
		},
		FlushLog: s.DesktopLog.FlushSync,
		ReloadMainWindow: func() {
			if w := s.mainWindow(); w != nil && !w.IsDestroyed() {
				w.w.Reload()
			}
		},
	})}
	b.svc.WireAuth(b.auth)
	return b
}

// ServiceStartup runs the basic store switch and the one shot legacy secret migration before any window loads
func (b *ConnectionsService) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	b.auth.start(application.Get())
	return b.svc.Startup()
}

// SshConfigHosts handles sshConfigHosts
func (b *ConnectionsService) SshConfigHosts() connections.SSHConfigHostsResult {
	return b.svc.SSHConfigHosts()
}

// SshResolveHost handles sshResolveHost
func (b *ConnectionsService) SshResolveHost(host any) (connections.SSHResolved, error) {
	return b.svc.SSHResolveHost(host)
}

// GetSecretStorageEncryption handles getSecretStorageEncryption
func (b *ConnectionsService) GetSecretStorageEncryption() connections.SecretStorageState {
	return b.svc.GetSecretStorageEncryption()
}

// SetSecretStorageEncryption handles setSecretStorageEncryption
func (b *ConnectionsService) SetSecretStorageEncryption(on any) (connections.SecretStorageState, error) {
	return b.svc.SetSecretStorageEncryption(on)
}

// ProfileGetDefault handles profile.getDefault
func (b *ConnectionsService) ProfileGetDefault() *connections.DesktopProfileRoute {
	return b.svc.ProfileGetDefault()
}

// ProfileSetDefault handles profile.setDefault
func (b *ConnectionsService) ProfileSetDefault(route any) (connections.DesktopProfileRoute, error) {
	return b.svc.ProfileSetDefault(route)
}

// ProfileGet handles profile.get
func (b *ConnectionsService) ProfileGet() connections.ProfileResult {
	return b.svc.ProfileGet()
}

// ProfileRemember handles profile.remember
func (b *ConnectionsService) ProfileRemember(name any) (connections.ProfileResult, error) {
	return b.svc.ProfileRemember(name)
}

// ProfileImportConnections handles profile.importConnections
func (b *ConnectionsService) ProfileImportConnections(record any) error {
	return b.svc.ProfileImportConnections(record)
}

// ProfileRememberConnection handles profile.rememberConnection
func (b *ConnectionsService) ProfileRememberConnection(connectionID, profile any) error {
	return b.svc.ProfileRememberConnection(connectionID, profile)
}

// GetMachineProfile handles getMachineProfile
func (b *ConnectionsService) GetMachineProfile() connections.MachineProfile {
	return b.accounts.GetMachineProfile()
}

// GetRemoteDisplayReason handles getRemoteDisplayReason
func (b *ConnectionsService) GetRemoteDisplayReason() *string {
	return b.accounts.GetRemoteDisplayReason()
}

// McpOauthListen handles mcpOauth.listen
func (b *ConnectionsService) McpOauthListen() (connections.McpOAuthListener, error) {
	return b.svc.McpOauthListen()
}

// McpOauthWait handles mcpOauth.wait
func (b *ConnectionsService) McpOauthWait(id any, timeoutMs any) connections.McpOAuthCallbackResult {
	return b.svc.McpOauthWait(id, timeoutMs)
}

// McpOauthCancel handles mcpOauth.cancel
func (b *ConnectionsService) McpOauthCancel(id any) bool {
	return b.svc.McpOauthCancel(id)
}

// ConnectionsList handles connections.list
func (b *ConnectionsService) ConnectionsList() (connections.SanitizedRegistry, error) {
	return b.svc.ConnectionsList()
}

// ConnectionsSetLastUsed handles connections.setLastUsed
func (b *ConnectionsService) ConnectionsSetLastUsed(id any) (connections.RegistryMutationResult, error) {
	return b.svc.ConnectionsSetLastUsed(id)
}

// ServiceShutdown closes the pooled REST sockets so none outlives quit
func (b *ConnectionsService) ServiceShutdown() error {
	connections.DestroyKeepaliveAgents()
	return nil
}

// OauthLoginConnectionConfig handles oauthLoginConnectionConfig
func (b *ConnectionsService) OauthLoginConnectionConfig(rawURL any, opts any) (connections.OauthLoginResult, error) {
	return b.svc.OauthLoginConnectionConfig(rawURL, opts)
}

// OauthLogoutConnectionConfig handles oauthLogoutConnectionConfig
func (b *ConnectionsService) OauthLogoutConnectionConfig(rawURL any) (connections.OauthLogoutResult, error) {
	return b.svc.OauthLogoutConnectionConfig(rawURL)
}

// ConnectionsSetPrimary handles connections.setPrimary
func (b *ConnectionsService) ConnectionsSetPrimary(id any) (connections.RegistryMutationResult, error) {
	return b.svc.ConnectionsSetPrimary(id)
}

// ConnectionsSetLaunchMode handles connections.setLaunchMode
func (b *ConnectionsService) ConnectionsSetLaunchMode(mode any) (connections.RegistryMutationResult, error) {
	return b.svc.ConnectionsSetLaunchMode(mode)
}
