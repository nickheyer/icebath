package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/pm"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	hostutil "github.com/nickheyer/icebath/pkg/utils/host"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Versions of the pinned openai SDK and httpx whose wire identity the model clients present
const (
	openAISDKVersion = "2.54.0"
	httpxVersion     = "0.28.1"
)

// Content codings the pinned httpx decodes on a default install, with neither brotli nor zstandard importable
var httpxDecoders = []string{"gzip", "deflate"}

// Default headers of an httpx.Client built with none of its own
func httpxClientHeaders() http.Header {
	return headerOf(
		headerField{Name: "Accept", Value: "*/*"},
		headerField{Name: "Accept-Encoding", Value: strings.Join(httpxDecoders, ", ")},
		headerField{Name: "Connection", Value: "keep-alive"},
		headerField{Name: "User-Agent", Value: "python-httpx/" + httpxVersion})
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
	if label := strings.ToLower(hostutil.Platform()); label != "" {
		return "Other:" + label
	}
	return "Unknown"
}

// Architecture label get_architecture of a Stainless SDK reports for this host
func stainlessArch() string {
	machine := strings.ToLower(hostutil.Machine())
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
func stainlessPlatformHeaders(version string) []headerField {
	return []headerField{
		{Name: "X-Stainless-Lang", Value: "python"},
		{Name: "X-Stainless-Package-Version", Value: version},
		{Name: "X-Stainless-OS", Value: stainlessOS()},
		{Name: "X-Stainless-Arch", Value: stainlessArch()},
		{Name: "X-Stainless-Runtime", Value: "CPython"},
		{Name: "X-Stainless-Runtime-Version", Value: pinnedPythonRuntimeVersion()},
	}
}

// Version of the pinned anthropic SDK whose wire identity the Messages clients present
const anthropicSDKVersion = "0.125.0"

// Request headers by case-insensitive name, a later write replacing the value and spelling of an earlier one, an omitted name left out of what is sent until it is set again
type sdkHeaders struct{ byName map[string]sdkHeader }

// One header of a set, its name as last written and its value, or the mark of a name the caller left out
type sdkHeader struct {
	name, value string
	omitted     bool
}

// Header set holding the fields, a later field replacing an earlier one of the same name
func newSDKHeaders(fields ...headerField) *sdkHeaders {
	d := &sdkHeaders{byName: make(map[string]sdkHeader, len(fields))}
	for _, f := range fields {
		d.set(f.Name, f.Value)
	}
	return d
}

// Stores the header under its case-insensitive key
func (d *sdkHeaders) put(h sdkHeader) {
	if d.byName == nil {
		d.byName = map[string]sdkHeader{}
	}
	d.byName[strings.ToLower(h.name)] = h
}

// Sets the value of a name
func (d *sdkHeaders) set(name, value string) { d.put(sdkHeader{name: name, value: value}) }

// Leaves the name out of what is sent, remembering that the caller omitted it
func (d *sdkHeaders) omit(name string) { d.put(sdkHeader{name: name, omitted: true}) }

// Sets a name to the text of a value, a nil value omitting the name
func (d *sdkHeaders) setValue(name string, v any) {
	if v == nil {
		d.omit(name)
		return
	}
	d.set(name, anyutil.String(v))
}

// Applies every header of the other set over this one, its omissions included
func (d *sdkHeaders) update(other *sdkHeaders) {
	if other == nil {
		return
	}
	for _, h := range other.byName {
		d.put(h)
	}
}

// Copy of the set, an empty set for nil
func (d *sdkHeaders) clone() *sdkHeaders {
	out := newSDKHeaders()
	out.update(d)
	return out
}

// Value of a name that is set and not omitted
func (d *sdkHeaders) get(name string) (string, bool) {
	if d == nil {
		return "", false
	}
	h, ok := d.byName[strings.ToLower(name)]
	return h.value, ok && !h.omitted
}

// Whether the caller omitted the name
func (d *sdkHeaders) isOmitted(name string) bool {
	return d != nil && d.byName[strings.ToLower(name)].omitted
}

// Whether the set names the header at all, set or omitted
func (d *sdkHeaders) has(name string) bool {
	if d == nil {
		return false
	}
	_, ok := d.byName[strings.ToLower(name)]
	return ok
}

// Fields the set sends, omitted names left out, sorted by name
func (d *sdkHeaders) fields() []headerField {
	if d == nil {
		return nil
	}
	out := make([]headerField, 0, len(d.byName))
	for _, key := range slices.Sorted(maps.Keys(d.byName)) {
		if h := d.byName[key]; !h.omitted {
			out = append(out, headerField{Name: h.name, Value: h.value})
		}
	}
	return out
}

// Fields of a header mapping in declaration order, null values dropped and other values as text, a string map in sorted order
func orderedExtraHeaders(raw any) []headerField {
	var out []headerField
	switch m := raw.(type) {
	case *jsonutil.Object:
		if m == nil {
			return nil
		}
		for _, k := range m.Keys() {
			if v := m.Value(k); v != nil {
				out = append(out, headerField{Name: k, Value: cfgString(v)})
			}
		}
	case map[string]any:
		for _, k := range config.MappingKeys(m) {
			if v := m[k]; v != nil {
				out = append(out, headerField{Name: k, Value: cfgString(v)})
			}
		}
	case map[string]string:
		for _, k := range slices.Sorted(maps.Keys(m)) {
			out = append(out, headerField{Name: k, Value: m[k]})
		}
	case []headerField:
		out = append(out, m...)
	}
	return out
}

// extra_headers of the first route matching custom provider entry declaring any, in declaration order
func customProviderExtraHeaderLines(baseURL string, customProviders []map[string]any) []headerField {
	for _, entry := range entriesForRoute(baseURL, customProviders) {
		if lines := orderedExtraHeaders(entry["extra_headers"]); len(lines) > 0 {
			return lines
		}
	}
	return nil
}

// MergeHeaderMaps is one map holding the fields of every header map, a later map winning over an earlier one
func MergeHeaderMaps(headers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, h := range headers {
		maps.Copy(out, h)
	}
	return out
}

// AnyHeaderMap is a header mapping of arbitrary values as a string map, null values dropped and other values as text
func AnyHeaderMap(m map[string]any) map[string]string {
	return fieldMap(orderedExtraHeaders(m)...)
}

// Header keys the Stainless merge_headers accumulates instead of replacing
const stainlessHelperHeader = "x-stainless-helper"

// Header sets merged in order, a later set winning except on the helper header whose values accumulate once each
func stainlessMergeHeaders(sets ...*sdkHeaders) *sdkHeaders {
	merged := newSDKHeaders()
	for _, s := range sets {
		if s == nil {
			continue
		}
		for key, h := range s.byName {
			if key == stainlessHelperHeader && !h.omitted {
				if existing, present := merged.get(stainlessHelperHeader); present {
					h.value = appendHelperValue(existing, h.value)
				}
			}
			merged.put(h)
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

// Headers of ANTHROPIC_CUSTOM_HEADERS, one name and value per line split at the first colon
func anthropicCustomHeadersEnv() (*sdkHeaders, bool) {
	raw, ok := os.LookupEnv("ANTHROPIC_CUSTOM_HEADERS")
	if !ok {
		return nil, false
	}
	d := newSDKHeaders()
	for _, line := range strings.Split(raw, "\n") {
		if name, value, found := strings.Cut(line, ":"); found {
			d.set(strings.TrimSpace(name), strings.TrimSpace(value))
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
	// Default headers the client is given, the names it omits included
	custom *sdkHeaders
	// Text of the read phase of the client timeout, empty for a client without one
	readTimeout string
}

// Custom headers the client keeps, ANTHROPIC_CUSTOM_HEADERS lines under the given ones on the Anthropic class
func (s anthSDKClientSpec) customHeaders() *sdkHeaders {
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

// Default headers of the client class, identity, platform, credentials and the custom headers over all of them
func (s anthSDKClientSpec) defaultHeaders() *sdkHeaders {
	d := newSDKHeaders(headerField{Name: "Accept", Value: "application/json"}, headerField{Name: "Content-Type", Value: "application/json"},
		headerField{Name: "User-Agent", Value: s.class + "/Python " + anthropicSDKVersion})
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
		d.set("X-Stainless-Async", "false")
		d.set("anthropic-version", "2023-06-01")
	}
	d.update(s.customHeaders())
	return d
}

// One SDK call, its per call headers and the text of its timeout option
type anthSDKCall struct {
	headers *sdkHeaders
	// Text of the call timeout as x-stainless-timeout carries it, NOT_GIVEN when the call passes none
	timeoutText string
	// Text of the read phase of a call timeout, empty when the call passes none
	readTimeout string
	// Whether the call explicitly passed no timeout
	timeoutNone bool
}

// Per call headers of messages.stream, the stream helper telemetry under the extra headers
func anthStreamCallHeaders(extra *sdkHeaders) *sdkHeaders {
	d := newSDKHeaders(headerField{Name: "x-stainless-helper-method", Value: "stream"}, headerField{Name: "x-stainless-stream-helper", Value: "messages"})
	d.update(extra)
	return d
}

// Refusal of a request that carries no credential and omits neither credential header
var errAnthNoAuthentication = errors.New(`"Could not resolve authentication method. Expected one of api_key, auth_token, or credentials to be set. Or for one of the ` + "`X-Api-Key` or `Authorization`" + ` headers to be explicitly omitted"`)

// Headers of one attempt, the client defaults under the call headers over the httpx client defaults, with the retry and read timeout telemetry the call does not set
func (s anthSDKClientSpec) requestLines(call anthSDKCall, retries int) (http.Header, error) {
	timeoutText := call.timeoutText
	if timeoutText == "" {
		timeoutText = "NOT_GIVEN"
	}
	base := newSDKHeaders(headerField{Name: "x-stainless-timeout", Value: timeoutText})
	base.update(s.defaultHeaders())
	merged := stainlessMergeHeaders(base, call.headers)
	if s.class == "Anthropic" {
		key, _ := merged.get("X-Api-Key")
		auth, _ := merged.get("Authorization")
		if key == "" && auth == "" && !call.headers.isOmitted("X-Api-Key") && !call.headers.isOmitted("Authorization") {
			return nil, &auxTypeError{Message: errAnthNoAuthentication.Error()}
		}
	}
	h := httpxClientHeaders()
	setFields(h, merged.fields())
	if !call.headers.has("x-stainless-retry-count") {
		h.Set("x-stainless-retry-count", strconv.Itoa(retries))
	}
	if !call.headers.has("x-stainless-read-timeout") {
		switch {
		case call.timeoutNone:
		case call.readTimeout != "":
			h.Set("x-stainless-read-timeout", call.readTimeout)
		case s.readTimeout != "":
			h.Set("x-stainless-read-timeout", s.readTimeout)
		}
	}
	return h, nil
}

// Guardrail headers of the AnthropicBedrock client
func bedrockGuardrailHeaderSet(ctx context.Context) *sdkHeaders {
	return newSDKHeaders(orderedExtraHeaders(bedrockGuardrailHeaders(ctx))...)
}

// Lines with the botocore SigV4 signature the AnthropicBedrock request hook adds, the Content-Length of the body signed with them
func botocoreSignedLines(req *http.Request, lines http.Header, body []byte, creds awsCredentials, service, region string, now time.Time) (http.Header, error) {
	if !creds.valid() {
		return nil, errors.New("could not resolve credentials from session")
	}
	out := lines.Clone()
	if len(body) > 0 || req.Method == http.MethodPost || req.Method == http.MethodPut || req.Method == http.MethodPatch {
		out.Set("Content-Length", strconv.Itoa(len(body)))
	}
	ts := now.UTC()
	out.Del("Authorization")
	if len(out.Values("Date")) > 0 {
		out.Set("Date", ts.Format(botocoreDateFormat))
		out.Del("X-Amz-Date")
	} else {
		out.Set("X-Amz-Date", ts.Format(sigV4TimeFormat))
	}
	if creds.SessionToken != "" {
		out.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	signing := out.Clone()
	signing.Del("Connection")
	signed := &http.Request{Method: req.Method, URL: req.URL, Host: req.Host, Header: signing}
	res := sigV4Compute(signed, sigV4PayloadHash(signed, body), creds, service, region, ts, nil)
	out.Set("Authorization", res.Authorization)
	return out, nil
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

// Headers a requests call sends, the session defaults with the call headers over them
func requestsLines(call ...headerField) http.Header {
	out := headerOf(headerField{Name: "User-Agent", Value: "python-requests/" + requestsVersion},
		headerField{Name: "Accept-Encoding", Value: strings.Join(requestsDecoders, ", ")},
		headerField{Name: "Accept", Value: "*/*"}, headerField{Name: "Connection", Value: "keep-alive"})
	setFields(out, call)
	return out
}

// HTTP/1.1 transport with a connect limit, the proxy the environment or the system names and the platform trust store
func stackWireTransport(connect time.Duration, maxIdle int) *http.Transport {
	return &http.Transport{
		Proxy:               httputil.Proxy,
		DialContext:         netpref.DialContext((&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext),
		TLSClientConfig:     PlatformTLSConfig(),
		TLSHandshakeTimeout: connect,
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
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

// HTTP/1.1 transport with the given pool limits and TLS settings, sockets passed through wrap when given
func httpxWireTransportTLS(limits httpxLimits, connect time.Duration, wrap func(net.Conn) net.Conn, cfg *tls.Config) (*http.Transport, error) {
	dial := netpref.DialContext((&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext)
	if wrap != nil {
		base := dial
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := base(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return wrap(c), nil
		}
	}
	return &http.Transport{
		Proxy:                 httputil.Proxy,
		DialContext:           dial,
		TLSClientConfig:       cfg,
		TLSHandshakeTimeout:   connect,
		MaxIdleConns:          limits.maxKeepalive,
		MaxIdleConnsPerHost:   limits.maxKeepalive,
		MaxConnsPerHost:       limits.maxConns,
		IdleConnTimeout:       limits.expiry,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}, nil
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
	osName := strings.ToLower(hostutil.OSName())
	switch osName {
	case "windows", "linux", "macos", "android", "ios", "watchos", "tvos":
	case "darwin":
		osName = "macos"
	default:
		osName = ""
	}
	release := botocoreUAComponent(hostutil.Release())
	if osName != "" {
		parts = append(parts, "os/"+osName+"#"+release)
	} else {
		parts = append(parts, "os/other", "md/"+botocoreUAComponent(hostutil.OSName())+"#"+release)
	}
	if machine := strings.ToLower(hostutil.Machine()); machine != "" {
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
func (c *awsServiceClient) botocoreLines(ctx context.Context, req *http.Request, body []byte, call http.Header, r botocoreRetries) (http.Header, error) {
	aws := headerOf()
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
		for _, l := range fieldsOf(aws) {
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
	if len(aws.Values("Accept-Encoding")) == 0 {
		aws.Set("Accept-Encoding", "identity")
	}
	return aws, nil
}

// Default addheaders of a urllib opener, the Python-urllib user agent of the pinned CPython
func urllibAddHeaders() []headerField {
	version := pinnedPythonRuntimeVersion()
	if parts := strings.Split(version, "."); len(parts) >= 2 {
		version = parts[0] + "." + parts[1]
	}
	return []headerField{{Name: "User-agent", Value: "Python-urllib/" + version}}
}

// HTTP/1.1 transport opening one connection per request through the proxy the environment or the system names
func urllibWireTransport(tlsCfg *tls.Config, connect time.Duration) *http.Transport {
	if tlsCfg == nil {
		tlsCfg = PlatformTLSConfig()
	}
	return &http.Transport{
		Proxy:               httputil.Proxy,
		DialContext:         netpref.DialContext((&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext),
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: connect,
		DisableKeepAlives:   true,
		DisableCompression:  true,
	}
}

// One urllib request, the headers argument then add_header calls in order, opened by urlopen or open_credentialed_url
type urllibCall struct {
	// Request method, GET without data and POST with it when empty
	method string
	url    string
	// Request body, nil for a request without data
	data    []byte
	headers http.Header
	// open_credentialed_url, addheaders moved onto the request and headers scoped to the first origin
	credentialed bool
	tls          *tls.Config
	timeout      time.Duration
	// Transport under the wire layer, the urllib wire transport when nil
	base http.RoundTripper
}

// Sends the call and follows its redirects hop by hop, body headers dropped after the first hop and, for a credentialed call, every header but the safe ones dropped once a hop leaves the first origin
func urllibOpen(ctx context.Context, c urllibCall) (*http.Response, error) {
	header := c.headers.Clone()
	if header == nil {
		header = http.Header{}
	}
	if header.Get("User-Agent") == "" {
		setFields(header, urllibAddHeaders())
	}
	origin, err := URLOrigin(c.url)
	if err != nil {
		return nil, err
	}
	base := c.base
	if base == nil {
		base = urllibWireTransport(c.tls, c.timeout)
	}
	client := &http.Client{Transport: httputil.DecodingTransport(base),
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
		if c.credentialed {
			hopOrigin, err := URLOrigin(u.String())
			if err != nil {
				return nil, err
			}
			if hopOrigin != origin {
				for name := range header {
					if !crossOriginSafeHeaders[strings.ToLower(name)] {
						header.Del(name)
					}
				}
			}
		}
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return nil, err
		}
		req.Close = true
		resp, err := client.Do(withHeader(req, header))
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
		header.Del("Content-Length")
		header.Del("Content-Type")
		if method != http.MethodHead {
			method = http.MethodGet
		}
		data = nil
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
	parts, err := urlutil.Split(location[0])
	if err != nil {
		return "", false
	}
	switch parts.Scheme {
	case "http", "https", "ftp", "":
	default:
		return "", false
	}
	if parts.Path == "" && parts.Authority != "" {
		parts.Path = "/"
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", false
	}
	next, err := base.Parse(urlutil.Escape(parts.String(), "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"))
	if err != nil {
		return "", false
	}
	return strings.ReplaceAll(next.String(), " ", "%20"), true
}
