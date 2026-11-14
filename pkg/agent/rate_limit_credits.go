package agent

import (
	"net/http"
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
		a.credits = provider.NewCreditsCapture(tree, a.provider, a.model, a.baseURL, a.sessionID)
	} else {
		a.credits.SetRoute(a.provider, a.model, a.baseURL)
		if a.credits.SessionID != a.sessionID {
			a.credits.SessionID = a.sessionID
		}
	}
	a.credits.Notice, a.credits.NoticeClear = nil, nil
	if a.noticeCallback != nil || a.noticeClearCallback != nil {
		a.credits.Notice, a.credits.NoticeClear = a.emitNotice, a.emitNoticeClear
	}
	return a.credits
}

// Headers of a response value, nil when there is nothing to parse
func responseHeaders(response any) http.Header {
	switch r := response.(type) {
	case *http.Response:
		if r != nil {
			return r.Header
		}
	case http.Header:
		return r
	}
	return nil
}

// Captures rate limit and credits state from Anthropic Messages response headers
func (a *AIAgent) captureAnthropicResponseHeaders(response any) {
	a.creditsCapture().CaptureAnthropicResponseHeaders(responseHeaders(response))
}
