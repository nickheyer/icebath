package pm

import "github.com/nickheyer/icebath/pkg/internal/pyrepr"

// Install stamp base version of the repo checkout, 0.0.0 when the stamp has none
func DunderVersion() string {
	v, _ := ReadInstallStamp(RepoRoot()).Get("baseVersion")
	if !pyrepr.Truthy(v) {
		return "0.0.0"
	}
	return pyrepr.Str(v)
}
