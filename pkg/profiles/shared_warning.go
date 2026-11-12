package profiles

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Warning when another live install verifiably uses the same home, empty otherwise
func SharedProfileWarning(ctx context.Context, homeDir, projectRoot string) string {
	ownInstall := tools.InstallID(projectRoot)
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	homeKey := home.Key(homeDir)
	for _, entry := range tools.LedgerEntries(tools.LedgerOptions{AllInstalls: true, VerifiedOnly: true}) {
		install := entry["install"]
		if pyTruthy(install) && install != any(ownInstall) && entry["hermes_home"] == any(homeKey) {
			return "Another icebath installation is using this profile. Both installations share " +
				"its settings and data, so changes can conflict. You can continue, or close " +
				"the other installation before making changes."
		}
	}
	return ""
}
