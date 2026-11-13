//go:build darwin

package main

import (
	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// Sign-in window over a per partition WKWebsiteDataStore, its cookie store and request headers
func (h *authHostShell) openPlatformLoginWindow(spec connections.LoginWindowSpec) (connections.LoginWindow, error) {
	return openWKWebViewLoginWindow(h, spec)
}
