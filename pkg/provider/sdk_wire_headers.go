package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Versions of the pinned openai SDK and httpx whose wire identity the model clients present
const (
	openAISDKVersion = "2.54.0"
	httpxVersion     = "0.28.1"
)

// Content codings the pinned httpx decodes on a default install, with neither brotli nor zstandard importable
var httpxDecoders = []string{"gzip", "deflate"}

// Default headers of an httpx.Client built with none of its own
func httpxClientHeaders() *pyhttpx.Headers {
	return pyhttpx.NewHeaders(
		pyhttpx.HeaderLine{Name: "Accept", Value: "*/*"},
		pyhttpx.HeaderLine{Name: "Accept-Encoding", Value: strings.Join(httpxDecoders, ", ")},
		pyhttpx.HeaderLine{Name: "Connection", Value: "keep-alive"},
		pyhttpx.HeaderLine{Name: "User-Agent", Value: "python-httpx/" + httpxVersion})
}

// Operating system label get_platform of a Stainless SDK reports for this host
func stainlessOS() string {
	switch runtime.GOOS {
	case "ios":
		return "iOS"
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "linux":
		return "Linux"
	}
	if label := strings.ToLower(pyplatform.Platform()); label != "" {
		return "Other:" + label
	}
	return "Unknown"
}

// Architecture label get_architecture of a Stainless SDK reports for this host
func stainlessArch() string {
	machine := strings.ToLower(pyplatform.Machine())
	switch {
	case machine == "arm64" || machine == "aarch64":
		return "arm64"
	case machine == "arm":
		return "arm"
	case machine == "x86_64":
		return "x64"
	case strconv.IntSize == 32:
		return "x32"
	case machine != "":
		return "other:" + machine
	}
	return "unknown"
}

// Patch release of the Python runtime the lock pins without its build tag, unknown when it pins none
func pinnedPythonRuntimeVersion() string {
	version, ok := pm.NewLockfile(pm.LockfilePath()).Version("python")
	if !ok || version == "" {
		return "unknown"
	}
	version, _, _ = strings.Cut(version, "+")
	return version
}

// X-Stainless platform headers of a Stainless SDK at the given version on the pinned CPython
func stainlessPlatformHeaders(version string) []pyhttpx.HeaderLine {
	return []pyhttpx.HeaderLine{
		{Name: "X-Stainless-Lang", Value: "python"},
		{Name: "X-Stainless-Package-Version", Value: version},
		{Name: "X-Stainless-OS", Value: stainlessOS()},
		{Name: "X-Stainless-Arch", Value: stainlessArch()},
		{Name: "X-Stainless-Runtime", Value: "CPython"},
		{Name: "X-Stainless-Runtime-Version", Value: pinnedPythonRuntimeVersion()},
	}
}

// Ordered str keyed dict with the case sensitive key identity of a Python dict, a removal standing for Omit
type sdkHeaderDict struct{ lines []pyhttpx.HeaderLine }

// Replaces the value of an existing key in place, else appends the key
func (d *sdkHeaderDict) set(name, value string) {
	for i, l := range d.lines {
		if l.Name == name {
			d.lines[i].Value = value
			return
		}
	}
	d.lines = append(d.lines, pyhttpx.HeaderLine{Name: name, Value: value})
}

// Removes the key the way an Omit value drops it from the merged mapping
func (d *sdkHeaderDict) omit(name string) {
	for i, l := range d.lines {
		if l.Name == name {
			d.lines = append(d.lines[:i], d.lines[i+1:]...)
			return
		}
	}
}

// Version of the pinned anthropic SDK whose wire identity the Messages clients present
const anthropicSDKVersion = "0.125.0"

// One key of a Python header dict, an Omit value holding its slot until the dict is stripped
type pyHeaderEntry struct {
	name, value string
	omit        bool
}

// Ordered str keyed header dict with Python dict key identity, Omit values kept in their slots
type pyHeaderDict struct{ entries []pyHeaderEntry }

// Dict of the lines in order, the later of two equal keys winning in the first slot
func pyHeaderDictOf(lines ...pyhttpx.HeaderLine) *pyHeaderDict {
	d := &pyHeaderDict{}
	for _, l := range lines {
		d.set(l.Name, l.Value)
	}
	return d
}

// Replaces the entry of the same key in place, else appends it
func (d *pyHeaderDict) put(e pyHeaderEntry) {
	for i := range d.entries {
		if d.entries[i].name == e.name {
			d.entries[i] = e
			return
		}
	}
	d.entries = append(d.entries, e)
}

func (d *pyHeaderDict) set(name, value string) { d.put(pyHeaderEntry{name: name, value: value}) }

// Marks the key Omit, the SDK removal marker dropped when the merged dict is stripped
func (d *pyHeaderDict) omit(name string) { d.put(pyHeaderEntry{name: name, omit: true}) }

// Merges the other dict over this one the way a dict unpacking merge does
func (d *pyHeaderDict) update(other *pyHeaderDict) {
	if other == nil {
		return
	}
	for _, e := range other.entries {
		d.put(e)
	}
}

// Value of the key when present and not Omit
func (d *pyHeaderDict) get(name string) (string, bool) {
	for _, e := range d.entries {
		if e.name == name {
			return e.value, !e.omit
		}
	}
	return "", false
}

// Whether the key is present as an Omit marker
func (d *pyHeaderDict) omitted(name string) bool {
	for _, e := range d.entries {
		if e.name == name {
			return e.omit
		}
	}
	return false
}

// Lines of the dict with every Omit entry dropped
func (d *pyHeaderDict) stripped() []pyhttpx.HeaderLine {
	out := make([]pyhttpx.HeaderLine, 0, len(d.entries))
	for _, e := range d.entries {
		if !e.omit {
			out = append(out, pyhttpx.HeaderLine{Name: e.name, Value: e.value})
		}
	}
	return out
}

// Header mapping as str pairs in declaration order, None values dropped, a string map in its recorded order
func orderedExtraHeaders(raw any) []pyhttpx.HeaderLine {
	var out []pyhttpx.HeaderLine
	switch m := raw.(type) {
	case *pyjson.Object:
		if m == nil {
			return nil
		}
		for _, k := range m.Keys {
			if v := m.Vals[k]; v != nil {
				out = append(out, pyhttpx.HeaderLine{Name: k, Value: cfgString(v)})
			}
		}
	case map[string]any:
		for _, k := range config.MappingKeys(m) {
			if v := m[k]; v != nil {
				out = append(out, pyhttpx.HeaderLine{Name: k, Value: cfgString(v)})
			}
		}
	case map[string]string:
		for _, k := range pyhttpx.HeaderOrder(m) {
			out = append(out, pyhttpx.HeaderLine{Name: k, Value: m[k]})
		}
	case []pyhttpx.HeaderLine:
		out = append(out, m...)
	}
	return out
}

// extra_headers of the first route matching custom provider entry declaring any, in declaration order
func customProviderExtraHeaderLines(baseURL string, customProviders []map[string]any, cfg map[string]any) []pyhttpx.HeaderLine {
	for _, entry := range entriesForRoute(baseURL, customProviders, cfg) {
		if lines := orderedExtraHeaders(entry["extra_headers"]); len(lines) > 0 {
			return lines
		}
	}
	return nil
}

// MergeHeaderMaps is the dict merge of header maps left to right, later values keeping earlier slots and new keys appended
func MergeHeaderMaps(maps ...map[string]string) map[string]string {
	return pyhttpx.MergeHeaderMaps(maps...)
}

// OrderedHeaders records the key order of a header map and returns the map
func OrderedHeaders(m map[string]string, keys []string) map[string]string {
	pyhttpx.SetHeaderOrder(m, keys)
	return m
}

// HeaderKeys are the keys of a header map in the order a Python dict of them iterates
func HeaderKeys(m map[string]string) []string { return pyhttpx.HeaderOrder(m) }

// AnyHeaderMap is a header mapping of arbitrary values as a string map in declaration order, values printed with fmt
func AnyHeaderMap(m map[string]any) map[string]string {
	var lines []pyhttpx.HeaderLine
	for _, k := range config.MappingKeys(m) {
		lines = append(lines, pyhttpx.HeaderLine{Name: k, Value: fmt.Sprint(m[k])})
	}
	return pyhttpx.OrderedHeaderMap(lines...)
}

// Header keys the Stainless merge_headers accumulates instead of replacing
const stainlessHelperHeader = "x-stainless-helper"

// Stainless merge_headers, later mappings winning except the helper key whose values accumulate deduplicated
func stainlessMergeHeaders(mappings ...*pyHeaderDict) *pyHeaderDict {
	merged := &pyHeaderDict{}
	for _, m := range mappings {
		if m == nil {
			continue
		}
		for _, e := range m.entries {
			if strings.ToLower(e.name) != stainlessHelperHeader {
				merged.put(e)
				continue
			}
			existing, present := merged.get(stainlessHelperHeader)
			if present && !e.omit {
				merged.set(stainlessHelperHeader, appendHelperValue(existing, e.value))
				continue
			}
			merged.put(pyHeaderEntry{name: stainlessHelperHeader, value: e.value, omit: e.omit})
		}
	}
	return merged
}

// Comma separated helper value with the new tokens appended once each
func appendHelperValue(existing, addition string) string {
	var tokens []string
	seen := map[string]bool{}
	for _, raw := range append(strings.Split(existing, ","), strings.Split(addition, ",")...) {
		if t := strings.TrimSpace(raw); t != "" && !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}
	return strings.Join(tokens, ", ")
}

// Lines of ANTHROPIC_CUSTOM_HEADERS, one name and value per line split at the first colon
func anthropicCustomHeadersEnv() (*pyHeaderDict, bool) {
	raw, ok := os.LookupEnv("ANTHROPIC_CUSTOM_HEADERS")
	if !ok {
		return nil, false
	}
	d := &pyHeaderDict{}
	for _, line := range strings.Split(raw, "\n") {
		if name, value, found := strings.Cut(line, ":"); found {
			d.set(pystr.Strip(name), pystr.Strip(value))
		}
	}
	return d, true
}

// Constructor arguments of one Anthropic SDK client that shape its request heads
type anthSDKClientSpec struct {
	// Client class, Anthropic or AnthropicBedrock
	class     string
	apiKey    *string
	authToken *string
	// default_headers in declaration order with their Omit entries
	custom *pyHeaderDict
	// str of the read phase of the client timeout, empty for a client without one
	readTimeout string
}

// Custom headers the client keeps, ANTHROPIC_CUSTOM_HEADERS lines first on the Anthropic class
func (s anthSDKClientSpec) customHeaders() *pyHeaderDict {
	if s.class != "Anthropic" {
		return s.custom
	}
	env, ok := anthropicCustomHeadersEnv()
	if !ok {
		return s.custom
	}
	env.update(s.custom)
	return env
}

// default_headers property of the client class, identity, platform, credentials and custom headers
func (s anthSDKClientSpec) defaultHeaders() *pyHeaderDict {
	custom := s.customHeaders()
	d := &pyHeaderDict{}
	d.set("Accept", "application/json")
	d.set("Content-Type", "application/json")
	d.set("User-Agent", s.class+"/Python "+anthropicSDKVersion)
	for _, l := range stainlessPlatformHeaders(anthropicSDKVersion) {
		d.set(l.Name, l.Value)
	}
	if s.class == "Anthropic" {
		if s.apiKey != nil {
			d.set("X-Api-Key", *s.apiKey)
		}
		if s.authToken != nil {
			d.set("Authorization", "Bearer "+*s.authToken)
		}
	}
	d.update(custom)
	if s.class == "Anthropic" {
		d.set("X-Stainless-Async", "false")
		d.set("anthropic-version", "2023-06-01")
		d.update(custom)
	}
	return d
}

// One SDK call, its per call headers in order and the str of its timeout option
type anthSDKCall struct {
	headers *pyHeaderDict
	// str of options.timeout as x-stainless-timeout carries it, NOT_GIVEN when the call passes none
	timeoutText string
	// str of the read phase of a call timeout, empty when the call passes none
	readTimeout string
	// Whether the call timeout was given as None
	timeoutNone bool
}

// Per call headers of messages.stream, the stream helper telemetry first then extra_headers
func anthStreamCallHeaders(extra *pyHeaderDict) *pyHeaderDict {
	d := &pyHeaderDict{}
	d.set("x-stainless-helper-method", "stream")
	d.set("x-stainless-stream-helper", "messages")
	d.update(extra)
	return d
}

// TypeError the Anthropic client raises when a request carries no credential and omits none
var errAnthNoAuthentication = errors.New(`"Could not resolve authentication method. Expected one of api_key, auth_token, or credentials to be set. Or for one of the ` + "`X-Api-Key` or `Authorization`" + ` headers to be explicitly omitted"`)

// Header lines of one attempt as _build_headers merges them and the SDK httpx client sends them
func (s anthSDKClientSpec) requestLines(call anthSDKCall, retries int) (*pyhttpx.Headers, error) {
	timeoutText := call.timeoutText
	if timeoutText == "" {
		timeoutText = "NOT_GIVEN"
	}
	base := &pyHeaderDict{}
	base.set("x-stainless-timeout", timeoutText)
	base.update(s.defaultHeaders())
	merged := stainlessMergeHeaders(base, call.headers)
	if s.class == "Anthropic" {
		key, _ := merged.get("X-Api-Key")
		auth, _ := merged.get("Authorization")
		if key == "" && auth == "" && !(call.headers != nil && (call.headers.omitted("X-Api-Key") || call.headers.omitted("Authorization"))) {
			return nil, &auxTypeError{Message: errAnthNoAuthentication.Error()}
		}
	}
	sdk := pyhttpx.NewHeaders(merged.stripped()...)
	custom := map[string]bool{}
	if call.headers != nil {
		for _, e := range call.headers.entries {
			custom[strings.ToLower(e.name)] = true
		}
	}
	if !custom["x-stainless-retry-count"] {
		sdk.Set("x-stainless-retry-count", strconv.Itoa(retries))
	}
	if !custom["x-stainless-read-timeout"] {
		switch {
		case call.timeoutNone:
		case call.readTimeout != "":
			sdk.Set("x-stainless-read-timeout", call.readTimeout)
		case s.readTimeout != "":
			sdk.Set("x-stainless-read-timeout", s.readTimeout)
		}
	}
	h := httpxClientHeaders()
	h.Update(sdk.Lines())
	return h, nil
}

// Lines an httpx request holds once built, Host first and the Content-Length of a body or body method last, as request hooks see them
func httpxPreparedLines(req *http.Request, h *pyhttpx.Headers, length int) *pyhttpx.Headers {
	lines := append([]pyhttpx.HeaderLine{{Name: "Host", Value: req.Host}}, h.Lines()...)
	switch {
	case length > 0, req.Method == http.MethodPost, req.Method == http.MethodPut, req.Method == http.MethodPatch:
		lines = append(lines, pyhttpx.HeaderLine{Name: "Content-Length", Value: strconv.Itoa(length)})
	}
	return pyhttpx.NewHeaders(lines...)
}

// Guardrail headers of the AnthropicBedrock client in the order bedrock_guardrail_headers declares them
func bedrockGuardrailHeaderDict(cfg map[string]any) *pyHeaderDict {
	headers := bedrockGuardrailHeaders(cfg)
	d := &pyHeaderDict{}
	for _, name := range []string{"X-Amzn-Bedrock-GuardrailIdentifier", "X-Amzn-Bedrock-GuardrailVersion", "X-Amzn-Bedrock-Trace"} {
		if v, ok := headers[name]; ok {
			d.set(name, v)
		}
	}
	return d
}

// Lines after the AnthropicBedrock request hook signs them with botocore SigV4Auth, every line but Connection re-added in botocore order
func botocoreSignedLines(req *http.Request, lines *pyhttpx.Headers, body []byte, creds awsCredentials, service, region string, now time.Time) (*pyhttpx.Headers, error) {
	if !creds.valid() {
		return nil, errors.New("could not resolve credentials from session")
	}
	signing := pyhttpx.NewHeaders(lines.Lines()...)
	signing.Del("connection")
	var aws []pyhttpx.HeaderLine
	for _, l := range httpxItems(signing) {
		aws = append(aws, l)
	}
	drop := func(name string) {
		kept := aws[:0]
		for _, l := range aws {
			if !strings.EqualFold(l.Name, name) {
				kept = append(kept, l)
			}
		}
		aws = kept
	}
	has := func(name string) bool {
		for _, l := range aws {
			if strings.EqualFold(l.Name, name) {
				return true
			}
		}
		return false
	}
	ts := now.UTC()
	drop("Authorization")
	if has("Date") {
		drop("Date")
		aws = append(aws, pyhttpx.HeaderLine{Name: "Date", Value: ts.Format(botocoreDateFormat)})
		drop("X-Amz-Date")
	} else {
		drop("X-Amz-Date")
		aws = append(aws, pyhttpx.HeaderLine{Name: "X-Amz-Date", Value: ts.Format(sigV4TimeFormat)})
	}
	if creds.SessionToken != "" {
		drop("X-Amz-Security-Token")
		aws = append(aws, pyhttpx.HeaderLine{Name: "X-Amz-Security-Token", Value: creds.SessionToken})
	}
	signed := &http.Request{Method: req.Method, URL: req.URL, Host: req.Host, Header: http.Header{}}
	for _, l := range aws {
		signed.Header[strings.ToLower(l.Name)] = append(signed.Header[strings.ToLower(l.Name)], l.Value)
	}
	res := sigV4Compute(signed, sigV4PayloadHash(signed, body), creds, service, region, ts, nil)
	aws = append(aws, pyhttpx.HeaderLine{Name: "Authorization", Value: res.Authorization})
	out := pyhttpx.NewHeaders(lines.Lines()...)
	out.Update(aws)
	return out, nil
}

// httpx Headers.items, lowercased names in first appearance order with repeated names joined by a comma and space
func httpxItems(h *pyhttpx.Headers) []pyhttpx.HeaderLine {
	var out []pyhttpx.HeaderLine
	at := map[string]int{}
	for _, l := range h.Lines() {
		name := strings.ToLower(l.Name)
		if i, ok := at[name]; ok {
			out[i].Value += ", " + l.Value
			continue
		}
		at[name] = len(out)
		out = append(out, pyhttpx.HeaderLine{Name: name, Value: l.Value})
	}
	return out
}

// Redirect policy of an httpx client built without follow_redirects, the redirect response handed back to the caller
func httpxNoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Host header httpx writes, the netloc with the default port of the scheme dropped
func httpxHost(u *url.URL) string {
	if port := u.Port(); (port == "443" && u.Scheme == "https") || (port == "80" && u.Scheme == "http") {
		host := u.Hostname()
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return u.Host
}

// Version of the pinned requests whose session heads the requests stack writes
const requestsVersion = "2.34.2"

// Content codings requests decodes through urllib3 on the pinned CPython, zstd through the stdlib codec
var requestsDecoders = []string{"gzip", "deflate", "zstd"}

// Lines a requests call writes, the session defaults updated case insensitively by the call headers, None values dropped
func requestsLines(call ...pyhttpx.HeaderLine) *pyhttpx.Headers {
	type entry struct{ name, value string }
	var merged []entry
	at := map[string]int{}
	set := func(name, value string) {
		if i, ok := at[strings.ToLower(name)]; ok {
			merged[i] = entry{name, value}
			return
		}
		at[strings.ToLower(name)] = len(merged)
		merged = append(merged, entry{name, value})
	}
	set("User-Agent", "python-requests/"+requestsVersion)
	set("Accept-Encoding", strings.Join(requestsDecoders, ", "))
	set("Accept", "*/*")
	set("Connection", "keep-alive")
	for _, l := range call {
		set(l.Name, l.Value)
	}
	out := pyhttpx.NewHeaders()
	for _, e := range merged {
		out.Set(e.name, e.value)
	}
	return out
}

// HTTP/1.1 wire transport of a urllib3 or http.client stack, the proxy the chooser names and the http.client CONNECT head
func pyStackWireTransport(choose func(*url.URL) (*url.URL, error), connect time.Duration, maxIdle int) *http.Transport {
	dial := (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	t := &http.Transport{MaxIdleConns: maxIdle, MaxIdleConnsPerHost: maxIdle, IdleConnTimeout: 90 * time.Second, DisableCompression: true}
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme != "http" {
			return nil, nil
		}
		return choose(req.URL)
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, err := pyhttpx.RequestTarget(ctx); err != nil {
			return nil, err
		}
		return pyhttpx.DialWire(ctx, dial, nil, network, address)
	}
	t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		proxy, err := choose(target)
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, connect)
		defer cancel()
		return pyhttpx.DialWireTLSStyle(cctx, dial, proxy, network, address, PlatformTLSConfig(), pyhttpx.TunnelHTTPClient)
	}
	return t
}

// Pool settings of an httpx client, its connection caps and keepalive expiry
type httpxLimits struct {
	maxConns, maxKeepalive int
	expiry                 time.Duration
}

var (
	// httpx.Limits the Stainless SDK clients pass, 1000 connections and 100 kept alive
	stainlessLimits = httpxLimits{maxConns: 1000, maxKeepalive: 100, expiry: 5 * time.Second}
	// Default httpx.Limits of a bare httpx client
	httpxDefaultLimits = httpxLimits{maxConns: 100, maxKeepalive: 20, expiry: 5 * time.Second}
)

// HTTP/1.1 transport of an httpx client writing wire heads over env proxy mounts, sockets passed through wrap when given
func httpxWireTransport(limits httpxLimits, connect time.Duration, wrap func(net.Conn) net.Conn) (*http.Transport, error) {
	return httpxWireTransportTLS(limits, connect, wrap, PlatformTLSConfig())
}

// HTTP/1.1 transport of an httpx client built with the given verify context
func httpxWireTransportTLS(limits httpxLimits, connect time.Duration, wrap func(net.Conn) net.Conn, cfg *tls.Config) (*http.Transport, error) {
	proxies, err := pyhttpx.NewEnvProxies(pyurl.Getproxies())
	if err != nil {
		return nil, err
	}
	base := (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	dial := base
	if wrap != nil {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := base(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return wrap(c), nil
		}
	}
	t := &http.Transport{MaxIdleConns: limits.maxKeepalive, MaxIdleConnsPerHost: limits.maxKeepalive, MaxConnsPerHost: limits.maxConns,
		IdleConnTimeout: limits.expiry, ExpectContinueTimeout: time.Second, DisableCompression: true}
	wireEnvDials(t, dial, cfg, connect, proxies)
	return t, nil
}

// Points a transport at wire capable HTTP/1.1 connections routed by the environment proxy mounts httpx trust_env reads
func wireEnvDials(t *http.Transport, dial pyhttpx.DialFunc, cfg *tls.Config, connect time.Duration, proxies *pyhttpx.EnvProxies) {
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme != "http" {
			return nil, nil
		}
		return pyhttpx.ForwardProxy(proxies.For(req.URL))
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		return pyhttpx.DialWire(ctx, dial, proxies.For(target), network, address)
	}
	t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if connect > 0 {
			cctx, cancel = context.WithTimeout(ctx, connect)
		}
		defer cancel()
		return pyhttpx.DialWireTLS(cctx, dial, proxies.For(target), network, address, cfg)
	}
}

// Versions of the pinned boto3 and botocore whose user agent the AWS clients carry
const (
	boto3Version    = "1.43.115"
	botocoreVersion = "1.43.115"
)

// Request path of a Bedrock model operation with the model id encoded as a botocore uri label, colons escaped
func botocoreModelPath(model, operation string) string {
	return "/model/" + sigV4Escape(model, false) + "/" + operation
}

// Retry context botocore keeps for one call, the invocation id, attempt, max attempts and the ttl of the last response
type botocoreRetries struct {
	invocation string
	attempt    int
	max        int
	ttl        string
}

// Records the ttl botocore derives from a response Date header and the read timeout
func (r *botocoreRetries) noteResponse(resp *http.Response, read time.Duration) {
	server, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return
	}
	r.ttl = server.UTC().Add(read).Format(sigV4TimeFormat)
}

// amz-sdk-request value, the ttl, attempt and max keys present in that order
func (r botocoreRetries) header() string {
	var parts []string
	if r.ttl != "" {
		parts = append(parts, "ttl="+r.ttl)
	}
	parts = append(parts, "attempt="+strconv.Itoa(r.attempt))
	if r.max > 0 {
		parts = append(parts, "max="+strconv.Itoa(r.max))
	}
	return strings.Join(parts, "; ")
}

// Feature id botocore registers for the credential provider that resolved, none for keys handed to the session
var botocoreCredentialFeatures = map[string]string{"env": "g", "shared-credentials-file": "n", "config-file": "n", "custom-process": "w",
	"iam-role": "0", "container-role": "z", "sso": "s", "assume-role": "i", "assume-role-with-web-identity": "k"}

// Characters botocore keeps in a user agent component, every other byte replaced with a dash
func botocoreUAComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!$%&'*+-.^_`|~,", r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// User-Agent of a boto3 client, sdk, platform, language, features, retry mode, app id and the boto3 extra
func (c *awsServiceClient) botocoreUserAgent(features []string) string {
	parts := []string{"Boto3/" + boto3Version, "md/Botocore#" + botocoreVersion, "ua/2.1"}
	osName := strings.ToLower(pyplatform.System())
	switch osName {
	case "windows", "linux", "macos", "android", "ios", "watchos", "tvos":
	case "darwin":
		osName = "macos"
	default:
		osName = ""
	}
	release := botocoreUAComponent(pyplatform.Release())
	if osName != "" {
		parts = append(parts, "os/"+osName+"#"+release)
	} else {
		parts = append(parts, "os/other", "md/"+botocoreUAComponent(pyplatform.System())+"#"+release)
	}
	if machine := strings.ToLower(pyplatform.Machine()); machine != "" {
		parts = append(parts, "md/arch#"+botocoreUAComponent(machine))
	}
	parts = append(parts, "lang/python#"+pinnedPythonRuntimeVersion(), "md/pyimpl#CPython")
	if env := os.Getenv("AWS_EXECUTION_ENV"); env != "" {
		parts = append(parts, "exec-env/"+botocoreUAComponent(env))
	}
	if len(features) > 0 {
		parts = append(parts, "m/"+strings.Join(features, ","))
	}
	parts = append(parts, "cfg/retry-mode#"+c.retry.mode)
	if app := c.session.setting("AWS_SDK_UA_APP_ID", "sdk_ua_app_id"); app != "" {
		parts = append(parts, "app/"+botocoreUAComponent(app))
	}
	return strings.Join(append(parts, "Botocore/"+botocoreVersion), " ")
}

// Feature ids of one request, the client config features then the credential provider feature
func (c *awsServiceClient) botocoreFeatures(source string, bearerFromEnv bool) []string {
	features := []string{map[string]string{"legacy": "D", "standard": "E", "adaptive": "F"}[c.retry.mode]}
	if c.session.configuredEndpoint(c.service) != "" {
		features = append(features, "N")
	}
	request, response := "Z", "b"
	if strings.EqualFold(c.session.setting("AWS_REQUEST_CHECKSUM_CALCULATION", "request_checksum_calculation"), "when_required") {
		request = "a"
	}
	if strings.EqualFold(c.session.setting("AWS_RESPONSE_CHECKSUM_VALIDATION", "response_checksum_validation"), "when_required") {
		response = "c"
	}
	features = append(features, request, response)
	switch {
	case bearerFromEnv:
		features = append(features, "3")
	case !c.resolvedCredentials():
	case c.session.opts.AccessKeyID != "" && c.session.opts.SecretAccessKey != "":
		features = append(features, "e")
	case botocoreCredentialFeatures[source] != "":
		features = append(features, botocoreCredentialFeatures[source])
	}
	sort.Strings(features)
	return features
}

// Head lines botocore and urllib3 write for one attempt, serializer headers, user agent, auth, retry headers and length
func (c *awsServiceClient) botocoreLines(ctx context.Context, req *http.Request, body []byte, call http.Header, r botocoreRetries) (*pyhttpx.Headers, error) {
	aws := pyhttpx.NewHeaders()
	for _, name := range sortedHeaderKeys(call) {
		for _, v := range call[name] {
			aws.Set(name, v)
		}
	}
	var creds awsCredentials
	if c.bearer == "" {
		var err error
		if creds, err = c.creds.Retrieve(ctx); err != nil {
			return nil, err
		}
	}
	aws.Set("User-Agent", c.botocoreUserAgent(c.botocoreFeatures(creds.Source, c.bearer != "" && c.bearerFromEnv)))
	if c.bearer != "" {
		aws.Set("Authorization", "Bearer "+c.bearer)
	} else {
		ts := awsNow().UTC()
		aws.Del("Authorization")
		aws.Del("X-Amz-Date")
		aws.Set("X-Amz-Date", ts.Format(sigV4TimeFormat))
		if creds.SessionToken != "" {
			aws.Del("X-Amz-Security-Token")
			aws.Set("X-Amz-Security-Token", creds.SessionToken)
		}
		signed := &http.Request{Method: req.Method, URL: req.URL, Host: req.Host, Header: http.Header{}}
		for _, l := range aws.Lines() {
			signed.Header[strings.ToLower(l.Name)] = append(signed.Header[strings.ToLower(l.Name)], l.Value)
		}
		res := sigV4Compute(signed, sigV4PayloadHash(signed, body), creds, c.signingName, c.region, ts, nil)
		aws.Set("Authorization", res.Authorization)
	}
	aws.Set("amz-sdk-invocation-id", r.invocation)
	aws.Set("amz-sdk-request", r.header())
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		aws.Set("Content-Length", strconv.Itoa(len(body)))
	}
	lines := []pyhttpx.HeaderLine{{Name: "Host", Value: req.Host}}
	if _, has := aws.Get("Accept-Encoding"); !has {
		lines = append(lines, pyhttpx.HeaderLine{Name: "Accept-Encoding", Value: "identity"})
	}
	return pyhttpx.NewHeaders(append(lines, aws.Lines()...)...), nil
}

// Default addheaders of a urllib opener, the Python-urllib user agent of the pinned CPython
func urllibAddHeaders() []pyhttpx.HeaderLine {
	version := pinnedPythonRuntimeVersion()
	if parts := strings.Split(version, "."); len(parts) >= 2 {
		version = parts[0] + "." + parts[1]
	}
	return []pyhttpx.HeaderLine{{Name: "User-agent", Value: "Python-urllib/" + version}}
}

// HTTP/1.1 transport of a urllib opener, one connection per request, ProxyHandler routing and http.client CONNECT tunnels
func urllibWireTransport(tlsCfg *tls.Config, connect time.Duration) *http.Transport {
	if tlsCfg == nil {
		tlsCfg = PlatformTLSConfig()
	}
	dial := (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext
	forward := func(target *url.URL) (*url.URL, error) {
		proxy := pyhttpx.URLLibProxy(target)
		if proxy == nil {
			return nil, nil
		}
		switch strings.ToLower(proxy.Scheme) {
		case "http", "https":
		default:
			return nil, &pyurl.URLError{Reason: "unknown url type: " + strings.ToLower(proxy.Scheme)}
		}
		bare := *proxy
		bare.User = nil
		return &bare, nil
	}
	t := &http.Transport{DisableKeepAlives: true, DisableCompression: true}
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme != "http" {
			return nil, nil
		}
		proxy, err := forward(req.URL)
		if err != nil || proxy == nil {
			return nil, err
		}
		return pyhttpx.ForwardProxy(proxy)
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		proxy, err := forward(target)
		if err != nil {
			return nil, err
		}
		return pyhttpx.DialWire(ctx, dial, proxy, network, address)
	}
	t.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target, err := pyhttpx.RequestTarget(ctx)
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, connect)
		defer cancel()
		return pyhttpx.DialWireTLSStyle(cctx, dial, pyhttpx.URLLibProxy(target), network, address, tlsCfg, pyhttpx.TunnelURLLib)
	}
	return t
}

// One urllib request, the headers argument then add_header calls in order, opened by urlopen or open_credentialed_url
type urllibCall struct {
	// Request method, GET without data and POST with it when empty
	method string
	url    string
	// Request body, nil for a request without data
	data    []byte
	headers []pyhttpx.HeaderLine
	// open_credentialed_url, addheaders moved onto the request and headers scoped to the first origin
	credentialed bool
	tls          *tls.Config
	timeout      time.Duration
	// Transport under the wire layer, the urllib wire transport when nil
	base http.RoundTripper
}

// Sends the call as urllib does hop by hop, each head written as do_request_, ProxyHandler, do_open and http.client build it
func urllibOpen(ctx context.Context, c urllibCall) (*http.Response, error) {
	cur := pyhttpx.NewURLLibRequest(c.headers...)
	var addheaders []pyhttpx.HeaderLine
	if c.credentialed {
		for _, l := range urllibAddHeaders() {
			if !cur.HasHeader(l.Name) {
				cur.AddHeader(l.Name, l.Value)
			}
		}
	} else {
		addheaders = urllibAddHeaders()
	}
	origin, err := URLOrigin(c.url)
	if err != nil {
		return nil, err
	}
	base := c.base
	if base == nil {
		base = urllibWireTransport(c.tls, c.timeout)
	}
	client := &http.Client{Transport: pyhttpx.WireTransport{Base: base},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: c.timeout}
	target := c.url
	method, data := c.method, c.data
	if method == "" {
		method = http.MethodGet
		if data != nil {
			method = http.MethodPost
		}
	}
	visited := map[string]int{}
	for {
		u, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		u.Fragment, u.RawFragment = "", ""
		cur.DoRequest(u.Host, data, data != nil, addheaders)
		if c.credentialed {
			hopOrigin, err := URLOrigin(u.String())
			if err != nil {
				return nil, err
			}
			if hopOrigin != origin {
				cur.StripHeaders(crossOriginSafeHeaders)
			}
		}
		proxy := pyhttpx.URLLibProxy(u)
		cur.AddProxyAuthorization(proxy)
		tunnel := proxy != nil && u.Scheme == "https"
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return nil, err
		}
		req.Close = true
		resp, err := client.Do(pyhttpx.WithWire(req, cur.WireLines(method, pyhttpx.URLLibPutHost(u), tunnel, data, data != nil)))
		if err != nil {
			return nil, err
		}
		next, ok := urllibRedirectTarget(resp, target, method)
		if !ok {
			return resp, nil
		}
		if visited[next] >= 4 || len(visited) >= 10 {
			return resp, nil
		}
		visited[next]++
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cur = cur.Redirected()
		if method != http.MethodHead {
			method = http.MethodGet
		}
		data = nil
		if c.credentialed {
			hopOrigin, err := URLOrigin(next)
			if err != nil {
				return nil, err
			}
			if hopOrigin != origin {
				cur.StripHeaders(crossOriginSafeHeaders)
			}
		}
		target = next
	}
}

// URL HTTPRedirectHandler moves the request to, false when the status and method make no redirect it follows
func urllibRedirectTarget(resp *http.Response, current, method string) (string, bool) {
	switch resp.StatusCode {
	case 301, 302, 303:
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost {
			return "", false
		}
	case 307, 308:
		if method != http.MethodGet && method != http.MethodHead {
			return "", false
		}
	default:
		return "", false
	}
	location, has := resp.Header["Location"]
	if !has {
		location, has = resp.Header["Uri"]
	}
	if !has || len(location) == 0 {
		return "", false
	}
	parts, err := pyurl.Parse(location[0])
	if err != nil {
		return "", false
	}
	switch parts.Scheme {
	case "http", "https", "ftp", "":
	default:
		return "", false
	}
	if parts.Path == "" && parts.Netloc != "" {
		parts.Path = "/"
	}
	next, err := pyurl.Join(current, pyurl.Quote(pyurl.Unparse(parts), "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"))
	if err != nil {
		return "", false
	}
	return strings.ReplaceAll(next, " ", "%20"), true
}
