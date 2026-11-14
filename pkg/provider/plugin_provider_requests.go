package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

const providerRequestDefaultTimeout = 30 * time.Second

// ProviderNotSignedInError is raised when this profile has no usable sign in for the provider
type ProviderNotSignedInError struct{ Message string }

func (e *ProviderNotSignedInError) Error() string { return e.Message }

func newProviderNotSignedIn(message string) error { return &ProviderNotSignedInError{Message: message} }

func isProviderNotSignedIn(err error) bool {
	var target *ProviderNotSignedInError
	return errors.As(err, &target)
}

// ProviderRequestPermissionError is the PermissionError of a refused caller, provider or origin
type ProviderRequestPermissionError struct{ Message string }

func (e *ProviderRequestPermissionError) Error() string { return e.Message }

func (e *ProviderRequestPermissionError) Unwrap() error { return fs.ErrPermission }

// UnsupportedProviderRequestError is the ValueError of a provider with no signed in request support
type UnsupportedProviderRequestError struct{ Message string }

func (e *UnsupportedProviderRequestError) Error() string { return e.Message }

// ProviderResponse is the status, lowercased headers and body of a signed in request, never its credential
type ProviderResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Body decoded as UTF-8 with each invalid subpart replaced
func (r ProviderResponse) Text() string { return loadDecodeReplace(r.Body) }

// Body parsed as JSON
func (r ProviderResponse) JSON() (any, error) {
	var v any
	err := json.Unmarshal(bytes.TrimPrefix(r.Body, []byte{0xEF, 0xBB, 0xBF}), &v)
	return v, err
}

// ProviderRequestOptions carries the JSON body, headers, timeout and calling code files of a signed in request
type ProviderRequestOptions struct {
	JSON    any
	Headers map[string]string
	// Zero is the 30 second default and a negative value waits without a timeout
	Timeout time.Duration
	// Source files on the calling stack innermost first, nil reads the Go call stack
	Caller []string
}

type pluginProviderSpec struct {
	origins []PluginOrigin
	resolve PluginOAuthResolver
}

// Core sign ins a plugin may request with, chatgpt.com the Codex backend and api.openai.com its Realtime secrets
var pluginProviderRequestProviders = map[string]pluginProviderSpec{
	"openai-codex": {origins: []PluginOrigin{{"https", "chatgpt.com", 443}, {"https", "api.openai.com", 443}}, resolve: pluginProviderCodexAuth},
}

// Codex auth headers and credential base URL from the locked refresh resolver, not signed in on a relogin error
func pluginProviderCodexAuth(rawURL string) (map[string]string, string, error) {
	env := pkceEnvFn()
	creds, err := resolveCodexRuntimeCredentials(env, CodexResolveOptions{})
	if err != nil {
		var authErr *AuthError
		if !errors.As(err, &authErr) || !authErr.ReloginRequired {
			return nil, "", err
		}
		creds = map[string]any{}
	}
	token := strings.TrimSpace(pyOrStr(creds["api_key"]))
	if token == "" {
		return nil, "", newProviderNotSignedIn(fmt.Sprintf("Not signed in to Codex in Icebath. Run `%s` or sign in to OpenAI Codex under Providers in the Icebath desktop app.",
			oauthReloginCommandIn(envHome(env), "openai-codex")))
	}
	headers := map[string]string{}
	if origin, err := pluginRequestURLOrigin(rawURL); err == nil && origin.Host == "chatgpt.com" {
		headers = codexAccountHeaders(token)
	}
	headers["Authorization"] = "Bearer " + token
	return headers, pyOrStr(creds["base_url"]), nil
}

func pluginProviderRequestProviderIDs() map[string]bool {
	ids := make(map[string]bool, len(pluginProviderRequestProviders))
	for id := range pluginProviderRequestProviders {
		ids[id] = true
	}
	return ids
}

// Scheme, lowercased host without a trailing dot and effective port, an error on a malformed port
func pluginRequestURLOrigin(rawURL string) (PluginOrigin, error) {
	parts, err := pyURLParse(rawURL, "")
	if err != nil {
		return PluginOrigin{}, err
	}
	host, _, _ := pyNetlocHostPort(parts.netloc)
	port, ok, err := pyURLPort(rawURL)
	if err != nil {
		return PluginOrigin{}, err
	}
	if !ok {
		port = map[string]int{"http": 80, "https": 443}[parts.scheme]
	}
	return PluginOrigin{Scheme: parts.scheme, Host: strings.TrimRight(host, "."), Port: port}, nil
}

func pluginOriginIn(origin PluginOrigin, origins []PluginOrigin) bool {
	for _, o := range origins {
		if o == origin {
			return true
		}
	}
	return false
}

// Every plugins directory an installed plugin can live under, each as given and resolved
func pluginProviderRequestRoots() []string {
	bundled := os.Getenv("ICEBATH_BUNDLED_PLUGINS")
	if bundled == "" {
		bundled = filepath.Join(pm.RepoRoot(), "plugins")
	}
	bases := []string{home.PluginsDir(envHome(pkceEnvFn())), home.PluginsDir(home.Process()), home.PluginsDir(home.Root()), bundled}
	seen := map[string]bool{}
	var roots []string
	for _, base := range bases {
		for _, r := range pluginRequestPathForms(base) {
			if !seen[r] {
				seen[r] = true
				roots = append(roots, r)
			}
		}
	}
	return roots
}

// Absolute and symlink resolved forms of a path, the absolute form standing in when resolution fails
func pluginRequestPathForms(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}
	return []string{abs, resolved}
}

// Reports a path strictly inside root
func pathUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Plugin directory of the innermost calling file under a plugins root, empty when no caller is plugin code
func callingPluginDir(callerFiles []string) string {
	roots := pluginProviderRequestRoots()
	for _, raw := range callerFiles {
		for _, path := range pluginRequestPathForms(raw) {
			root := ""
			for _, r := range roots {
				if pathUnder(path, r) {
					root = r
					break
				}
			}
			if root == "" {
				continue
			}
			for parent := filepath.Dir(path); parent != root && parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
				if isRegularFile(filepath.Join(parent, "plugin.yaml")) || isRegularFile(filepath.Join(parent, "plugin.yml")) {
					return parent
				}
			}
			rel, _ := filepath.Rel(root, path)
			return filepath.Join(root, strings.SplitN(rel, string(filepath.Separator), 2)[0])
		}
	}
	return ""
}

// Source files of the Go call stack above the caller of the request function, innermost first
func providerRequestStackFiles(skip int) []string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(skip, pcs)])
	var files []string
	for {
		frame, more := frames.Next()
		if frame.File != "" {
			files = append(files, frame.File)
		}
		if !more {
			return files
		}
	}
}

// DeclaredAuthProviders lists the requires_auth providers of a manifest whose sign in the plugin uses
func DeclaredAuthProviders(manifest map[string]any) []string {
	raw, ok := manifest["requires_auth"].([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, p := range raw {
		if s := strings.TrimSpace(pyStr(p)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RequiresAuthNotice is the install and show line naming the sign ins and oauth clients a plugin uses
func RequiresAuthNotice(manifest map[string]any) string {
	var parts []string
	if providers := DeclaredAuthProviders(manifest); len(providers) > 0 {
		parts = append(parts, fmt.Sprintf("Uses your sign-in for: %s (Icebath sends the requests; the plugin never receives the token)", strings.Join(providers, ", ")))
	}
	if notice := OAuthNotice(manifest); notice != "" {
		parts = append(parts, notice)
	}
	return strings.Join(parts, "; ")
}

func pluginDeclaredAuthProviders(pluginDir string) ([]string, error) {
	file, err := pm.NativeManifestFile(pluginDir)
	if err != nil || file == "" {
		return []string{}, err
	}
	manifest, err := pm.ReadNativeManifest(file)
	if err != nil {
		return nil, err
	}
	return DeclaredAuthProviders(manifest), nil
}

// Error rebuilt without its wrapped chain so no value holding a token or secret reaches plugin code
func providerRequestDetached(err error) error {
	switch e := err.(type) {
	case *AuthError:
		c := *e
		c.Err = nil
		return &c
	case *url.Error:
		return &url.Error{Op: e.Op, URL: e.URL, Err: errors.New(e.Err.Error())}
	case *ProviderNotSignedInError, *ProviderRequestPermissionError, *PluginOAuthPermissionError, *PluginOAuthManifestError, *UnsupportedProviderRequestError:
		return err
	case *fs.PathError:
		return &fs.PathError{Op: e.Op, Path: e.Path, Err: e.Err}
	}
	return errors.New(err.Error())
}

// JSON body the way httpx encodes it, compact with non ASCII text kept
func providerRequestJSONBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+5 < len(raw) && (string(raw[i+1:i+6]) == "u2028" || string(raw[i+1:i+6]) == "u2029") {
			out = utf8.AppendRune(out, map[string]rune{"u2028": ' ', "u2029": ' '}[string(raw[i+1:i+6])])
			i += 5
			continue
		}
		out = append(out, raw[i])
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
			out = append(out, raw[i])
		}
	}
	return out, nil
}

// Reader that rearms a timer before each read so a stalled phase cancels the request
type providerRequestIdleReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (p *providerRequestIdleReader) Read(b []byte) (int, error) {
	if p.timer != nil {
		p.timer.Reset(p.timeout)
	}
	return p.r.Read(b)
}

// Sends without following redirects under per phase connect, write, header and read timeouts
func providerRequestSend(req *http.Request, timeout time.Duration) (*ProviderResponse, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	var timer *time.Timer
	if timeout > 0 {
		transport.DialContext = (&net.Dialer{Timeout: timeout}).DialContext
		transport.TLSHandshakeTimeout = timeout
		transport.ResponseHeaderTimeout = timeout
		timer = time.AfterFunc(timeout, cancel)
		timer.Stop()
		defer timer.Stop()
	}
	defer transport.CloseIdleConnections()
	req = req.WithContext(ctx)
	if req.Body != nil {
		req.Body = io.NopCloser(&providerRequestIdleReader{r: req.Body, timer: timer, timeout: timeout})
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(&providerRequestIdleReader{r: resp.Body, timer: timer, timeout: timeout})
	if err != nil {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}
	headers := map[string]string{}
	for name, values := range resp.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return &ProviderResponse{Status: resp.StatusCode, Headers: headers, Body: body}, nil
}

// CredentialedProviderRequest sends method and URL with the calling plugin's provider sign in attached by Icebath
func CredentialedProviderRequest(provider, method, rawURL string, opts ProviderRequestOptions) (*ProviderResponse, error) {
	caller := opts.Caller
	if caller == nil {
		caller = providerRequestStackFiles(3)
	}
	pluginDir := callingPluginDir(caller)
	if pluginDir == "" {
		return nil, &ProviderRequestPermissionError{Message: "credentialed_provider_request must be called from an installed plugin's code"}
	}
	// The plugin's own oauth entry first so a core provider added later under the same id cannot take it over
	origins, resolve, ok, err := pluginOAuthSpec(pluginDir, provider)
	if err != nil {
		return nil, err
	}
	if !ok {
		spec, known := pluginProviderRequestProviders[provider]
		if !known {
			ids := make([]string, 0, len(pluginProviderRequestProviders))
			for id := range pluginProviderRequestProviders {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return nil, &UnsupportedProviderRequestError{Message: fmt.Sprintf("No signed-in requests for provider %s (supported: %s, or an id the plugin declares under oauth: in plugin.yaml)",
				pyRepr(provider), strings.Join(ids, ", "))}
		}
		declared, err := pluginDeclaredAuthProviders(pluginDir)
		if err != nil {
			return nil, err
		}
		if !containsString(declared, provider) {
			return nil, &ProviderRequestPermissionError{Message: fmt.Sprintf("Plugin '%s' must declare `requires_auth: [%s]` in plugin.yaml", filepath.Base(pluginDir), provider)}
		}
		origins, resolve = spec.origins, spec.resolve
	}
	origin, err := pluginRequestURLOrigin(rawURL)
	if err != nil {
		return nil, err
	}
	if !pluginOriginIn(origin, origins) {
		target := origin.Host
		if target == "" {
			target = pyRepr(rawURL)
		}
		return nil, &ProviderRequestPermissionError{Message: fmt.Sprintf("Refusing to send the %s sign-in to %s", provider, target)}
	}
	var body io.Reader
	if opts.JSON != nil {
		raw, err := providerRequestJSONBody(opts.JSON)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(strings.ToUpper(method), rawURL, body)
	if err != nil {
		return nil, providerRequestDetached(err)
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}
	if opts.JSON != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	authHeaders, credentialBase, err := resolve(rawURL)
	if err != nil {
		return nil, providerRequestDetached(err)
	}
	base, err := pluginRequestURLOrigin(credentialBase)
	if err != nil {
		return nil, err
	}
	if !pluginOriginIn(base, origins) {
		// A pooled gateway key belongs to its own host only
		return nil, &ProviderRequestPermissionError{Message: fmt.Sprintf("This profile's %s credential routes to a custom endpoint; refusing", provider)}
	}
	for name, value := range authHeaders {
		req.Header.Del(name)
		req.Header.Set(name, value)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = providerRequestDefaultTimeout
	}
	resp, err := providerRequestSend(req, timeout)
	if err != nil {
		return nil, providerRequestDetached(err)
	}
	return resp, nil
}
