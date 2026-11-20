//go:build !icebath_core && !icebath_noextras

package app

import (
	"github.com/nickheyer/icebath/pkg/browser"
)

// Adds the browser session reaper to shutdown
func init() {
	emergencyCleanupBrowsers = browser.EmergencyCleanupAllSessions
}
