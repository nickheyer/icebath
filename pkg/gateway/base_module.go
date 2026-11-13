package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Encodes the file to voip tuned Ogg Opus through ffmpeg, empty when ffmpeg is missing or the encode fails
func TranscodeToOggOpus(ctx context.Context, path, bitrate string, timeout time.Duration, outputPath string) (string, error) {
	return mediacache.TranscodeToOggOpus(ctx, path, bitrate, timeout, outputPath)
}

// Thread routing metadata for an adapter send, nil when the source carries no thread or workspace
func threadMetadataForSource(source *SessionSource, replyTo *string) map[string]any {
	if source == nil {
		return nil
	}
	platform := platformName(source.Platform)
	metadata := map[string]any{}
	if source.ThreadID != nil {
		metadata["thread_id"] = *source.ThreadID
	}
	if platform == "slack" && source.ScopeID != nil && *source.ScopeID != "" {
		metadata["slack_team_id"] = *source.ScopeID
	}
	if len(metadata) == 0 {
		return nil
	}
	if platform == "telegram" && source.ChatType == "dm" {
		metadata["telegram_dm_topic_reply_fallback"] = true
		if tid := *source.ThreadID; tid != "" && tid != "1" {
			metadata["direct_messages_topic_id"] = tid
		}
		anchor := replyTo
		if anchor == nil || *anchor == "" {
			anchor = source.MessageID
		}
		if anchor != nil {
			metadata["telegram_reply_to_message_id"] = *anchor
		}
	}
	if source.Profile != nil {
		if profile := pystr.Strip(*source.Profile); profile != "" {
			metadata["icebath_profile"] = profile
		}
	}
	return metadata
}

// Thread metadata for the event anchored on its reply id
func threadMetadataForEvent(event *MessageEvent) map[string]any {
	return threadMetadataForSource(event.Source, replyAnchorForEvent(event))
}

// Copy of the metadata marked as a notify worthy user visible reply
func markNotifyMetadata(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	out["notify"] = true
	return out
}

// Message id a reply should anchor on for platforms with reply semantics, nil for none
func replyAnchorForEvent(event *MessageEvent) *string {
	if override, _, _ := event.ReplyFields(); override != nil {
		return override
	}
	platform, chatType := "", ""
	var threadID *string
	if event.Source != nil {
		platform, chatType, threadID = platformName(event.Source.Platform), event.Source.ChatType, event.Source.ThreadID
	}
	raw, isDict := event.RawMessage.(map[string]any)
	if event.Internal || platform == "slack" && isDict && pyrepr.Truthy(raw["_icebath_no_thread_response"]) {
		return nil
	}
	threaded := threadID != nil && *threadID != ""
	if platform == "telegram" && threaded {
		if chatType != "dm" {
			return nil
		}
		if event.MessageID != nil && *event.MessageID != "" {
			return event.MessageID
		}
		return event.ReplyToMessageID
	}
	if platform == "feishu" && threaded && event.ReplyToMessageID != nil && *event.ReplyToMessageID != "" {
		return event.ReplyToMessageID
	}
	return event.MessageID
}

var mediaKindKeys = map[string]bool{"audio": true, "video": true, "file": true, "image": true}

// Localized notice that an attachment could not be delivered, naming only the given file name
func mediaFailureText(ctx context.Context, kind, fileName string) string {
	label := kind
	if mediaKindKeys[kind] {
		label = i18n.T(ctx, "gateway.notify.media_kind."+kind, "", nil)
	}
	if fileName != "" {
		return i18n.T(ctx, "gateway.notify.media_delivery_failed_with_name", "", map[string]any{"kind": label, "file_name": fileName})
	}
	return i18n.T(ctx, "gateway.notify.media_delivery_failed", "", map[string]any{"kind": label})
}

// UTF-16 code units of the text, astral characters counting two as Telegram measures them
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Largest code point offset whose prefix measures within the budget
func customUnitToCP(s []rune, budget int, lenFn func(string) int) int {
	if lenFn(string(s)) <= budget {
		return len(s)
	}
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lenFn(string(s[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// Text unchanged or its longest prefix within the measured budget followed by the suffix
func FitPrefix(text string, budget int, measure func(string) int, suffix string) string {
	rs := []rune(text)
	n := customUnitToCP(rs, budget, measure)
	if n == len(rs) {
		return text
	}
	return string(rs[:n]) + suffix
}

// Longest prefix within the UTF-16 limit that never splits a surrogate pair
func prefixWithinUTF16Limit(s string, limit int) string {
	rs := []rune(s)
	return string(rs[:customUnitToCP(rs, limit, UTF16Len)])
}

var lookupBindHost = net.DefaultResolver.LookupIPAddr

// Reports whether binding the host would expose the server beyond loopback, failing closed on DNS errors
func IsNetworkAccessible(ctx context.Context, host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return !addr.Unmap().IsLoopback()
	}
	addrs, err := lookupBindHost(ctx, host)
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			return true
		}
	}
	return false
}

const macOSProxyTTL = 60 * time.Second

var macOSProxyCache struct {
	sync.Mutex
	set   bool
	at    time.Time
	value string
}

var (
	macOSProxyIsDarwin = runtime.GOOS == "darwin"
	macOSProxyNow      = time.Now
)

var scutilProxyOutput = func(ctx context.Context) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "scutil", "--proxy").Output()
	return strings.ToValidUTF8(string(out), "�"), err
}

// System HTTP or HTTPS proxy from scutil on macOS memoised for a minute, empty elsewhere or on error
func detectMacOSSystemProxy(ctx context.Context) string {
	if !macOSProxyIsDarwin {
		return ""
	}
	macOSProxyCache.Lock()
	defer macOSProxyCache.Unlock()
	now := macOSProxyNow()
	if macOSProxyCache.set && now.Sub(macOSProxyCache.at) < macOSProxyTTL {
		return macOSProxyCache.value
	}
	out, err := scutilProxyOutput(ctx)
	resolved := ""
	if err == nil {
		resolved = parseScutilProxy(out)
	}
	macOSProxyCache.set, macOSProxyCache.at, macOSProxyCache.value = true, now, resolved
	return resolved
}

// First enabled HTTPS then HTTP proxy in scutil output as an http URL
func parseScutilProxy(out string) string {
	props := map[string]string{}
	for _, line := range pystr.Splitlines(out, false) {
		if key, val, ok := strings.Cut(pystr.Strip(line), " : "); ok {
			props[pystr.Strip(key)] = pystr.Strip(val)
		}
	}
	for _, keys := range [][3]string{{"HTTPSEnable", "HTTPSProxy", "HTTPSPort"}, {"HTTPEnable", "HTTPProxy", "HTTPPort"}} {
		if props[keys[0]] == "1" && props[keys[1]] != "" && props[keys[2]] != "" {
			return "http://" + props[keys[1]] + ":" + props[keys[2]]
		}
	}
	return ""
}

// Drops the memoised scutil answer so the next lookup reads it again
func ResetMacOSProxyCache() {
	macOSProxyCache.Lock()
	macOSProxyCache.set = false
	macOSProxyCache.Unlock()
}

// Reports whether NO_PROXY matches at least one of the target hosts
func ShouldBypassProxy(targetHosts []string) bool {
	return provider.ShouldBypassProxy(targetHosts, nil)
}

// Proxy URL from the scoped platform var, the configured value, the trusted env or the macOS system proxy
func ResolveProxyURL(ctx context.Context, platformEnvVar string, targetHosts []string, configured string) (string, error) {
	value := ""
	if platformEnvVar != "" {
		scoped, err := GetScopedSecret(ctx, platformEnvVar, "", false)
		if err != nil {
			return "", err
		}
		value = pystr.Strip(scoped)
	}
	if value == "" {
		value = pystr.Strip(configured)
	}
	if value == "" {
		if !GatewayTrustEnv(ctx) {
			return "", nil
		}
		value = provider.FirstProxyEnvValue()
	}
	if value == "" {
		value = detectMacOSSystemProxy(ctx)
	}
	p := config.NormalizeProxyURL(value)
	if p != "" && ShouldBypassProxy(targetHosts) {
		return "", nil
	}
	return p, nil
}

type ProxyConnector struct {
	Type     string
	Host     string
	Port     int
	Username string
	Password string
	dialer   net.Dialer
}

// Connector for a socks5, socks4 or http proxy URL that resolves target names on the proxy side
func NewProxyConnector(proxyURL string) (*ProxyConnector, error) {
	parsed, err := pyurl.Split(proxyURL)
	if err != nil {
		return nil, err
	}
	scheme := pystr.Lower(parsed.Scheme)
	if scheme != "socks5" && scheme != "socks4" && scheme != "http" {
		return nil, &mediacache.ValueError{Msg: "Invalid scheme component: " + scheme}
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Hostname() == "" {
		return nil, &mediacache.ValueError{Msg: "Empty host component"}
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 0 || port > 65535 {
		return nil, &mediacache.ValueError{Msg: "Invalid port component"}
	}
	c := &ProxyConnector{Type: scheme, Host: strings.ToLower(u.Hostname()), Port: port, dialer: net.Dialer{Timeout: 60 * time.Second}}
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			c.Username, c.Password = u.User.Username(), pw
		}
	}
	return c, nil
}

// Opens a connection to the address tunnelled through the proxy
func (c *ProxyConnector) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyAddr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	switch c.Type {
	case "socks5":
		var auth *proxy.Auth
		if c.Username != "" || c.Password != "" {
			auth = &proxy.Auth{User: c.Username, Password: c.Password}
		}
		d, err := proxy.SOCKS5("tcp", proxyAddr, auth, &c.dialer)
		if err != nil {
			return nil, err
		}
		return d.(proxy.ContextDialer).DialContext(ctx, network, addr)
	case "socks4":
		return c.dialSOCKS4(ctx, proxyAddr, addr)
	}
	return c.dialHTTPConnect(ctx, proxyAddr, addr)
}

// Socks4 handshake, the 4a host name form whenever the target is not an IPv4 literal
func (c *ProxyConnector) dialSOCKS4(ctx context.Context, proxyAddr, addr string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, err
	}
	conn, err := c.dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	req := []byte{4, 1, 0, 0}
	binary.BigEndian.PutUint16(req[2:], uint16(port))
	ip := net.ParseIP(host).To4()
	if ip == nil {
		req = append(req, 0, 0, 0, 1)
	} else {
		req = append(req, ip...)
	}
	req = append(append(req, c.Username...), 0)
	if ip == nil {
		req = append(append(req, host...), 0)
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	reply := make([]byte, 8)
	if _, err = conn.Write(req); err == nil {
		_, err = io.ReadFull(conn, reply)
	}
	if err == nil && (reply[0] != 0 || reply[1] != 90) {
		err = fmt.Errorf("SOCKS4 proxy server sent invalid data (reply code %d)", reply[1])
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// HTTP CONNECT tunnel to the address through the proxy
func (c *ProxyConnector) dialHTTPConnect(ctx context.Context, proxyAddr, addr string) (net.Conn, error) {
	conn, err := c.dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\nUser-Agent: icebath-agent\r\n"
	if c.Username != "" || c.Password != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password)) + "\r\n"
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("Proxy server error. Status: %d", resp.StatusCode)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// HTTP transport whose every connection goes through the proxy
func (c *ProxyConnector) Transport() *http.Transport {
	return &http.Transport{DialContext: c.DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second}
}

type ProxyKwargs struct {
	Connector *ProxyConnector
	Proxy     string
}

// Bot client proxy settings, a connector for socks proxies and the plain URL for http ones
func ProxyKwargsForBot(proxyURL string) (ProxyKwargs, error) {
	if proxyURL == "" {
		return ProxyKwargs{}, nil
	}
	if strings.HasPrefix(pystr.Lower(proxyURL), "socks") {
		c, err := NewProxyConnector(proxyURL)
		if err != nil {
			return ProxyKwargs{}, err
		}
		return ProxyKwargs{Connector: c}, nil
	}
	return ProxyKwargs{Proxy: proxyURL}, nil
}

// Session and request proxy settings for a standalone client, every scheme through a connector
func ProxyKwargsForAiohttp(proxyURL string) (session, request ProxyKwargs, err error) {
	if proxyURL == "" {
		return ProxyKwargs{}, ProxyKwargs{}, nil
	}
	c, err := NewProxyConnector(proxyURL)
	if err != nil {
		return ProxyKwargs{}, ProxyKwargs{}, err
	}
	return ProxyKwargs{Connector: c}, ProxyKwargs{}, nil
}

// Read only config section by name, empty when unreadable or not a mapping
func configSection(ctx context.Context, name string) map[string]any {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return map[string]any{}
	}
	if section, ok := tree[name].(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

// Whether gateway HTTP clients honor proxy and certificate env, true unless gateway.trust_env says off
func GatewayTrustEnv(ctx context.Context) bool {
	value, ok := configSection(ctx, "gateway")["trust_env"]
	if !ok || value == nil {
		return true
	}
	if s, isStr := value.(string); isStr {
		switch pystr.Lower(pystr.Strip(s)) {
		case "0", "false", "no", "off":
			return false
		}
		return true
	}
	return pyrepr.Truthy(value)
}

// HTTP client with a total timeout reading proxy env vars only when the gateway trusts the environment
func TrustEnvHTTPClient(ctx context.Context, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if GatewayTrustEnv(ctx) {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Reports whether the host matches a NO_PROXY entry, the given value overriding the environment
func IsHostExcludedByNoProxy(hostname string, noProxyValue *string) bool {
	return provider.ShouldBypassProxy([]string{hostname}, noProxyValue)
}

const GatewaySecretCaptureUnsupportedMessage = "Secure secret entry is not supported over messaging. " +
	"Load this skill in the local CLI to be prompted, or add the key to ~/.icebath/.env manually."

const UnauthorizedActionNotice = "This bot is private and you're not on its allowed list. If you own it, run " +
	"`icebath pairing approve {platform} <request-id>` on the host (`icebath pairing list` shows the id)."

// Localized private bot refusal naming the platform, resolved per refusal so the language follows the config
func UnauthorizedActionNoticeFor(ctx context.Context, platform Platform) string {
	name := string(platform)
	if name == "" {
		name = "<platform>"
	}
	return i18n.T(ctx, "gateway.unauthorized.action_notice", "", map[string]any{"platform": name})
}

// URL safety env carrying the readonly config so private URL allowances apply
func urlSafetyEnv(ctx context.Context) *tools.Env {
	tree, _ := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	return &tools.Env{Home: home.Dir(ctx), Config: tree, TerminalScope: tools.GetTerminalScope(ctx)}
}

// Refuses a redirect whose target is a private or internal address
func SSRFRedirectGuard(env *tools.Env, target string) error {
	if target != "" && !mediaURLIsSafe(env, target) {
		return &mediacache.ValueError{Msg: "Blocked redirect to private/internal address: " + SafeURLForLog(target, 80)}
	}
	return nil
}

// URL without query, fragment or credentials and clipped to the length for logs
func SafeURLForLog(raw string, maxLen int) string {
	if maxLen <= 0 || raw == "" {
		return ""
	}
	parsed, err := pyurl.Split(raw)
	if err != nil {
		return string(clipRunes([]rune(raw), maxLen))
	}
	safe := raw
	if parsed.Scheme != "" && parsed.Netloc != "" {
		tail := ""
		if p := parsed.Path; p != "" && p != "/" {
			base := p[strings.LastIndex(p, "/")+1:]
			if base != "" {
				tail = "/.../" + base
			} else {
				tail = "/..."
			}
		}
		netloc := parsed.Netloc[strings.LastIndex(parsed.Netloc, "@")+1:]
		safe = parsed.Scheme + "://" + netloc + tail
	}
	rs := []rune(safe)
	if len(rs) <= maxLen {
		return safe
	}
	if maxLen <= 3 {
		return strings.Repeat(".", maxLen)
	}
	return string(rs[:maxLen-3]) + "..."
}

func clipRunes(rs []rune, n int) []rune {
	if len(rs) > n {
		return rs[:n]
	}
	return rs
}

type httpStatusError struct {
	Code int
	msg  string
}

func (e *httpStatusError) Error() string { return e.msg }

// Status failure in the wording httpx raise_for_status uses
func newHTTPStatusError(resp *http.Response, rawURL string) *httpStatusError {
	kind := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error", 5: "Server error"}[resp.StatusCode/100]
	if kind == "" {
		kind = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'", kind, resp.StatusCode, http.StatusText(resp.StatusCode), rawURL)
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && loc != "" {
		msg += fmt.Sprintf("\nRedirect location: '%s'", loc)
	}
	msg += fmt.Sprintf("\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", resp.StatusCode)
	return &httpStatusError{Code: resp.StatusCode, msg: msg}
}

var errMediaReadTimeout = errors.New("timed out reading the media response")

var (
	mediaFetchIdleTimeout = 30 * time.Second
	mediaURLIsSafe        = tools.IsSafeURL
	newMediaFetchClient   = func(env *tools.Env) *http.Client { return tools.NewSSRFSafeClient(env, 0, true) }
	mediaRetrySleep       = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
)

// Response body read under the inbound media cap, an oversized declared length rejected before any chunk
func ReadHTTPBodyWithLimit(ctx context.Context, resp *http.Response, mediaType string) ([]byte, error) {
	return readBodyWithLimit(ctx, resp.Body, resp.Header.Get("Content-Length"), mediaType, func() {})
}

// Streams the body under the inbound media cap, rejecting an oversized declared length early
func readBodyWithLimit(ctx context.Context, body io.Reader, contentLength, mediaType string, idle func()) ([]byte, error) {
	h := home.Dir(ctx)
	maxBytes := mediacache.InboundMediaMaxBytes(ctx, h)
	if contentLength != "" {
		if n, ok := pyint.Parse(contentLength); !ok {
			slog.Debug(fmt.Sprintf("Ignoring invalid Content-Length for inbound %s: %s", mediaType, pyrepr.Repr(contentLength)))
		} else if !n.IsInt64() && n.Sign() > 0 && maxBytes != 0 {
			return nil, &mediacache.ValueError{Msg: fmt.Sprintf("Inbound %s payload is too large (%s bytes > %d bytes)", mediaType, n.String(), maxBytes)}
		} else if n.IsInt64() {
			if err := mediacache.ValidateInboundMediaSize(ctx, h, n.Int64(), mediaType, &maxBytes); err != nil {
				return nil, err
			}
		}
	}
	var out bytes.Buffer
	buf := make([]byte, 64*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			idle()
			if verr := mediacache.ValidateInboundMediaSize(ctx, h, int64(out.Len()+n), mediaType, &maxBytes); verr != nil {
				return nil, verr
			}
			out.Write(buf[:n])
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// One SSRF checked size capped GET of the URL, timeouts reported as errMediaReadTimeout
func fetchMediaOnce(ctx context.Context, client *http.Client, rawURL, accept, mediaType string) ([]byte, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fired atomic.Bool
	timer := time.AfterFunc(mediaFetchIdleTimeout, func() { fired.Store(true); cancel() })
	defer timer.Stop()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; icebath-agent/1.0)")
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	timeoutErr := func(err error) error {
		var ne net.Error
		if fired.Load() || errors.As(err, &ne) && ne.Timeout() {
			return errMediaReadTimeout
		}
		return err
	}
	if err != nil {
		var valueErr *mediacache.ValueError
		if errors.As(err, &valueErr) {
			return nil, valueErr
		}
		return nil, timeoutErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newHTTPStatusError(resp, resp.Request.URL.String())
	}
	data, err := readBodyWithLimit(ctx, resp.Body, resp.Header.Get("Content-Length"), mediaType, func() { timer.Reset(mediaFetchIdleTimeout) })
	if err != nil {
		var valueErr *mediacache.ValueError
		if errors.As(err, &valueErr) {
			return nil, err
		}
		return nil, timeoutErr(err)
	}
	return data, nil
}

// Downloads the URL into a media cache with SSRF checks on every hop and linear backoff on timeouts, 429 and 5xx
func cacheMediaFromURL(ctx context.Context, rawURL, ext string, retries int, mediaType, accept string, cacheFn func([]byte, string) (string, error), logLabel string) (string, error) {
	env := urlSafetyEnv(ctx)
	if !mediaURLIsSafe(env, rawURL) {
		return "", &mediacache.ValueError{Msg: "Blocked unsafe URL (SSRF protection): " + SafeURLForLog(rawURL, 80)}
	}
	client := newMediaFetchClient(env)
	inner := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := SSRFRedirectGuard(env, req.URL.String()); err != nil {
			return err
		}
		if inner == nil {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		return inner(req, via)
	}
	for attempt := 0; ; attempt++ {
		content, err := fetchMediaOnce(ctx, client, rawURL, accept, mediaType)
		if err == nil {
			return cacheFn(content, ext)
		}
		var statusErr *httpStatusError
		isStatus := errors.As(err, &statusErr)
		if !isStatus && !errors.Is(err, errMediaReadTimeout) || isStatus && statusErr.Code < 429 || attempt >= retries {
			return "", err
		}
		wait := time.Duration(1.5 * float64(attempt+1) * float64(time.Second))
		slog.Debug(fmt.Sprintf("%s cache retry %d/%d for %s (%.1fs): %s", logLabel, attempt+1, retries, SafeURLForLog(rawURL, 80), wait.Seconds(), err.Error()))
		if err := mediaRetrySleep(ctx, wait); err != nil {
			return "", err
		}
	}
}

// Downloads an image URL into the image cache and returns the absolute path
func CacheImageFromURL(ctx context.Context, rawURL, ext string, retries int) (string, error) {
	return cacheMediaFromURL(ctx, rawURL, ext, retries, "image", "image/*,*/*;q=0.8", func(data []byte, ext string) (string, error) {
		return mediacache.CacheImageFromBytes(ctx, home.Dir(ctx), data, ext)
	}, "Media")
}

// Downloads an audio URL into the audio cache and returns the absolute path
func CacheAudioFromURL(ctx context.Context, rawURL, ext string, retries int) (string, error) {
	return cacheMediaFromURL(ctx, rawURL, ext, retries, "audio", "audio/*,*/*;q=0.8", func(data []byte, ext string) (string, error) {
		return mediacache.CacheAudioFromBytes(ctx, home.Dir(ctx), data, ext)
	}, "Audio")
}

// Scope aware credential read, an unscoped default profile falling back to the process env
func GetScopedSecret(ctx context.Context, name, def string, externalFallback bool) (string, error) {
	val, found, err := LookupScopedSecret(ctx, name, externalFallback)
	if err != nil {
		return "", err
	}
	if !found {
		return def, nil
	}
	return val, nil
}

// Scope aware credential read reporting whether the name is set at all, the None of get_scoped_secret as false
func LookupScopedSecret(ctx context.Context, name string, externalFallback bool) (string, bool, error) {
	val, found, err := provider.GetSecret(ctx, name)
	var unscoped *provider.UnscopedSecretError
	if errors.As(err, &unscoped) {
		val, found = os.LookupEnv(name)
	} else if err != nil {
		return "", false, err
	}
	if !found && externalFallback && provider.CurrentSecretScope(ctx) == nil {
		val, found = unscopedProfileSecrets(ctx)[name]
	}
	return val, found, nil
}

var unscopedProfileSecretsCache struct {
	sync.Mutex
	secrets map[string]string
}

// Process cached secret scope of the profile for startup gates that run before any scope exists
func unscopedProfileSecrets(ctx context.Context) map[string]string {
	unscopedProfileSecretsCache.Lock()
	defer unscopedProfileSecretsCache.Unlock()
	if unscopedProfileSecretsCache.secrets == nil {
		secrets := map[string]string{}
		for k, v := range provider.BuildProfileSecretScope(home.Dir(ctx)) {
			secrets[k] = v
		}
		unscopedProfileSecretsCache.secrets = secrets
	}
	return unscopedProfileSecretsCache.secrets
}
