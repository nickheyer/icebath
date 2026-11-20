package gateway

import (
	"net/http"

	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
)

// Unguarded client of an adapter talking to its own configured server, env proxies honoured and the pool bounded by limits
func LimitedHTTPClient(limits HTTPXLimits, followRedirects bool) *http.Client {
	tr := netpref.Follow(http.DefaultTransport.(*http.Transport).Clone())
	applyHTTPXLimits(tr, limits)
	return &http.Client{Transport: tr, CheckRedirect: pyhttpx.RedirectPolicy(followRedirects)}
}
