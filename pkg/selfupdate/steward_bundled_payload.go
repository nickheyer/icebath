package selfupdate

import "github.com/nickheyer/icebath/pkg/pm"

// Whether the tree is a bundled desktop payload repo by its build stamp, an unreadable stamp meaning no
func IsBundledPayload(projectRoot string) bool {
	payload, _ := pm.ReadInstallStamp(projectRoot).Get("payload")
	return payload == "bundled"
}
