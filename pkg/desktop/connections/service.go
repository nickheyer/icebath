package connections

import (
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Service is the connections group the shell binds, one per app
type Service struct {
	platform        string
	sshPathOverride string
}

// NewService builds the connections group for an Electron platform name and the desktop ICEBATH_HOME
func NewService(platform, icebathHome string) *Service {
	return &Service{platform: platform, sshPathOverride: DesktopSSHPathOverride(icebathHome)}
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
