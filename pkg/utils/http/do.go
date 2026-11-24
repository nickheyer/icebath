package httputil

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Most memory Do reserves up front from a declared body length, a larger body growing the buffer as its bytes arrive
const maxPreallocatedBody = 4 << 20

// Response with its body still open: each read must return within the read limit, time between reads not counted, and Close ends the exchange
type StreamResponse struct {
	Response
	Body io.ReadCloser
	// Declared body length, -1 or 0 when unknown
	length int64
}

// Sends req through client with every phase held to its limit in t and reads the whole body; every failure is a *url.Error whose cause is a *TimeoutError when a limit ran out
func Do(client *http.Client, req *http.Request, t Timeout) (*Response, error) {
	s, err := Stream(client, req, t)
	if err != nil {
		return nil, err
	}
	defer s.Body.Close()
	body, err := readBody(s.Body, s.length)
	if err != nil {
		return nil, &url.Error{Op: errorOp(req.Method), URL: redactPassword(s.URL), Err: err}
	}
	resp := s.Response
	resp.Content = body
	return &resp, nil
}

// Sends req through client with every phase held to its limit in t and hands back the response with its body open; the caller closes it
func Stream(client *http.Client, req *http.Request, t Timeout) (*StreamResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithCancel(req.Context())
	w := newWatchdog(t, cancel)
	resp, err := client.Do(w.request(ctx, req))
	if err != nil {
		w.stop()
		cancel()
		return nil, w.clientError(err)
	}
	w.respond()
	final := req.URL.String()
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &StreamResponse{
		Response: Response{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header, URL: final},
		Body:     &watchedBody{rc: resp.Body, w: w, cancel: cancel},
		length:   resp.ContentLength,
	}, nil
}

// Whole body, the buffer sized from the declared length up to maxPreallocatedBody so a server announcing a huge body cannot make the client reserve it before sending it
func readBody(r io.Reader, length int64) ([]byte, error) {
	if length <= 0 {
		return io.ReadAll(r)
	}
	buf := bytes.NewBuffer(make([]byte, 0, min(length, maxPreallocatedBody)+bytes.MinRead))
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

// Operation name net/http gives a *url.Error, the method with only its first letter in upper case
func errorOp(method string) string {
	if method == "" {
		return "Get"
	}
	return method[:1] + strings.ToLower(method[1:])
}
