package browser

import "github.com/nickheyer/icebath/pkg/provider"

// A plugin browser owning its pages outside the CDP session whose live page browser_vault evaluates scripts on
type BrowserPageBackend interface {
	provider.ProviderBase
	HasPage(taskID string) bool
	Evaluate(taskID, expression string, secret bool) (any, error)
}
