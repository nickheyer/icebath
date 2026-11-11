package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Reason phrases of Python http.HTTPStatus where the Go status text differs
var pyStatusPhrases = map[int]string{413: "Content Too Large", 414: "URI Too Long", 416: "Range Not Satisfiable",
	418: "I'm a Teapot", 422: "Unprocessable Content"}

// Python HTTPStatus phrase of a status code, empty for an unknown code
func webReason(status int) string {
	if phrase, ok := pyStatusPhrases[status]; ok {
		return phrase
	}
	return http.StatusText(status)
}

// WebResponse is an aiohttp web.Response with its status, headers and body bytes
type WebResponse struct {
	Status   int
	Headers  http.Header
	Body     []byte
	prepared bool
	streamed int
}

// Response with an empty body and the given headers
func NewWebResponse(status int, headers map[string]string) *WebResponse {
	out := &WebResponse{Status: status, Headers: http.Header{}}
	for k, v := range headers {
		out.Headers.Set(k, v)
	}
	return out
}

// Text response with the aiohttp utf-8 charset, text/plain unless a content type is given
func WebTextResponse(status int, text, contentType string, headers map[string]string) *WebResponse {
	out := NewWebResponse(status, headers)
	if out.Headers.Get("Content-Type") == "" {
		if contentType == "" {
			contentType = "text/plain"
		}
		out.Headers.Set("Content-Type", contentType+"; charset=utf-8")
	}
	out.Body = []byte(text)
	return out
}

// JSON response of json.dumps with its default separators under application/json
func WebJSONResponse(data any, status int, headers map[string]string) (*WebResponse, error) {
	text, err := pyjson.Dumps(data)
	if err != nil {
		return nil, err
	}
	return WebTextResponse(status, text, "application/json", headers), nil
}

// Body decoded as text
func (r *WebResponse) Text() string { return string(r.Body) }

// Mime type of the Content-Type header, application/octet-stream when none is set
func (r *WebResponse) ContentType() string {
	raw := r.Headers.Get("Content-Type")
	if raw == "" {
		return "application/octet-stream"
	}
	mime, _, _ := strings.Cut(raw, ";")
	return strings.ToLower(strings.TrimSpace(mime))
}

// WebHTTPError is a raised aiohttp HTTPException answered with its status, text and headers
type WebHTTPError struct {
	Status  int
	Text    string
	Headers map[string]string
}

// Raised HTTP exception with explicit text
func NewWebHTTPError(status int, text string, headers map[string]string) *WebHTTPError {
	return &WebHTTPError{Status: status, Text: text, Headers: headers}
}

// Raised HTTP exception with the aiohttp default text of status and reason
func DefaultWebHTTPError(status int, headers map[string]string) *WebHTTPError {
	return NewWebHTTPError(status, fmt.Sprintf("%d: %s", status, webReason(status)), headers)
}

func (e *WebHTTPError) Error() string { return webReason(e.Status) }

// Response the exception answers
func (e *WebHTTPError) Response() *WebResponse {
	return WebTextResponse(e.Status, e.Text, "", e.Headers)
}

// Shared request body stream every clone of a request reads from
type webBody struct {
	mu sync.Mutex
	r  io.Reader
}

// WebRequest is an aiohttp web.Request, its body read once under the application's client body cap
type WebRequest struct {
	Method        string
	RelURL        *url.URL
	Header        http.Header
	Remote        string
	ContentLength *int64
	ProtoMajor    int
	ProtoMinor    int
	MatchInfo     map[string]string
	ClientMaxSize int64
	writer        http.ResponseWriter
	body          *webBody
	readBytes     []byte
	read          bool
}

// Request view of a net/http request with the remote peer address and the given body cap
func NewWebRequest(r *http.Request, clientMaxSize int64) *WebRequest {
	rel := &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery, Fragment: r.URL.Fragment}
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	var length *int64
	if r.ContentLength >= 0 && (r.ContentLength > 0 || r.Header.Get("Content-Length") != "") {
		n := r.ContentLength
		length = &n
	}
	var body io.Reader = http.NoBody
	if r.Body != nil {
		body = r.Body
	}
	return &WebRequest{Method: r.Method, RelURL: rel, Header: r.Header, Remote: remote, ContentLength: length,
		ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor, MatchInfo: map[string]string{},
		ClientMaxSize: clientMaxSize, body: &webBody{r: body}}
}

// Copy of the request at another relative URL with its own body cap, sharing the body stream
func (r *WebRequest) Clone(relURL *url.URL, clientMaxSize int64) *WebRequest {
	clone := *r
	clone.RelURL = relURL
	clone.ClientMaxSize = clientMaxSize
	clone.MatchInfo = map[string]string{}
	clone.readBytes, clone.read = nil, false
	return &clone
}

// Parsed query string keeping blank values
func (r *WebRequest) Query() url.Values {
	values, _ := url.ParseQuery(r.RelURL.RawQuery)
	return values
}

// First query value of the key, empty when absent
func (r *WebRequest) QueryGet(key string) string {
	return r.Query().Get(key)
}

// Whole body, the 413 HTTP exception once the body reaches the client body cap
func (r *WebRequest) Read() ([]byte, error) {
	if r.read {
		return r.readBytes, nil
	}
	r.body.mu.Lock()
	defer r.body.mu.Unlock()
	var body []byte
	buf := make([]byte, 64*1024)
	for {
		n, err := r.body.r.Read(buf)
		body = append(body, buf[:n]...)
		if r.ClientMaxSize > 0 && int64(len(body)) >= r.ClientMaxSize {
			return nil, NewWebHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"Maximum request body size %d exceeded, actual body size %d", r.ClientMaxSize, len(body)), nil)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	r.readBytes, r.read = body, true
	return body, nil
}

// Request path with every escape decoded except slash and percent, the yarl path_safe form
func (r *WebRequest) PathSafe() string {
	return pathSafe(r.RelURL.EscapedPath())
}

// Decodes percent escapes that form valid UTF-8, keeping %2F and %25 encoded in upper case
func pathSafe(escaped string) string {
	var out strings.Builder
	var pending []byte
	flush := func() {
		for len(pending) > 0 {
			r, size := utf8.DecodeRune(pending)
			if r == utf8.RuneError && size <= 1 {
				out.WriteString(fmt.Sprintf("%%%02X", pending[0]))
				pending = pending[1:]
				continue
			}
			out.Write(pending[:size])
			pending = pending[size:]
		}
	}
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == '%' && i+2 < len(escaped) && isHex(escaped[i+1]) && isHex(escaped[i+2]) {
			b, _ := strconv.ParseUint(escaped[i+1:i+3], 16, 8)
			i += 2
			if b == '/' || b == '%' {
				flush()
				out.WriteString(fmt.Sprintf("%%%02X", b))
				continue
			}
			pending = append(pending, byte(b))
			continue
		}
		flush()
		out.WriteByte(c)
	}
	flush()
	return out.String()
}

// Whether the byte is a hexadecimal digit
func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// WebHandler answers one request like an aiohttp handler coroutine, an error raising out of it
type WebHandler func(ctx context.Context, request *WebRequest) (*WebResponse, error)

// One routed path of the router with its method table
type webResource struct {
	origPath string
	path     string
	pattern  *regexp.Regexp
	names    []string
	routes   map[string]WebHandler
	anyRoute WebHandler
	allowed  map[string]bool
	dynamic  bool
	indexKey string
}

// Match info of the request path, nil when the resource does not route it
func (res *webResource) match(path string) map[string]string {
	if !res.dynamic {
		if res.path == path {
			return map[string]string{}
		}
		return nil
	}
	m := res.pattern.FindStringSubmatch(path)
	if m == nil {
		return nil
	}
	info := map[string]string{}
	for i, name := range res.pattern.SubexpNames() {
		if i == 0 || name == "" {
			continue
		}
		value := m[i]
		if strings.Contains(value, "%") {
			value = strings.ReplaceAll(strings.ReplaceAll(value, "%2F", "/"), "%25", "%")
		}
		info[name] = value
	}
	return info
}

var (
	routePartRE   = regexp.MustCompile(`(\{[_a-zA-Z][^{}]*(?:\{[^{}]*\}[^{}]*)*\})`)
	dynamicVarRE  = regexp.MustCompile(`^\{([_a-zA-Z][_a-zA-Z0-9]*)\}$`)
	dynamicWithRE = regexp.MustCompile(`^\{([_a-zA-Z][_a-zA-Z0-9]*):(.+)\}$`)
)

// Characters the yarl path quoter leaves as they are
const pathQuoteSafe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~!$&'()*+,;=:@/"

// Percent encodes the characters a URL path may not carry while keeping existing escapes
func requotePath(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '%' && i+2 < len(value) && isHex(value[i+1]) && isHex(value[i+2]) {
			out.WriteString("%" + strings.ToUpper(value[i+1:i+3]))
			i += 2
			continue
		}
		if strings.IndexByte(pathQuoteSafe, c) >= 0 {
			out.WriteByte(c)
			continue
		}
		out.WriteString(fmt.Sprintf("%%%02X", c))
	}
	return out.String()
}

// Resource for a route path, a regexp resource when it holds a named variable part
func newWebResource(path string) (*webResource, error) {
	res := &webResource{origPath: path, path: path, routes: map[string]WebHandler{}, allowed: map[string]bool{}}
	if !strings.ContainsAny(path, "{}") && !routePartRE.MatchString(path) {
		res.indexKey = strings.TrimRight(path, "/")
		if res.indexKey == "" {
			res.indexKey = "/"
		}
		return res, nil
	}
	pattern, formatter := "", ""
	parts := splitKeep(routePartRE, path)
	for _, part := range parts {
		if m := dynamicVarRE.FindStringSubmatch(part); m != nil {
			pattern += fmt.Sprintf("(?P<%s>[^{}/]+)", m[1])
			formatter += "{" + m[1] + "}"
			continue
		}
		if m := dynamicWithRE.FindStringSubmatch(part); m != nil {
			pattern += fmt.Sprintf("(?P<%s>%s)", m[1], m[2])
			formatter += "{" + m[1] + "}"
			continue
		}
		if strings.ContainsAny(part, "{}") {
			return nil, fmt.Errorf("ValueError: Invalid path '%s'['%s']", path, part)
		}
		part = requotePath(part)
		formatter += part
		pattern += regexp.QuoteMeta(part)
	}
	compiled, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		return nil, fmt.Errorf("ValueError: Bad pattern '%s': %v", pattern, err)
	}
	res.pattern, res.dynamic = compiled, true
	key := formatter
	if strings.Contains(key, "{") {
		before, _, _ := strings.Cut(key, "{")
		if idx := strings.LastIndex(before, "/"); idx >= 0 {
			key = before[:idx]
		} else {
			key = ""
		}
	}
	res.indexKey = strings.TrimRight(key, "/")
	if res.indexKey == "" {
		res.indexKey = "/"
	}
	return res, nil
}

// Splits text around the regexp's matches keeping each match as its own part, the re.split form with a group
func splitKeep(re *regexp.Regexp, text string) []string {
	var parts []string
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		parts = append(parts, text[last:loc[0]], text[loc[0]:loc[1]])
		last = loc[1]
	}
	return append(parts, text[last:])
}

// WebApplication is an aiohttp web.Application, an ordered router with a body cap that freezes once served
type WebApplication struct {
	ClientMaxSize int64
	Middlewares   []WebMiddleware
	State         map[string]any
	mu            sync.Mutex
	resources     []*webResource
	index         map[string][]*webResource
	frozen        bool
}

// WebMiddleware wraps the resolved handler of every request, the aiohttp new style middleware
type WebMiddleware func(ctx context.Context, request *WebRequest, handler WebHandler) (*WebResponse, error)

// Application with the given client body cap, the aiohttp default of 1 MiB when zero
func NewWebApplication(clientMaxSize int64) *WebApplication {
	if clientMaxSize == 0 {
		clientMaxSize = 1024 * 1024
	}
	return &WebApplication{ClientMaxSize: clientMaxSize, State: map[string]any{}, index: map[string][]*webResource{}}
}

// Resource for the path, the last one reused when it has the same path
func (a *WebApplication) addResource(path string) (*webResource, error) {
	if path != "" && !strings.HasPrefix(path, "/") {
		return nil, errors.New("ValueError: path should be started with / or be empty")
	}
	if n := len(a.resources); n > 0 && a.resources[n-1].origPath == path {
		return a.resources[n-1], nil
	}
	res, err := newWebResource(path)
	if err != nil {
		return nil, err
	}
	if a.frozen {
		return nil, errors.New("RuntimeError: Cannot register a resource into frozen router.")
	}
	a.resources = append(a.resources, res)
	a.index[res.indexKey] = append(a.index[res.indexKey], res)
	return res, nil
}

// Adds one method route to a resource, refused when the method or a catch all route is already there
func (res *webResource) addRoute(method string, handler WebHandler) error {
	if _, taken := res.routes[method]; taken || res.anyRoute != nil {
		registered := method
		if res.anyRoute != nil && !taken {
			registered = "*"
		}
		return fmt.Errorf("RuntimeError: Added route will never be executed, method %s is already registered", registered)
	}
	if method == "*" {
		res.anyRoute = handler
	}
	res.allowed[method] = true
	res.routes[method] = handler
	return nil
}

// Registers a handler for a method and path
func (a *WebApplication) AddRoute(method, path string, handler WebHandler) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	res, err := a.addResource(path)
	if err != nil {
		return err
	}
	return res.addRoute(strings.ToUpper(method), handler)
}

// Registers a GET handler and its HEAD twin
func (a *WebApplication) AddGet(path string, handler WebHandler) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	res, err := a.addResource(path)
	if err != nil {
		return err
	}
	if err := res.addRoute(http.MethodHead, handler); err != nil {
		return err
	}
	return res.addRoute(http.MethodGet, handler)
}

// Registers a POST handler
func (a *WebApplication) AddPost(path string, handler WebHandler) error {
	return a.AddRoute(http.MethodPost, path, handler)
}

// Freezes the router so no resource can be added and empty plain paths serve the root
func (a *WebApplication) Freeze() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frozen = true
	for _, res := range a.resources {
		if !res.dynamic && res.path == "" {
			res.path = "/"
		}
	}
}

// Whether the router is frozen
func (a *WebApplication) Frozen() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.frozen
}

// Handler and match info routing the request, else the 404 or 405 HTTP exception of the router
func (a *WebApplication) Resolve(request *WebRequest) (WebHandler, map[string]string, *WebHTTPError) {
	a.mu.Lock()
	defer a.mu.Unlock()
	allowed := map[string]bool{}
	path := request.PathSafe()
	part := path
	for part != "" {
		for _, res := range a.index[part] {
			info := res.match(path)
			if info == nil {
				continue
			}
			handler, ok := res.routes[request.Method]
			if !ok {
				handler = res.anyRoute
			}
			if handler != nil {
				return handler, info, nil
			}
			for method := range res.allowed {
				allowed[method] = true
			}
		}
		if part == "/" {
			break
		}
		idx := strings.LastIndex(part, "/")
		if idx <= 0 {
			part = "/"
		} else {
			part = part[:idx]
		}
	}
	if len(allowed) > 0 {
		methods := make([]string, 0, len(allowed))
		for method := range allowed {
			methods = append(methods, method)
		}
		slices.Sort(methods)
		return nil, nil, DefaultWebHTTPError(http.StatusMethodNotAllowed, map[string]string{"Allow": strings.Join(methods, ",")})
	}
	return nil, nil, DefaultWebHTTPError(http.StatusNotFound, nil)
}

// Routes and runs the request like Application._handle, the router's HTTP exception raised as an error
func (a *WebApplication) Handle(ctx context.Context, request *WebRequest) (*WebResponse, error) {
	handler, info, refusal := a.Resolve(request)
	if refusal != nil {
		handler = func(context.Context, *WebRequest) (*WebResponse, error) { return nil, refusal }
	} else {
		request.MatchInfo = info
	}
	for i := len(a.Middlewares) - 1; i >= 0; i-- {
		middleware, next := a.Middlewares[i], handler
		handler = func(ctx context.Context, request *WebRequest) (*WebResponse, error) {
			return middleware(ctx, request, next)
		}
	}
	resp, err := handler(ctx, request)
	if err == nil && resp == nil {
		err = errors.New("Missing return statement on request handler")
	}
	return resp, err
}

// A raised HTTP exception as its own response, any other error as a logged 500, else the handler response
func WebResponseFor(request *WebRequest, resp *WebResponse, err error) *WebResponse {
	if err == nil {
		return resp
	}
	var httpErr *WebHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Response()
	}
	slog.Default().With("logger", "aiohttp.server").Error("Error handling request", "error", err)
	title := "500 Internal Server Error"
	msg := "Server got itself in trouble"
	if strings.Contains(request.Header.Get("Accept"), "text/html") {
		return WebTextResponse(http.StatusInternalServerError, fmt.Sprintf(
			"<html><head><title>%s</title></head><body>\n<h1>%s</h1>\n%s\n</body></html>\n", title, title, msg), "text/html", nil)
	}
	return WebTextResponse(http.StatusInternalServerError, title+"\n\n"+msg, "", nil)
}

// Writes a response to the wire with the aiohttp octet stream default for an untyped body
func writeWebResponse(w http.ResponseWriter, resp *WebResponse) {
	if resp.prepared {
		return
	}
	for k, values := range resp.Headers {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	if len(resp.Body) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(resp.Body)))
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// Access log options of a bound listener, the aiohttp.access logger unless disabled or replaced
type AccessLog struct {
	Disabled bool
	Logger   *slog.Logger
}

// Bind options of a listener, the asyncio reuse address default when nil
type ListenOptions struct {
	ReuseAddress *bool
	AccessLog    AccessLog
}

// WebAppRunner is an aiohttp AppRunner with its started TCP sites
type WebAppRunner struct {
	app       *WebApplication
	accessLog *slog.Logger
	servers   []*http.Server
	listeners []net.Listener
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Bound addresses of the started sites
func (rn *WebAppRunner) Addrs() []net.Addr {
	out := make([]net.Addr, 0, len(rn.listeners))
	for _, l := range rn.listeners {
		out = append(out, l.Addr())
	}
	return out
}

// Port of the first started site, zero when none is bound
func (rn *WebAppRunner) Port() int {
	for _, l := range rn.listeners {
		if tcp, ok := l.Addr().(*net.TCPAddr); ok {
			return tcp.Port
		}
	}
	return 0
}

// Serves one wire request through the application and writes the access log line
func (rn *WebAppRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	request := NewWebRequest(r, rn.app.ClientMaxSize)
	request.writer = w
	resp, err := rn.app.Handle(rn.ctx, request)
	resp = WebResponseFor(request, resp, err)
	writeWebResponse(w, resp)
	rn.logAccess(request, resp, started)
}

// Access log line in the aiohttp default format
func (rn *WebAppRunner) logAccess(request *WebRequest, resp *WebResponse, started time.Time) {
	if rn.accessLog == nil || !rn.accessLog.Enabled(context.Background(), slog.LevelInfo) {
		return
	}
	remote := request.Remote
	if remote == "" {
		remote = "-"
	}
	header := func(name string) string {
		if v := request.Header.Get(name); v != "" {
			return v
		}
		return "-"
	}
	size := len(resp.Body) + resp.streamed
	if request.Method == http.MethodHead {
		size = 0
	}
	stamp := started.In(time.FixedZone("", standardUTCOffset(started)))
	target := request.RelURL.EscapedPath()
	if request.RelURL.RawQuery != "" {
		target += "?" + request.RelURL.RawQuery
	}
	rn.accessLog.Info(fmt.Sprintf(`%s %s "%s %s HTTP/%d.%d" %d %d "%s" "%s"`, remote, stamp.Format("[02/Jan/2006:15:04:05 -0700]"),
		request.Method, target, request.ProtoMajor, request.ProtoMinor, resp.Status, size, header("Referer"), header("User-Agent")))
}

// Offset of local standard time in seconds east of UTC, the -time.timezone of Python
func standardUTCOffset(now time.Time) int {
	_, jan := time.Date(now.Year(), time.January, 1, 0, 0, 0, 0, time.Local).Zone()
	_, jul := time.Date(now.Year(), time.July, 1, 0, 0, 0, 0, time.Local).Zone()
	return min(jan, jul)
}

// Hosts a listen address resolves to, both wildcards for the dual stack default
func listenHosts(ctx context.Context, host *string) ([]string, error) {
	if host == nil || *host == "" {
		return []string{"::", "0.0.0.0"}, nil
	}
	if ip := net.ParseIP(*host); ip != nil {
		return []string{*host}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, *host)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, addr := range addrs {
		text := addr.String()
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out, nil
}

// Starts the application on every address of host and port and returns its runner
func StartWebApp(ctx context.Context, app *WebApplication, host *string, port int, opts ListenOptions) (*WebAppRunner, error) {
	hosts, err := listenHosts(ctx, host)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	rn := &WebAppRunner{app: app, ctx: runCtx, cancel: cancel}
	if !opts.AccessLog.Disabled {
		rn.accessLog = opts.AccessLog.Logger
		if rn.accessLog == nil {
			rn.accessLog = slog.Default().With("logger", "aiohttp.access")
		}
	}
	app.Freeze()
	for _, h := range hosts {
		network := "tcp4"
		if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
			network = "tcp6"
		}
		lc := net.ListenConfig{Control: reuseAddressControl(opts.ReuseAddress)}
		l, err := lc.Listen(ctx, network, net.JoinHostPort(h, strconv.Itoa(port)))
		if err != nil {
			if host == nil || *host == "" {
				if errors.Is(err, errAddressFamilyUnsupported) {
					continue
				}
			}
			_ = rn.Cleanup(ctx)
			return nil, fmt.Errorf("error while attempting to bind on address %s: %w", pyAddrRepr(h, port), err)
		}
		rn.listeners = append(rn.listeners, l)
		server := &http.Server{Handler: rn, BaseContext: func(net.Listener) context.Context { return runCtx }}
		rn.servers = append(rn.servers, server)
		rn.wg.Add(1)
		go func() {
			defer rn.wg.Done()
			_ = server.Serve(l)
		}()
	}
	return rn, nil
}

// Python repr of a bound socket address tuple
func pyAddrRepr(host string, port int) string {
	return fmt.Sprintf("('%s', %d)", host, port)
}

// Stops the sites, waits up to the aiohttp 60 second shutdown timeout for handlers and then cancels them
func (rn *WebAppRunner) Cleanup(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	var firstErr error
	for _, server := range rn.servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			if firstErr == nil && !errors.Is(err, context.DeadlineExceeded) {
				firstErr = err
			}
		}
	}
	for _, l := range rn.listeners {
		_ = l.Close()
	}
	rn.cancel()
	rn.wg.Wait()
	rn.servers, rn.listeners = nil, nil
	return firstErr
}
