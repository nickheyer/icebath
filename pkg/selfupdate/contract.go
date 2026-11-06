package selfupdate

import "github.com/nickheyer/icebath/pkg/pm"

const CommitBuildUpdateMessage = "This build doesn't get updates. Ask the developer who gave it to you for a new build."

// Reports whether the install stamp at projectRoot names a commit build that never updates
func IsCommitBuild(projectRoot string) bool {
	value, _ := pm.ReadInstallStamp(projectRoot).Get("source")
	return value == "commit-build"
}
