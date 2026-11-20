package matrix

import "github.com/nickheyer/icebath/pkg/hostinfo"

// Encryption line of the version output, the linked mautrix version or the build command that adds it
func E2EEVersionLine() string {
	if !E2EEBuiltIn {
		return "Matrix E2EE: Not built in (" + E2EEBuildCommand + ")"
	}
	if v := hostinfo.ModuleVersion("maunium.net/go/mautrix"); v != "" {
		return "Matrix E2EE: goolm (mautrix " + v + ")"
	}
	return "Matrix E2EE: goolm"
}
