package config

import (
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Prefix of the managed Camofox userId older builds derived
const retiredCamofoxUserPrefix = "hermes_"

// Pins the managed Camofox userId an older build derived so the logins Camofox saved under it stay in use
func migrateTo66(r *migrationRun) error {
	cfg := r.read()
	browser, _ := cfg["browser"].(map[string]any)
	camofox, _ := browser["camofox"].(map[string]any)
	if camofox == nil || !pyrepr.Truthy(camofox["managed_persistence"]) {
		return nil
	}
	if pinned, _ := camofox["user_id"].(string); strings.TrimSpace(pinned) != "" {
		return nil
	}
	root := pypath.PathJoin(r.h, "browser_auth", "camofox")
	userID := retiredCamofoxUserPrefix + strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceURL, []byte("camofox-user:"+root)).String(), "-", "")[:10]
	camofox["user_id"] = userID
	return r.commit(cfg, "browser.camofox.user_id="+userID,
		"  ✓ Set browser.camofox.user_id to "+userID+", the Camofox identity managed persistence used before this release, "+
			"so the browser logins Camofox saved under it stay in use.")
}
