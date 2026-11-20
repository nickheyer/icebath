//go:build !icebath_core && !icebath_noextras

package gatewayrun

import "github.com/nickheyer/icebath/pkg/browser"

// Binds the browser session sweep of gateway shutdown
func init() { cleanupAllBrowsers = browser.CleanupAllBrowsers }
