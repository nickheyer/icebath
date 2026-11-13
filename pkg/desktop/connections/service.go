package connections

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

// ServiceOptions name the platform, directories, key store and log of the connections group
type ServiceOptions struct {
	// Electron platform name, linux, darwin or win32
	Platform string
	// Desktop ICEBATH_HOME
	IcebathHome string
	// Electron userData directory holding connection.json, connections.json and the secret stores
	UserData string
	// OS key store the stored desktop secrets are sealed with
	Safe SafeStorage
	// Linux --password-store backend the app launched with, empty for the default
	PasswordStoreSwitch string
	// Appends one line to desktop.log
	Log func(string)
	// Sends an event to every renderer window
	Broadcast func(event string, payload any)
	// Added by wd_conn_local, writes buffered desktop.log lines out at once
	FlushLog func()
	// Added by wd_conn_local, reloads the primary window
	ReloadMainWindow func()
}

// Service is the connections group the shell binds, one per app
type Service struct {
	platform            string
	sshPathOverride     string
	userData            string
	safe                SafeStorage
	passwordStoreSwitch string
	log                 func(string)

	mu            sync.Mutex
	policy        *SecretStoragePolicy
	config        *connectionConfig
	configStamp   fileStamp
	registry      *ConnectionRegistry
	registryStamp fileStamp

	profiles *desktopProfilePreferences

	// Added by wd_conn_local, ICEBATH_HOME plus the backend, bootstrap surface and profile gates of the routes
	icebathHome string
	local       localSlice

	// Added by wd_conn_cloud, liveness trackers and managed primary restores of the revalidation routes
	cloud *cloudSlice

	// Added by wd_conn_auth, cookie jars, native tokens, socket headers and sign-in flows
	auth authSlice

	// Added by wd_conn_ssh, SSH connections, bootstrap coordinator, keepalives and the managed update gate
	ssh *sshSlice
}

// NewService builds the connections group, nothing on disk or in the key store is touched until a call
func NewService(o ServiceOptions) *Service {
	s := &Service{
		platform:            o.Platform,
		icebathHome:         o.IcebathHome,
		sshPathOverride:     DesktopSSHPathOverride(o.IcebathHome),
		userData:            o.UserData,
		safe:                o.Safe,
		passwordStoreSwitch: o.PasswordStoreSwitch,
		log:                 o.Log,
		cloud:               newCloudSlice(),
	}
	s.ssh = newSSHSlice(s)
	s.local.remoteOwners = NewRemoteOwnerCache()
	s.local.broadcast, s.local.flushLog, s.local.reloadMainWindow = o.Broadcast, o.FlushLog, o.ReloadMainWindow
	s.profiles = &desktopProfilePreferences{
		path:          filepath.Join(o.UserData, "active-profile.json"),
		validateRoute: s.validateDesktopProfileRoute,
		onDefaultChanged: func(route *DesktopProfileRoute) {
			if route == nil {
				o.Broadcast("profile:default:changed", nil)
				return
			}
			o.Broadcast("profile:default:changed", *route)
		},
	}
	return s
}

// DesktopSSHPathOverride is desktop.ssh_path of config.yaml, empty when unset or unreadable
func DesktopSSHPathOverride(icebathHome string) string {
	data, err := config.LoadYAMLFileReadonly(filepath.Join(icebathHome, "config.yaml"))
	if err != nil {
		return ""
	}
	desktop := jsObject(jsObject(toJSONValue(data))["desktop"])
	value, _ := desktop["ssh_path"].(string)
	return strings.TrimSpace(value)
}

// SSHConfigHostsResult is the sshConfigHosts answer
type SSHConfigHostsResult struct {
	Hosts []string `json:"hosts"`
}

// SSHConfigHosts lists the literal Host aliases of the user's ssh config
func (s *Service) SSHConfigHosts() SSHConfigHostsResult {
	return SSHConfigHostsResult{Hosts: CollectSSHConfigHosts("", SSHConfigDeps{})}
}

// SSHResolveHost resolves an alias to its effective hostname, user, port and identity file through ssh -G
func (s *Service) SSHResolveHost(host any) (SSHResolved, error) {
	return ResolveSSHHost(DesktopSSHBinary(s.platform, s.sshPathOverride), host)
}
