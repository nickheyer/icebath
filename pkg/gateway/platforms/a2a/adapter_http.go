package a2a

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Error page http.server sends for an unsupported method
const httpErrorPage = `<!DOCTYPE HTML>
<html lang="en">
    <head>
        <meta charset="utf-8">
        <title>Error response</title>
    </head>
    <body>
        <h1>Error response</h1>
        <p>Error code: %d</p>
        <p>Message: %s.</p>
        <p>Error code explanation: %d - %s.</p>
    </body>
</html>
`

// HTTP surface of the adapter, every request served on its own goroutine
type requestHandler struct{ adapter *A2AAdapter }

// One request and its response writer as the handler methods see them
type a2aRequest struct {
	adapter *A2AAdapter
	w       http.ResponseWriter
	r       *http.Request
}

// Guards the host, dispatches GET and POST and aborts the connection on an unhandled error
func (h requestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := &a2aRequest{adapter: h.adapter, w: w, r: r}
	defer func() {
		if rec := recover(); rec != nil {
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			q.handleError(rec)
			panic(http.ErrAbortHandler)
		}
	}()
	if !q.parseRequest() {
		return
	}
	switch r.Method {
	case http.MethodGet:
		q.doGET()
	case http.MethodPost:
		q.doPOST()
	default:
		q.unsupportedMethod()
	}
}

// Without a token refuses a Host that is neither loopback nor the public URL host before routing
func (q *a2aRequest) parseRequest() bool {
	a := q.adapter
	if a.securityContext.LocalhostOnly() && !IsLoopbackHostHeader(q.r.Host, a.publicURL) {
		q.error(http.StatusForbidden, nil, ErrUnauthorized,
			"Host must be a loopback name or the A2A_PUBLIC_URL host (no A2A token configured)")
		return false
	}
	return true
}

// Access and error lines of the server at debug level instead of stderr
func (q *a2aRequest) logMessage(format string, args ...any) {
	slog.Debug(fmt.Sprintf("A2A http: "+format, args...))
}

func (q *a2aRequest) logRequest(code int) {
	q.logMessage(`"%s" %s %s`, q.r.Method+" "+q.r.RequestURI+" "+q.r.Proto, strconv.Itoa(code), "-")
}

// Prints the socketserver report of an exception a request raised
func (q *a2aRequest) handleError(rec any) {
	err, ok := rec.(error)
	if !ok {
		err = fmt.Errorf("%v", rec)
	}
	host, port, _ := net.SplitHostPort(q.r.RemoteAddr)
	addr := fmt.Sprintf("(%s, %s)", pyrepr.StrRepr(host), port)
	if strings.Contains(host, ":") {
		addr = fmt.Sprintf("(%s, %s, 0, 0)", pyrepr.StrRepr(host), port)
	}
	line := strings.Repeat("-", 40)
	fmt.Fprintf(os.Stderr, "%s\nException occurred during processing of request from %s\n%s%s: %s\n%s\n", line, addr,
		debug.Stack(), pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err), line)
}

// JSON body with its content type and length
func (q *a2aRequest) json(code int, payload any) {
	body, err := pyjson.Dumps(payload)
	if err != nil {
		panic(err)
	}
	q.logRequest(code)
	header := q.w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	q.w.WriteHeader(code)
	_, _ = io.WriteString(q.w, body)
}

func (q *a2aRequest) error(httpCode int, reqID any, code int, message string) {
	q.json(httpCode, JsonrpcError(reqID, code, message))
}

// Not implemented page http.server answers a method without a handler with
func (q *a2aRequest) unsupportedMethod() {
	message := fmt.Sprintf("Unsupported method (%s)", pyrepr.StrRepr(q.r.Method))
	q.logMessage("code %d, message %s", http.StatusNotImplemented, message)
	q.logRequest(http.StatusNotImplemented)
	header := q.w.Header()
	header.Set("Connection", "close")
	if q.r.Method == http.MethodHead {
		q.w.WriteHeader(http.StatusNotImplemented)
		return
	}
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(message)
	body := fmt.Sprintf(httpErrorPage, http.StatusNotImplemented, escaped, http.StatusNotImplemented, "Server does not support this operation")
	header.Set("Content-Type", "text/html;charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	q.w.WriteHeader(http.StatusNotImplemented)
	_, _ = io.WriteString(q.w, body)
}

// Peer address, an IPv4 peer of a dual stack bind keeping its plain IPv4 identity
func (q *a2aRequest) clientIP() string {
	ip := q.r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if strings.HasPrefix(ip, "::ffff:") && strings.Contains(ip, ".") {
		return strings.TrimPrefix(ip, "::ffff:")
	}
	return ip
}

// A2A_PUBLIC_URL, else the forwarded or Host header URL, empty meaning the bind host
func (q *a2aRequest) requestPublicURL() string {
	if explicit := q.adapter.publicURL; explicit != "" {
		return explicit
	}
	host := q.r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = q.r.Host
	}
	host, _, _ = strings.Cut(host, ",")
	host = pystr.Strip(host)
	scheme := q.r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	scheme, _, _ = strings.Cut(scheme, ",")
	scheme = pystr.Strip(scheme)
	if host == "" {
		return ""
	}
	return scheme + "://" + host + "/"
}

// First Authorization header, nil when the request carries none
func (q *a2aRequest) authorization() *string {
	if vals := q.r.Header.Values("Authorization"); len(vals) > 0 {
		return &vals[0]
	}
	return nil
}

// Agent Card, metrics and health, served agents shown only to local or authenticated callers
func (q *a2aRequest) doGET() {
	a := q.adapter
	rt := a.routeForPath(q.r.RequestURI)
	agent := rt.agent
	subpath := strings.TrimRight(rt.subpath, "/")
	if subpath == "" {
		subpath = "/"
	}
	publicURL := q.requestPublicURL()
	switch subpath {
	case "/.well-known/agent.json", "/.well-known/agent-card.json":
		q.json(http.StatusOK, a.buildCard(workerContext(), publicURL, agent))
		return
	case "/metrics":
		q.json(http.StatusOK, SharedMetrics.Snapshot())
		return
	case "/", "/health":
	default:
		q.json(http.StatusNotFound, obj("error", "not found"))
		return
	}
	name := agent.Name
	if name == "" {
		name = a.agentName
	}
	payload := obj("status", "ok", "agent", name)
	sec := a.securityContext
	show := sec.LocalhostOnly()
	if !show {
		_, ok, err := sec.Authenticate(q.authorization(), q.clientIP())
		if err != nil {
			panic(err)
		}
		show = ok
	}
	if show {
		payload.Set("served_agents", a.servedAgentSummary(publicURL))
	}
	q.json(http.StatusOK, payload)
}

// Authenticated JSON-RPC request run through the ordered checks then its method handler
func (q *a2aRequest) doPOST() {
	a := q.adapter
	identity, ok, err := a.securityContext.Authenticate(q.authorization(), q.clientIP())
	if err != nil {
		panic(err)
	}
	if !ok {
		q.error(http.StatusUnauthorized, nil, ErrUnauthorized, "unauthorized")
		return
	}
	if a.securityContext.LocalhostOnly() {
		if refusal, refused := BrowserPostRefusal(q.r.Header); refused {
			q.error(http.StatusForbidden, nil, ErrUnauthorized, refusal)
			return
		}
	}
	length := max(q.r.ContentLength, 0)
	if length > maxBody {
		q.error(http.StatusRequestEntityTooLarge, nil, ErrParse, "payload too large")
		return
	}
	raw := []byte("{}")
	if length > 0 {
		raw = make([]byte, length)
		n, _ := io.ReadFull(q.r.Body, raw)
		raw = raw[:n]
	}
	var req any
	text, err := pycodecs.Decode(raw, "utf-8")
	if err == nil {
		req, err = pyjson.Loads(text)
	}
	if err != nil {
		q.error(http.StatusBadRequest, nil, ErrParse, "parse error")
		return
	}
	reqObj, isObj := req.(*pyjson.Object)
	if !isObj || reqObj == nil {
		q.error(http.StatusBadRequest, nil, ErrInvalidParams, "JSON-RPC request must be an object")
		return
	}
	reqID := getOr(reqObj, "id")
	method := pyrepr.Str(pyGet(reqObj, "method", ""))
	var params any = pyjson.NewObject()
	if p := getOr(reqObj, "params"); p != nil {
		params = p
	}
	version := pystr.Strip(q.r.Header.Get("A2A-Version"))
	paramsObj, paramsIsObj := params.(*pyjson.Object)
	var rt route
	if paramsIsObj {
		rt = a.routeForRequest(q.r.RequestURI, paramsObj)
	}
	entry := rpcMethods[method]
	fail := func(httpCode, code int, msg string) {
		if code == ErrRateLimited {
			SharedMetrics.RateLimitTriggers.Add(1)
		}
		q.error(httpCode, reqID, code, msg)
	}
	switch {
	case !paramsIsObj:
		fail(http.StatusOK, ErrInvalidParams, "params must be an object")
		return
	case version != "" && version != "1.0" && version != "1.0.0":
		fail(http.StatusOK, ErrInvalidParams, "unsupported A2A-Version: "+version)
		return
	case rt.err != "":
		fail(http.StatusBadRequest, ErrInvalidParams, rt.err)
		return
	case !a.rateLimiter.Allow(identity):
		fail(http.StatusTooManyRequests, ErrRateLimited, "rate limit exceeded")
		return
	case !a.securityContext.IsTrustedPeer(identity):
		fail(http.StatusForbidden, ErrUntrustedPeer, fmt.Sprintf("peer '%s' not trusted", identity))
		return
	case entry.handler == "":
		fail(http.StatusOK, ErrMethodNotFound, "method not found: "+method)
		return
	}
	agent := rt.agent
	switch entry.handler {
	case rpcMessageSendName:
		q.json(http.StatusOK, a.rpcMessageSend(reqID, paramsObj, identity, agent, entry.v1))
	case rpcMessageStreamName:
		a.rpcMessageStream(q, reqID, paramsObj, identity, agent)
	case rpcTasksSubscribeName:
		a.rpcTasksSubscribe(q, reqID, paramsObj, agent)
	case rpcTasksGetName:
		q.json(http.StatusOK, a.rpcTasksGet(reqID, paramsObj, agent))
	case rpcTasksListName:
		q.json(http.StatusOK, a.rpcTasksList(reqID, paramsObj, agent))
	case rpcTasksCancelName:
		q.json(http.StatusOK, a.rpcTasksCancel(reqID, paramsObj, agent))
	case rpcPushConfigCreateNam:
		q.json(http.StatusOK, a.rpcPushConfigCreate(reqID, paramsObj, agent))
	case rpcPushConfigGetName:
		q.json(http.StatusOK, a.rpcPushConfigGet(reqID, paramsObj, agent))
	case rpcPushConfigListName:
		q.json(http.StatusOK, a.rpcPushConfigList(reqID, paramsObj, agent))
	case rpcPushConfigDelName:
		q.json(http.StatusOK, a.rpcPushConfigDelete(reqID, paramsObj, agent))
	}
}

// OSError a failed socket write raises, broken pipe and reset named like their Python classes
func sockError(err error) error {
	switch {
	case errors.Is(err, syscall.EPIPE):
		return &PyError{"BrokenPipeError", pyhttpx.OSErrorText(err)}
	case errors.Is(err, syscall.ECONNRESET):
		return &PyError{"ConnectionResetError", pyhttpx.OSErrorText(err)}
	}
	return &PyError{"OSError", pyhttpx.OSErrorText(err)}
}

// True for the broken pipe and reset errors a gone stream client raises
func clientGone(err error) bool {
	var perr *PyError
	return errors.As(err, &perr) && (perr.Type == "BrokenPipeError" || perr.Type == "ConnectionResetError")
}

// Event stream headers, the connection closing after the stream since closure signals the terminal state
func (q *a2aRequest) sseHeaders() {
	q.logRequest(http.StatusOK)
	header := q.w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "close")
	q.w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(q.w).Flush(); err != nil {
		panic(sockError(err))
	}
}

// Writes and flushes one SSE chunk, a broken pipe or reset meaning the client is gone
func (q *a2aRequest) sseWrite(chunk string) error {
	_, err := io.WriteString(q.w, chunk)
	if err == nil {
		err = http.NewResponseController(q.w).Flush()
	}
	if err == nil {
		return nil
	}
	serr := sockError(err)
	if !clientGone(serr) {
		panic(serr)
	}
	return serr
}

// Keepalive comment whose any failure means the client is gone
func (q *a2aRequest) keepalive() error {
	_, err := io.WriteString(q.w, ": keepalive\n\n")
	if err == nil {
		err = http.NewResponseController(q.w).Flush()
	}
	return err
}

// Reason phrase of a response status line
func statusReason(resp *http.Response) string {
	return strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" ")
}

// Redirect policy of urlopen, a POST refused on 307 and 308 and loops capped at ten hops or four repeats
func urlopenRedirect(req *http.Request, via []*http.Request) error {
	resp := req.Response
	method := via[len(via)-1].Method
	code := resp.StatusCode
	if !((code == 301 || code == 302 || code == 303 || code == 307 || code == 308) && (method == http.MethodGet || method == http.MethodHead) ||
		(code == 301 || code == 302 || code == 303) && method == http.MethodPost) {
		return &HTTPError{Code: code, Reason: statusReason(resp)}
	}
	visited := map[string]int{}
	for _, v := range via[1:] {
		visited[v.URL.String()]++
	}
	if visited[req.URL.String()] >= 4 || len(visited) >= 10 {
		return &HTTPError{Code: code, Reason: "The HTTP server returned a redirect error that would lead to an infinite loop.\n" +
			"The last 30x error message was:\n" + statusReason(resp)}
	}
	return nil
}

// URLError text of a failed request, a timeout before the response surfacing as timed out
func urlopenError(err error) error {
	var opErr *net.OpError
	dialFailed := errors.As(err, &opErr) && opErr.Op == "dial"
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if dialFailed {
			return &PyError{"URLError", "<urlopen error timed out>"}
		}
		return &PyError{"TimeoutError", "timed out"}
	}
	return &PyError{"URLError", fmt.Sprintf("<urlopen error %s>", pyhttpx.OSErrorText(err))}
}

// POSTs a JSON payload like urlopen and returns the final status, the HTTPError for a non success one
func urlopenPost(ctx context.Context, rawURL string, payload any, headers map[string]string, timeout time.Duration) (int, error) {
	body, err := pyjson.Dumps(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return 0, &PyError{"ValueError", "unknown url type: " + pyrepr.StrRepr(rawURL)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: pyurl.UrlopenProxy}, CheckRedirect: urlopenRedirect}
	resp, err := client.Do(req)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			return 0, httpErr
		}
		return 0, urlopenError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, &HTTPError{Code: resp.StatusCode, Reason: statusReason(resp)}
	}
	return resp.StatusCode, nil
}
