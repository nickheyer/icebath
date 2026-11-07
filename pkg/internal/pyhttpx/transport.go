package pyhttpx

import (
	"context"
	"net/http"
	"time"
)

type timeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

// TimeoutTransport gives each request the connect, write, read and pool limits of an httpx client built with the timeout
func TimeoutTransport(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return timeoutTransport{base: base, timeout: timeout}
}

func (t timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	w := newWatchdog(t.timeout, cancel)
	resp, err := t.base.RoundTrip(req.WithContext(w.trace(ctx)))
	if err != nil {
		w.stop()
		cancel()
		return nil, w.wrap(err)
	}
	w.reset("read")
	resp.Body = &watchedBody{rc: resp.Body, w: w, cancel: cancel}
	return resp, nil
}
