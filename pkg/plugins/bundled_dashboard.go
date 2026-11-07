package plugins

import (
	"context"
	"io/fs"
	"net/http"
	"sync"
)

type BundledDashboard struct {
	Name     string
	Manifest []byte
	Assets   fs.FS
	API      func(ctx context.Context) http.Handler
}

var (
	bundledDashboardMu    sync.RWMutex
	bundledDashboardOrder []BundledDashboard
)

// Records a compiled-in dashboard plugin, an empty or repeated name panics at init
func RegisterBundledDashboard(d BundledDashboard) {
	if d.Name == "" {
		panic("plugins: RegisterBundledDashboard with an empty plugin name")
	}
	bundledDashboardMu.Lock()
	defer bundledDashboardMu.Unlock()
	for _, prev := range bundledDashboardOrder {
		if prev.Name == d.Name {
			panic("plugins: bundled dashboard " + d.Name + " registered twice")
		}
	}
	bundledDashboardOrder = append(bundledDashboardOrder, d)
}

// Every compiled-in dashboard plugin in registration order
func BundledDashboards() []BundledDashboard {
	bundledDashboardMu.RLock()
	defer bundledDashboardMu.RUnlock()
	return append([]BundledDashboard(nil), bundledDashboardOrder...)
}
