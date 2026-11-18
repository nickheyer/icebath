package agent

import (
	"net/http"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Session header capture kept on the live provider
func (a *AIAgent) liveHeaderCapture() *provider.HeaderCapture {
	a.headerCaptureMu.Lock()
	defer a.headerCaptureMu.Unlock()
	if a.headerCapture == nil {
		a.headerCapture = provider.NewHeaderCapture(a.provider)
	} else {
		a.headerCapture.SetProvider(a.provider)
	}
	return a.headerCapture
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

// Captures rate limit state from Anthropic Messages response headers
func (a *AIAgent) captureAnthropicResponseHeaders(response any) {
	a.liveHeaderCapture().CaptureRateLimits(responseHeaders(response))
}
