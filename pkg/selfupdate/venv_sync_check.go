package selfupdate

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// One passive PM verdict on startup, the problem text or empty when the install is current or a developer checkout
func CheckRuntime(ctx context.Context, projectRoot string) string {
	if pathPresent(filepath.Join(projectRoot, ".git")) {
		mechanism := ""
		if stamp := pm.ReadInstallStamp(projectRoot); stamp != nil {
			mechanism, _ = stamp.Vals["updateMechanism"].(string)
		}
		if mechanism != "self" {
			return ""
		}
	}
	problems, err := pm.Activate(ctx, false)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		return ""
	}
	remedy := "run `icebath pm install`"
	if steward := SealedSteward(projectRoot); steward != nil {
		remedy = "this " + *steward + "-managed install must rebuild the artifact to fix"
	}
	return "install out of sync (" + strings.Join(problems, "; ") + ") — " + remedy
}
