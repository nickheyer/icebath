package connections

import (
	"sort"
	"strings"
	"sync"
)

// RemoteHeaderSource is one gateway base URL with the extra headers its requests carry
type RemoteHeaderSource struct {
	Headers map[string]string
	Kind    string
	URL     string
}

// CollectRemoteHeaderSources keeps remote and cloud entries with headers plus the v1 remote, longest URL first
func CollectRemoteHeaderSources(connections []RemoteHeaderSource, v1Remote *RemoteHeaderSource) []RemoteHeaderSource {
	sources := []RemoteHeaderSource{}
	for _, connection := range connections {
		if connection.Kind != "" && connection.Kind != "remote" && connection.Kind != "cloud" {
			continue
		}
		if connection.URL == "" || len(connection.Headers) == 0 {
			continue
		}
		sources = append(sources, RemoteHeaderSource{Headers: connection.Headers, URL: connection.URL})
	}
	if v1Remote != nil && v1Remote.URL != "" && len(v1Remote.Headers) > 0 {
		sources = append(sources, RemoteHeaderSource{Headers: v1Remote.Headers, URL: v1Remote.URL})
	}
	sort.SliceStable(sources, func(i, j int) bool { return len(sources[i].URL) > len(sources[j].URL) })
	return sources
}

// ResolveRemoteRequestHeaders is the exact socket headers when remembered, else the first matching source
func ResolveRemoteRequestHeaders(requestURL string, exact map[string]string, sources []RemoteHeaderSource) map[string]string {
	if len(exact) > 0 {
		return exact
	}
	for _, source := range sources {
		if source.URL == "" || source.Headers == nil {
			continue
		}
		if len(source.Headers) > 0 && RemoteRequestMatchesBaseURL(requestURL, source.URL) {
			return source.Headers
		}
	}
	return map[string]string{}
}

// RemoteRequestDetails is one outgoing webview request a header rule sees
type RemoteRequestDetails struct {
	ID             *int
	URL            string
	RequestHeaders map[string]string
}

// RemoteHeaderInjector applies remote headers to webview requests and strips them from redirect hops leaving the gateway
type RemoteHeaderInjector struct {
	mu         sync.Mutex
	headersFor func(string) map[string]string
	injected   map[int]map[string]bool
}

// NewRemoteHeaderInjector tracks injected header names per request over a header rule
func NewRemoteHeaderInjector(headersFor func(string) map[string]string) *RemoteHeaderInjector {
	return &RemoteHeaderInjector{headersFor: headersFor, injected: map[int]map[string]bool{}}
}

// Apply is the outgoing header set of a request, nil when the request stays as it is
func (r *RemoteHeaderInjector) Apply(details RemoteRequestDetails) map[string]string {
	headers := r.headersFor(details.URL)
	r.mu.Lock()
	defer r.mu.Unlock()
	var tracked map[string]bool
	if details.ID != nil {
		tracked = r.injected[*details.ID]
	}
	if len(headers) == 0 && tracked == nil {
		return nil
	}
	strip := map[string]bool{}
	for name := range tracked {
		strip[name] = true
	}
	for name := range headers {
		strip[strings.ToLower(name)] = true
	}
	out := map[string]string{}
	for name, value := range details.RequestHeaders {
		if !strip[strings.ToLower(name)] {
			out[name] = value
		}
	}
	if details.ID != nil {
		if len(headers) > 0 {
			names := map[string]bool{}
			for name := range headers {
				names[strings.ToLower(name)] = true
			}
			r.injected[*details.ID] = names
		} else {
			delete(r.injected, *details.ID)
		}
	}
	for name, value := range headers {
		out[name] = value
	}
	return out
}

// Forget drops the bookkeeping of a completed or failed request
func (r *RemoteHeaderInjector) Forget(id *int) {
	if id == nil {
		return
	}
	r.mu.Lock()
	delete(r.injected, *id)
	r.mu.Unlock()
}

// remoteWsHeaderStore remembers the headers of recent socket URLs, least recently used dropped past the limit
type remoteWsHeaderStore struct {
	mu    sync.Mutex
	limit int
	order []string
	byURL map[string]map[string]string
}

func newRemoteWsHeaderStore(limit int) *remoteWsHeaderStore {
	return &remoteWsHeaderStore{limit: limit, byURL: map[string]map[string]string{}}
}

func (r *remoteWsHeaderStore) touchLocked(key string) {
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.order = append(r.order, key)
}

func (r *remoteWsHeaderStore) remember(wsURL string, headers map[string]string) {
	if wsURL == "" || len(headers) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byURL[wsURL] = headers
	r.touchLocked(wsURL)
	for len(r.byURL) > r.limit && len(r.order) > 0 {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.byURL, oldest)
	}
}

func (r *remoteWsHeaderStore) headersFor(requestURL string) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	headers, ok := r.byURL[requestURL]
	if !ok {
		return map[string]string{}
	}
	r.touchLocked(requestURL)
	return headers
}

// Plaintext remote headers of a stored header block, values decrypted and stripped of CR and LF
func (s *Service) decryptRemoteHeaders(headers any) map[string]string {
	out := map[string]string{}
	for name, secret := range NormalizeRemoteHeaders(toJSONValue(headers)) {
		s.mu.Lock()
		plain := s.decryptDesktopSecret(secret)
		s.mu.Unlock()
		if value := SanitizeRemoteHeaderValue(plain); value != "" {
			out[name] = value
		}
	}
	return out
}

// Remembers the headers a renderer socket to wsURL must carry
func (s *Service) rememberRemoteWsHeaders(wsURL string, headers map[string]string) {
	s.auth.state().wsHeaders.remember(wsURL, headers)
}

// Decrypted header sources of the registry and the v1 remote, rebuilt only when either store changes
func (s *Service) remoteHeaderSources() []RemoteHeaderSource {
	st := s.auth.state()
	s.mu.Lock()
	config := s.readDesktopConnectionConfig()
	registry, _ := s.readDesktopConnectionsRegistry()
	configKey, registryKey := s.config, s.registry
	s.mu.Unlock()
	st.mu.Lock()
	if st.headerSources != nil && st.headerConfigKey == configKey && st.headerRegistryKey == registryKey {
		sources := st.headerSources
		st.mu.Unlock()
		return sources
	}
	st.mu.Unlock()
	entries := make([]RemoteHeaderSource, 0, len(registry.Connections))
	for _, entry := range registry.Connections {
		entries = append(entries, RemoteHeaderSource{Kind: entry.Kind, URL: entry.URL, Headers: s.decryptRemoteHeaders(entry.Headers)})
	}
	var v1 *RemoteHeaderSource
	remote := jsObject(config.Remote)
	if ModeIsRemoteLike(config.Mode) && jsTruthy(remote["url"]) {
		v1 = &RemoteHeaderSource{URL: jsString(remote["url"]), Headers: s.decryptRemoteHeaders(remote["headers"])}
	}
	sources := CollectRemoteHeaderSources(entries, v1)
	st.mu.Lock()
	st.headerSources, st.headerConfigKey, st.headerRegistryKey = sources, configKey, registryKey
	st.mu.Unlock()
	return sources
}

// Extra headers a request to requestURL carries, remembered socket headers first
func (s *Service) headersForRemoteRequest(requestURL string) map[string]string {
	return ResolveRemoteRequestHeaders(requestURL, s.auth.state().wsHeaders.headersFor(requestURL), s.remoteHeaderSources())
}

// RemoteRequestHeaders is the header rule the shell applies to webview requests and renderer sockets
func (s *Service) RemoteRequestHeaders(requestURL string) map[string]string {
	return s.headersForRemoteRequest(requestURL)
}
