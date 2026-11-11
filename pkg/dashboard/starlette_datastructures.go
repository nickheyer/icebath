package dashboard

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Case insensitive view over raw header pairs, the Starlette Headers and MutableHeaders
type Headers struct {
	list *[]Header
}

// Headers backed by the scope header list so writes land in the scope
func headersOfScope(s *Scope) Headers { return Headers{list: &s.Headers} }

// Headers built from a name to value mapping in its given order
func HeadersOf(pairs ...string) Headers {
	list := make([]Header, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		list = append(list, Header{mustLatin1(pystr.Lower(pairs[i])), mustLatin1(pairs[i+1])})
	}
	return Headers{list: &list}
}

// Headers over an existing raw list
func headersOfRaw(raw *[]Header) Headers { return Headers{list: raw} }

// Raw pairs in order
func (h Headers) Raw() []Header {
	if h.list == nil {
		return nil
	}
	return *h.list
}

// First value of a header as latin-1 text and whether it is present
func (h Headers) Lookup(key string) (string, bool) {
	k := strings.ToLower(key)
	for _, p := range h.Raw() {
		if latin1Decode(p[0]) == k {
			return latin1Decode(p[1]), true
		}
	}
	return "", false
}

// First value of a header, empty when absent
func (h Headers) Get(key string) string {
	v, _ := h.Lookup(key)
	return v
}

// Every value of a header in order
func (h Headers) GetList(key string) []string {
	k := strings.ToLower(key)
	var out []string
	for _, p := range h.Raw() {
		if latin1Decode(p[0]) == k {
			out = append(out, latin1Decode(p[1]))
		}
	}
	return out
}

// Names and values as latin-1 text in order
func (h Headers) Items() [][2]string {
	out := make([][2]string, 0, len(h.Raw()))
	for _, p := range h.Raw() {
		out = append(out, [2]string{latin1Decode(p[0]), latin1Decode(p[1])})
	}
	return out
}

// Whether the header is present
func (h Headers) Contains(key string) bool {
	_, ok := h.Lookup(key)
	return ok
}

// Replaces the first occurrence of a header and drops the rest, appending when absent
func (h Headers) Set(key, value string) {
	setKey, setValue := mustLatin1(pystr.Lower(key)), mustLatin1(value)
	var found []int
	for i, p := range *h.list {
		if p[0] == setKey {
			found = append(found, i)
		}
	}
	for i := len(found) - 1; i >= 1; i-- {
		*h.list = append((*h.list)[:found[i]], (*h.list)[found[i]+1:]...)
	}
	if len(found) > 0 {
		(*h.list)[found[0]] = Header{setKey, setValue}
		return
	}
	*h.list = append(*h.list, Header{setKey, setValue})
}

// Removes every occurrence of a header
func (h Headers) Del(key string) {
	delKey := mustLatin1(pystr.Lower(key))
	out := (*h.list)[:0]
	for _, p := range *h.list {
		if p[0] != delKey {
			out = append(out, p)
		}
	}
	*h.list = out
}

// Value of a present header, else appends the given value and returns it
func (h Headers) SetDefault(key, value string) string {
	setKey := mustLatin1(pystr.Lower(key))
	for _, p := range *h.list {
		if p[0] == setKey {
			return latin1Decode(p[1])
		}
	}
	*h.list = append(*h.list, Header{setKey, mustLatin1(value)})
	return value
}

// Sets each pair in order
func (h Headers) Update(pairs ...string) {
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
}

// Appends one more occurrence of a header
func (h Headers) Append(key, value string) {
	*h.list = append(*h.list, Header{mustLatin1(pystr.Lower(key)), mustLatin1(value)})
}

// Adds a value to the comma joined vary header
func (h Headers) AddVaryHeader(vary string) {
	h.Set("vary", strings.Join(append(h.GetList("vary"), vary), ", "))
}

// Ordered multi valued query parameters, the last value of a key winning a plain lookup
type QueryParams struct {
	items [][2]string
}

// Query parameters parsed from a raw query string with blank values kept
func ParseQueryParams(query string) QueryParams {
	return QueryParams{items: pyurl.ParseQSL(latin1Decode(query), true)}
}

// Last value of a key and whether it is present
func (q QueryParams) Lookup(key string) (string, bool) {
	val, ok := "", false
	for _, kv := range q.items {
		if kv[0] == key {
			val, ok = kv[1], true
		}
	}
	return val, ok
}

// Last value of a key, empty when absent
func (q QueryParams) Get(key string) string {
	v, _ := q.Lookup(key)
	return v
}

// Every value of a key in order
func (q QueryParams) GetList(key string) []string {
	var out []string
	for _, kv := range q.items {
		if kv[0] == key {
			out = append(out, kv[1])
		}
	}
	return out
}

// Pairs in order with repeats
func (q QueryParams) MultiItems() [][2]string { return q.items }

// Distinct keys in first seen order
func (q QueryParams) Keys() []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range q.items {
		if !seen[kv[0]] {
			seen[kv[0]] = true
			out = append(out, kv[0])
		}
	}
	return out
}

// Attribute bag of app.state and request.state
type State struct {
	mu   sync.RWMutex
	vals map[string]any
}

// State over a backing map, a fresh one when nil
func NewState(vals map[string]any) *State {
	if vals == nil {
		vals = map[string]any{}
	}
	return &State{vals: vals}
}

// Attribute value and whether it is set
func (s *State) Lookup(name string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.vals[name]
	return v, ok
}

// Attribute value, nil when unset
func (s *State) Get(name string) any {
	v, _ := s.Lookup(name)
	return v
}

// Sets an attribute
func (s *State) Set(name string, v any) {
	s.mu.Lock()
	s.vals[name] = v
	s.mu.Unlock()
}

// Removes an attribute
func (s *State) Del(name string) {
	s.mu.Lock()
	delete(s.vals, name)
	s.mu.Unlock()
}

// Attribute as a bool, false when unset or not a bool
func (s *State) Bool(name string) bool {
	b, _ := s.Get(name).(bool)
	return b
}

// Attribute as a string, empty when unset or not a string
func (s *State) String(name string) string {
	str, _ := s.Get(name).(string)
	return str
}

// Parsed request URL, the Starlette URL over urlsplit components
type URL struct {
	raw string
}

// URL of a raw string
func NewURL(raw string) URL { return URL{raw: raw} }

// URL a scope addresses from its scheme, host header or server address, path and query
func URLOfScope(s *Scope) URL {
	scheme := s.Scheme
	if scheme == "" {
		scheme = "http"
	}
	path := strings.NewReplacer("?", "%3F", "#", "%23", "\\", "%5C").Replace(s.Path)
	var hostHeader *string
	for _, h := range s.Headers {
		if h[0] == "host" {
			v := latin1Decode(h[1])
			hostHeader = &v
			break
		}
	}
	netloc, haveNetloc := "", false
	if host, port, ok := parseHostHeader(hostHeader); ok && hostPortValid(port) {
		netloc, haveNetloc = host, true
		if port != nil {
			netloc += ":" + *port
		}
	} else if s.Server != nil {
		host := s.Server.Host
		if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
			host = "[" + host + "]"
		}
		def := map[string]int{"http": 80, "https": 443, "ws": 80, "wss": 443}[scheme]
		netloc, haveNetloc = host, true
		if s.Server.Port != def {
			netloc = host + ":" + strconv.Itoa(s.Server.Port)
		}
	}
	query := s.QueryString
	if haveNetloc {
		return URL{raw: pyurl.Unsplit(pyurl.URL{Scheme: scheme, Netloc: netloc, Path: path, Query: query})}
	}
	if strings.HasPrefix(path, "//") {
		path = "/%2F" + path[2:]
	}
	if query != "" {
		return URL{raw: path + "?" + query}
	}
	return URL{raw: path}
}

// Host header shape that cannot change the URL path or authority
var hostHeaderRE = regexp.MustCompile(`(?i)^(?P<host>[a-z0-9._~%!$&'()*+,;=-]+|\[(?:(?P<ipv6>[a-f0-9]*:[a-f0-9.:]+)|(?-i:v)[a-f0-9]+\.[a-z0-9._~!$&'()*+,;=:-]+)\])(?::(?P<port>[0-9]+))?$`)

// Host and optional port of a valid Host header with IP literal brackets kept
func parseHostHeader(hostHeader *string) (string, *string, bool) {
	if hostHeader == nil {
		return "", nil, false
	}
	m := hostHeaderRE.FindStringSubmatch(*hostHeader)
	if m == nil {
		return "", nil, false
	}
	if ipv6 := m[hostHeaderRE.SubexpIndex("ipv6")]; ipv6 != "" {
		if a, err := netip.ParseAddr(ipv6); err != nil || !a.Is6() || a.Zone() != "" {
			return "", nil, false
		}
	}
	host := m[hostHeaderRE.SubexpIndex("host")]
	if pi := hostHeaderRE.SubexpIndex("port"); m[pi] != "" {
		p := m[pi]
		return host, &p, true
	}
	return host, nil, true
}

// Whether a parsed Host port fits in 16 bits
func hostPortValid(port *string) bool {
	if port == nil {
		return true
	}
	p := strings.TrimLeft(*port, "0")
	if len(p) > 5 {
		return false
	}
	if p == "" {
		return true
	}
	n, _ := strconv.Atoi(p)
	return n <= 65535
}

func (u URL) String() string { return u.raw }

func (u URL) parts() pyurl.URL {
	p, _ := pyurl.Split(u.raw)
	return p
}

// Scheme component
func (u URL) Scheme() string { return u.parts().Scheme }

// Netloc component
func (u URL) Netloc() string { return u.parts().Netloc }

// Path component
func (u URL) Path() string { return u.parts().Path }

// Query component
func (u URL) Query() string { return u.parts().Query }

// Lowered hostname, empty when the netloc has none
func (u URL) Hostname() string { return u.parts().Hostname() }

// Port and whether the netloc names one
func (u URL) Port() (int, bool) {
	p, ok, err := u.parts().Port()
	return p, ok && err == nil
}

// Whether the scheme is https or wss
func (u URL) IsSecure() bool { s := u.Scheme(); return s == "https" || s == "wss" }

// URL with the given scheme, netloc, path or query replaced, nil fields kept
func (u URL) Replace(scheme, netloc, path, query *string) URL {
	p := u.parts()
	if scheme != nil {
		p.Scheme = *scheme
	}
	if netloc != nil {
		p.Netloc = *netloc
	}
	if path != nil {
		p.Path = *path
	}
	if query != nil {
		p.Query = *query
	}
	return URL{raw: pyurl.Unsplit(p)}
}

// URL with each given query parameter set over the existing ones
func (u URL) IncludeQueryParams(pairs ...string) URL {
	items := pyurl.ParseQSL(u.Query(), true)
	for i := 0; i+1 < len(pairs); i += 2 {
		kept := items[:0]
		for _, kv := range items {
			if kv[0] != pairs[i] {
				kept = append(kept, kv)
			}
		}
		items = append(kept, [2]string{pairs[i], pairs[i+1]})
	}
	q := pyurl.URLEncode(items)
	return u.Replace(nil, nil, nil, &q)
}

// URL with only the given query parameters
func (u URL) ReplaceQueryParams(pairs ...string) URL {
	var items [][2]string
	for i := 0; i+1 < len(pairs); i += 2 {
		items = append(items, [2]string{pairs[i], pairs[i+1]})
	}
	q := pyurl.URLEncode(items)
	return u.Replace(nil, nil, nil, &q)
}

// URL without the given query parameters
func (u URL) RemoveQueryParams(keys ...string) URL {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	var items [][2]string
	for _, kv := range pyurl.ParseQSL(u.Query(), true) {
		if !drop[kv[0]] {
			items = append(items, kv)
		}
	}
	q := pyurl.URLEncode(items)
	return u.Replace(nil, nil, nil, &q)
}
