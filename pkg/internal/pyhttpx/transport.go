package pyhttpx

import (
	"context"
	"net/http"
	"time"
)

type timeoutTransport struct {
	base  http.RoundTripper
	watch func(cancel func()) *watchdog
}

// TimeoutTransport gives each request the connect, write, read and pool limits of an httpx client built with the timeout
func TimeoutTransport(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return timeoutTransport{base: base, watch: func(cancel func()) *watchdog { return newWatchdog(timeout, cancel) }}
}

// PhaseTimeoutTransport gives each request the distinct connect, write, read and pool limits of one httpx.Timeout
func PhaseTimeoutTransport(base http.RoundTripper, timeout Timeout) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return timeoutTransport{base: base, watch: func(cancel func()) *watchdog { return newPhaseWatchdog(timeout, cancel) }}
}

func (t timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	w := t.watch(cancel)
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
