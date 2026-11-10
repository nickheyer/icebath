package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// One passive PM verdict on startup, the problem text or empty when the install is current or a developer checkout
func CheckRuntime(ctx context.Context, projectRoot string) (string, error) {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err == nil {
		if mechanism, _ := pm.ReadInstallStamp(projectRoot).Get("updateMechanism"); mechanism != "self" {
			return "", nil
		}
	}
	problems, err := checkRuntimeActivate(ctx, false)
	if err != nil {
		return "", err
	}
	if len(problems) == 0 {
		return "", nil
	}
	remedy := "run `icebath pm install`"
	if steward := SealedSteward(projectRoot); steward != nil {
		remedy = "this " + *steward + "-managed install must rebuild the artifact to fix"
	}
	return "install out of sync (" + strings.Join(problems, "; ") + ") — " + remedy, nil
}

// Store activation the startup verdict runs, the pm.activate attribute hermes tests replace
var checkRuntimeActivate = pm.Activate
