package agent

import (
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Returns the session credits capture kept on the live route with notices routed to the agent callbacks
func (a *AIAgent) creditsCapture() *provider.CreditsCapture {
	a.creditsMu.Lock()
	defer a.creditsMu.Unlock()
	if a.credits == nil {
		ctx := a.homeContext()
		tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
		if err != nil {
			tree = nil
		}
		c := provider.NewCreditsCapture(tree, a.provider, a.model, a.baseURL, a.sessionID)
		c.Notice = a.emitNotice
		c.NoticeClear = a.emitNoticeClear
		a.credits = c
		return c
	}
	a.credits.SetRoute(a.provider, a.model, a.baseURL)
	if a.credits.SessionID != a.sessionID {
		a.credits.SessionID = a.sessionID
	}
	return a.credits
}
