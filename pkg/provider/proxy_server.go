package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Stripped when forwarding upstream, host and content-length are recomputed, authorization is replaced with the bearer
var proxyHopByHopHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true, "transfer-encoding": true, "upgrade": true, "authorization": true,
}

// Forwarded request drop set, accept-encoding left to the transport so the relayed body is the decoded one
var proxyRequestDropHeaders = withDroppedHeaders(proxyHopByHopHeaders, "accept-encoding")

// Response drop set, the transport decoded the body so its encoding and length no longer apply
var proxyResponseDropHeaders = withDroppedHeaders(proxyHopByHopHeaders, "content-encoding", "content-length")

const (
	DefaultProxyPort     = 8645
	DefaultProxyHost     = "127.0.0.1"
	proxyMaxRequestBytes = 10_000_000
)

// The HTTP stack hermes imports lazily is part of the Go runtime
const aiohttpAvailable = true

var proxyLoopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Sec-Fetch-Site values a browser sends for a request no foreign page made
var proxyUnforgedFetchSites = map[string]bool{"none": true, "same-origin": true}

// On a wildcard bind a DNS rebound page is same origin with its Host, so only none is trusted
var proxyWildcardFetchSites = map[string]bool{"none": true}

func withDroppedHeaders(base map[string]bool, extra ...string) map[string]bool {
	out := maps.Clone(base)
	for _, name := range extra {
		out[name] = true
	}
	return out
}

func requireAiohttp() error {
	if !aiohttpAvailable {
		return errors.New("aiohttp is required for `icebath proxy`. Run `icebath setup` to install it.")
	}
	return nil
}

// Writes an OpenAI style error JSON response
func proxyJSONError(w http.ResponseWriter, status int, message, code string) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// Lowercased host without a trailing dot, an IP literal in its compressed form
func canonicalHost(host string) string {
	host = strings.TrimRight(strings.ToLower(host), ".")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String()
	}
	return host
}

// Hostname and port of a Host header or Origin authority, false when malformed so the request fails closed
func parseAuthority(authority string) (string, int, bool) {
	if authority == "" || strings.ContainsAny(authority, "@/\\?# \t") {
		return "", 0, false
	}
	parsed, err := url.Parse("//" + authority)
	if err != nil {
		return "", 0, false
	}
	port := 80
	if raw := parsed.Port(); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 || n > 65535 {
			return "", 0, false
		}
		if n != 0 {
			port = n
		}
	}
	if parsed.Hostname() == "" {
		return "", 0, false
	}
	return canonicalHost(parsed.Hostname()), port, true
}

// True for an empty or unspecified bind address
func isWildcard(bound string) bool {
	if bound == "" {
		return true
	}
	addr, err := netip.ParseAddr(bound)
	return err == nil && addr.IsUnspecified()
}

// Why a request must be refused, empty when its Host, Origin and Sec-Fetch-Site all pass
func localRequestError(hostHeader string, origin, fetchSite *string, wildcard bool, allowedHosts map[string]bool) string {
	host, port, ok := parseAuthority(hostHeader)
	if !ok || (!wildcard && !allowedHosts[host]) {
		return "host_not_allowed"
	}
	if origin != nil {
		scheme, rest, sep := strings.Cut(strings.TrimSpace(*origin), "://")
		originHost, originPort, originOK := parseAuthority(rest)
		if wildcard || !sep || strings.ToLower(scheme) != "http" || !originOK || originHost != host || originPort != port {
			return "origin_not_allowed"
		}
	}
	trusted := proxyUnforgedFetchSites
	if wildcard {
		trusted = proxyWildcardFetchSites
	}
	if fetchSite != nil && !trusted[strings.ToLower(strings.TrimSpace(*fetchSite))] {
		return "origin_not_allowed"
	}
	return ""
}

// Headers without the dropped names
func filterHeaders(headers http.Header, drop map[string]bool) http.Header {
	out := http.Header{}
	for name, values := range headers {
		if drop[strings.ToLower(name)] {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// Upstream client with the aiohttp timeouts, no total deadline, a 15s connect and a 300s socket read
var proxyUpstreamClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

const proxyUpstreamReadTimeout = 300 * time.Second

// Error response the proxy answers with when the upstream could not be reached
type proxyError struct {
	status  int
	message string
	code    string
}

// Sends the request upstream with the credential, an upstream response or the error the client gets
func openUpstream(ctx context.Context, r *http.Request, relPath string, body []byte, cred UpstreamCredential, log *slog.Logger) (*http.Response, context.CancelFunc, *proxyError) {
	upstreamURL := strings.TrimRight(cred.BaseURL, "/") + relPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	log.Debug(fmt.Sprintf("proxy: forwarding %s %s -> %s (body=%d bytes)", r.Method, relPath, upstreamURL, len(body)))
	reqCtx, cancel := context.WithCancel(ctx)
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(reqCtx, r.Method, upstreamURL, reader)
	if err != nil {
		cancel()
		return nil, nil, &proxyError{500, pyrepr.ExceptionMessage(err), "proxy_error"}
	}
	req.Header = filterHeaders(r.Header, proxyRequestDropHeaders)
	req.Header.Set("Authorization", cred.tokenType()+" "+cred.Bearer)
	resp, err := proxyUpstreamClient.Do(req)
	if err != nil {
		cancel()
		var netErr net.Error
		if (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, &proxyError{504, "upstream request timed out", "upstream_timeout"}
		}
		log.Warn(fmt.Sprintf("proxy: upstream connection failed: %s", pyrepr.ExceptionMessage(err)))
		return nil, nil, &proxyError{502, fmt.Sprintf("upstream connection failed: %s", pyrepr.ExceptionMessage(err)), "upstream_unreachable"}
	}
	return resp, cancel, nil
}

// One upstream body read, the error nil at a clean EOF
type upstreamChunk struct {
	data []byte
	err  error
}

// Relays status, filtered headers and the body chunk by chunk, a missing SSE [DONE] appended after a clean EOF
func streamBack(w http.ResponseWriter, upstream *http.Response, cancel context.CancelFunc, log *slog.Logger) {
	defer cancel()
	defer upstream.Body.Close()
	for name, values := range filterHeaders(upstream.Header, proxyResponseDropHeaders) {
		w.Header()[name] = values
	}
	w.WriteHeader(upstream.StatusCode)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()
	var tracker *SseDoneTracker
	if contentTypeIsSSE(upstream.Header) {
		tracker = &SseDoneTracker{}
	}
	chunks := make(chan upstreamChunk)
	go func() {
		for {
			buf := make([]byte, 32*1024)
			n, err := upstream.Body.Read(buf)
			chunks <- upstreamChunk{data: buf[:n], err: err}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(proxyUpstreamReadTimeout)
	defer timer.Stop()
	for {
		var chunk upstreamChunk
		select {
		case chunk = <-chunks:
		case <-timer.C:
			chunk = upstreamChunk{err: fmt.Errorf("upstream read timed out after %s", proxyUpstreamReadTimeout)}
			cancel()
		}
		if len(chunk.data) > 0 {
			if tracker != nil {
				tracker.Feed(chunk.data)
			}
			if _, err := w.Write(chunk.data); err != nil {
				if tracker != nil {
					tracker.MarkInterrupted()
				}
				log.Warn(fmt.Sprintf("proxy: streaming interrupted: %s", pyrepr.ExceptionMessage(err)))
				return
			}
			flush()
		}
		if chunk.err == nil {
			timer.Reset(proxyUpstreamReadTimeout)
			continue
		}
		if errors.Is(chunk.err, io.EOF) {
			break
		}
		if tracker != nil {
			tracker.MarkInterrupted()
		}
		log.Warn(fmt.Sprintf("proxy: streaming interrupted: %s", pyrepr.ExceptionMessage(chunk.err)))
		return
	}
	if tracker != nil && tracker.ShouldAppendDone() {
		if _, err := w.Write(DoneSSEFrame); err != nil {
			log.Debug(fmt.Sprintf("proxy: DONE append skipped: %s", pyrepr.ExceptionMessage(err)))
			return
		}
		flush()
	}
}

// Writes a JSON document with status 200
func proxyJSONResponse(w http.ResponseWriter, payload map[string]any) {
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// Builds the proxy handler bound to one upstream adapter, every adapter call running on the request goroutine
func createProxyApp(adapter UpstreamAdapter, boundHost string, log *slog.Logger) http.Handler {
	bound := canonicalHost(strings.Trim(boundHost, "[]"))
	wildcard := isWildcard(bound)
	allowedHosts := maps.Clone(proxyLoopbackHosts)
	allowedHosts[bound] = true
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "405: Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		authenticated := adapter.IsAuthenticated()
		proxyJSONResponse(w, map[string]any{"status": "ok", "upstream": adapter.DisplayName(), "authenticated": authenticated})
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		handleProxy(w, r, adapter, log)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var origin, fetchSite *string
		if values, ok := r.Header["Origin"]; ok && len(values) > 0 {
			origin = &values[0]
		}
		if values, ok := r.Header["Sec-Fetch-Site"]; ok && len(values) > 0 {
			fetchSite = &values[0]
		}
		if refusal := localRequestError(r.Host, origin, fetchSite, wildcard, allowedHosts); refusal != "" {
			proxyJSONError(w, http.StatusForbidden, "Request refused: browser requests and foreign Host names are not accepted by this proxy.", refusal)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Forwards one /v1 request, resolving the credential first and retrying once after a 401 or 429
func handleProxy(w http.ResponseWriter, r *http.Request, adapter UpstreamAdapter, log *slog.Logger) {
	relPath := "/" + strings.TrimLeft(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	allowedPaths := adapter.AllowedPaths()
	if !allowedPaths[relPath] {
		allowed := make([]string, 0, len(allowedPaths))
		for p := range allowedPaths {
			allowed = append(allowed, p)
		}
		sort.Strings(allowed)
		proxyJSONError(w, http.StatusNotFound, fmt.Sprintf("Path /v1%s is not forwarded by this proxy. Allowed: %s", relPath, strings.Join(allowed, ", ")), "path_not_allowed")
		return
	}
	cred, err := adapter.GetCredential()
	if err != nil {
		log.Warn(fmt.Sprintf("proxy: credential resolution failed: %s", pyrepr.ExceptionMessage(err)))
		proxyJSONError(w, http.StatusUnauthorized, pyrepr.ExceptionMessage(err), "upstream_auth_failed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proxyMaxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			proxyJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("Maximum request body size %d exceeded", proxyMaxRequestBytes), "request_entity_too_large")
			return
		}
		proxyJSONError(w, http.StatusBadRequest, pyrepr.ExceptionMessage(err), "bad_request")
		return
	}
	upstream, cancel, perr := openUpstream(r.Context(), r, relPath, body, cred, log)
	if perr != nil {
		proxyJSONError(w, perr.status, perr.message, perr.code)
		return
	}
	if upstream.StatusCode == http.StatusUnauthorized || upstream.StatusCode == http.StatusTooManyRequests {
		retry, err := adapter.GetRetryCredential(cred, upstream.StatusCode)
		if err != nil {
			log.Warn(fmt.Sprintf("proxy: retry credential resolution failed: %s", pyrepr.ExceptionMessage(err)))
			retry = nil
		}
		if retry != nil {
			upstream.Body.Close()
			cancel()
			upstream, cancel, perr = openUpstream(r.Context(), r, relPath, body, *retry, log)
			if perr != nil {
				proxyJSONError(w, perr.status, perr.message, perr.code)
				return
			}
		}
	}
	streamBack(w, upstream, cancel, log)
}

// Runs the proxy until the shutdown channel closes or, when it is nil, until SIGINT or SIGTERM arrives
func runProxyServer(ctx context.Context, adapter UpstreamAdapter, host string, port int, shutdown <-chan struct{}, log *slog.Logger) error {
	if err := requireAiohttp(); err != nil {
		return err
	}
	handler := createProxyApp(adapter, host, log)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler}
	log.Info(fmt.Sprintf("proxy: listening on http://%s:%d/v1 -> %s", host, port, adapter.DisplayName()))
	stop := shutdown
	if stop == nil {
		signalCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		stop = signalCtx.Done()
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case <-stop:
	case <-ctx.Done():
	case err := <-served:
		return err
	}
	log.Info("proxy: shutting down")
	return server.Shutdown(context.Background())
}
