package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	httputil "github.com/nickheyer/icebath/pkg/utils/http"
)

// Transport of the vendor calls that bound each phase themselves: the shared web transport without its own TLS handshake limit, the call's connect limit bounding the handshake instead
var phasedWebTransport = func() *http.Transport {
	t := webHTTPTransport.Clone()
	t.TLSHandshakeTimeout = 0
	return t
}()

// One vendor call with every phase held to its limit in tm, the CA bundle the environment names trusted and redirects followed with credentials kept to the first origin
func requestsSend(ctx context.Context, tm httputil.Timeout, method, target string, body []byte, header http.Header) (*sdkResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	client := &http.Client{Transport: httputil.TrustEnvCA(phasedWebTransport), CheckRedirect: httputil.RedirectPolicy(true)}
	resp, err := httputil.Do(client, req, tm)
	if err != nil {
		return nil, err
	}
	return &sdkResponse{status: resp.StatusCode, header: resp.Header, body: resp.Content}, nil
}

// Exa request that ran out of time: a phase limit, or a refused connection reported only once the connect limit had passed
type exaTimeoutError struct{ err error }

func (e *exaTimeoutError) Error() string { return e.err.Error() }

func (e *exaTimeoutError) Unwrap() error { return e.err }

// Failure of one Exa request with its timeouts typed: a dial refused after the connect limit counts as a connect timeout, and every timeout becomes an *exaTimeoutError
func exaTypedTimeouts(err error, tm httputil.Timeout, elapsed time.Duration) error {
	var op *net.OpError
	var dns *net.DNSError
	if errors.As(err, &op) && op.Op == "dial" && !errors.As(err, &dns) && elapsed >= tm.Connect {
		return &exaTimeoutError{fmt.Errorf("connect timed out after %s, the last address tried failed with: %w", tm.Connect, err)}
	}
	var te *httputil.TimeoutError
	if errors.As(err, &te) {
		return &exaTimeoutError{err}
	}
	return err
}
