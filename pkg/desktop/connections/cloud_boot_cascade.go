package connections

import "github.com/nickheyer/icebath/pkg/desktop/backend"

// Whether a boot failure of a remote is the expired cookie of a Cloud OAuth agent a silent sign in can renew
func ShouldAttemptCloudBootCascade(remote backend.Connection, err error) bool {
	if remote == nil {
		return false
	}
	if kind, _ := remote["remoteKind"].(string); kind != "cloud" {
		return false
	}
	if mode, _ := remote["authMode"].(string); mode != "oauth" {
		return false
	}
	return backend.IsReauthRequiredError(err)
}
