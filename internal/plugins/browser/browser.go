package browser

import "github.com/nickheyer/icebath/internal/browser"

func Register(host *browser.Browser) *browser.Registry {
	registry := new(browser.Registry)
	host.Register(registry)
	return registry
}
