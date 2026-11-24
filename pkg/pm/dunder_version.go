package pm

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Install stamp base version of the repo checkout, 0.0.0 when the stamp has none
func DunderVersion() string {
	v, _ := ReadInstallStamp(RepoRoot()).Get("baseVersion")
	if !anyutil.IsTrue(v) {
		return "0.0.0"
	}
	return anyutil.String(v)
}
