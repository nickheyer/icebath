package connections

import (
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ParsedCookie is the name, value, path and expiry verdict of one Set-Cookie header
type ParsedCookie struct {
	Name    string
	Value   string
	Path    string
	Expired bool
}

type mirroredCookie struct {
	value string
	path  string
}

func normalizeCookiePath(raw string) string {
	path := strings.TrimSpace(raw)
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/"
}

// ParseSetCookie reads one Set-Cookie header, nil when it has no name
func ParseSetCookie(header string) *ParsedCookie {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	parts := strings.Split(header, ";")
	first := parts[0]
	eq := strings.Index(first, "=")
	if eq <= 0 {
		return nil
	}
	name := strings.TrimSpace(first[:eq])
	value := strings.TrimSpace(first[eq+1:])
	if name == "" {
		return nil
	}
	path := ""
	expired := value == ""
	for _, attribute := range parts[1:] {
		attrName, attrValue := attribute, ""
		if i := strings.Index(attribute, "="); i >= 0 {
			attrName, attrValue = attribute[:i], strings.TrimSpace(attribute[i+1:])
		}
		switch strings.ToLower(strings.TrimSpace(attrName)) {
		case "path":
			path = attrValue
		case "max-age":
			if seconds := authNumber(attrValue); !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds <= 0 {
				expired = true
			}
		case "expires":
			if at, ok := parseCookieDate(attrValue); ok && !at.After(time.Now()) {
				expired = true
			}
		}
	}
	return &ParsedCookie{Name: name, Value: value, Path: normalizeCookiePath(path), Expired: expired}
}

// Expires date of the HTTP date forms and ISO 8601, false when Date.parse would yield NaN
func parseCookieDate(value string) (time.Time, bool) {
	if at, err := http.ParseTime(value); err == nil {
		return at, true
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02", "Mon, 02 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST"} {
		if at, err := time.Parse(layout, value); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// OriginKeyFor is the scheme and host of an http or https URL, empty otherwise
func OriginKeyFor(rawURL string) string {
	parsed, err := parseURL(rawURL)
	if err != nil || parsed.Protocol() != "http:" && parsed.Protocol() != "https:" {
		return ""
	}
	return parsed.Protocol() + "//" + parsed.Host()
}

func requestPathFor(rawURL string) string {
	parsed, err := parseURL(rawURL)
	if err != nil || parsed.Pathname() == "" {
		return "/"
	}
	return parsed.Pathname()
}

// RFC 6265 path match of a cookie path against a request path
func cookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == requestPath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || requestPath[len(cookiePath)] == '/'
}

type mirroredJar struct {
	order  []string
	byName map[string]mirroredCookie
}

func (m *mirroredJar) set(name string, c mirroredCookie) {
	if _, ok := m.byName[name]; !ok {
		m.order = append(m.order, name)
	}
	m.byName[name] = c
}

func (m *mirroredJar) remove(name string) {
	if _, ok := m.byName[name]; !ok {
		return
	}
	delete(m.byName, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

// RemoteSessionCookieStore mirrors gateway session cookies per partition and origin for an explicit Cookie header
type RemoteSessionCookieStore struct {
	mu          sync.Mutex
	byPartition map[string]map[string]*mirroredJar
}

// NewRemoteSessionCookieStore is an empty mirror
func NewRemoteSessionCookieStore() *RemoteSessionCookieStore {
	return &RemoteSessionCookieStore{byPartition: map[string]map[string]*mirroredJar{}}
}

func (r *RemoteSessionCookieStore) jarFor(partition, origin string, create bool) *mirroredJar {
	origins := r.byPartition[partition]
	if origins == nil {
		if !create {
			return nil
		}
		origins = map[string]*mirroredJar{}
		r.byPartition[partition] = origins
	}
	jar := origins[origin]
	if jar == nil && create {
		jar = &mirroredJar{byName: map[string]mirroredCookie{}}
		origins[origin] = jar
	}
	return jar
}

// Record applies the Set-Cookie headers a response from rawURL carried
func (r *RemoteSessionCookieStore) Record(partition, rawURL string, setCookie []string) {
	origin := OriginKeyFor(rawURL)
	if partition == "" || origin == "" || len(setCookie) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, header := range setCookie {
		parsed := ParseSetCookie(header)
		if parsed == nil {
			continue
		}
		if parsed.Expired {
			if jar := r.jarFor(partition, origin, false); jar != nil {
				jar.remove(parsed.Name)
			}
			continue
		}
		r.jarFor(partition, origin, true).set(parsed.Name, mirroredCookie{value: parsed.Value, path: parsed.Path})
	}
}

// RecordFromJar copies the named valued cookies a partition jar holds for rawURL
func (r *RemoteSessionCookieStore) RecordFromJar(partition, rawURL string, cookies []JarCookie) {
	origin := OriginKeyFor(rawURL)
	if partition == "" || origin == "" || cookies == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range cookies {
		if c.Name != "" && c.Value != "" {
			r.jarFor(partition, origin, true).set(c.Name, mirroredCookie{value: c.Value, path: normalizeCookiePath(c.Path)})
		}
	}
}

// CookieHeaderFor is the explicit Cookie header for a request, empty when nothing is mirrored
func (r *RemoteSessionCookieStore) CookieHeaderFor(partition, rawURL string) string {
	origin := OriginKeyFor(rawURL)
	if partition == "" || origin == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	jar := r.jarFor(partition, origin, false)
	if jar == nil || len(jar.order) == 0 {
		return ""
	}
	requestPath := requestPathFor(rawURL)
	pairs := []string{}
	for _, name := range jar.order {
		if c := jar.byName[name]; cookiePathMatches(c.path, requestPath) {
			pairs = append(pairs, name+"="+c.value)
		}
	}
	return strings.Join(pairs, "; ")
}

// Clear forgets every partition, one partition, or one origin of a partition
func (r *RemoteSessionCookieStore) Clear(partition, originOrURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if partition == "" {
		r.byPartition = map[string]map[string]*mirroredJar{}
		return
	}
	if originOrURL == "" {
		delete(r.byPartition, partition)
		return
	}
	origin := OriginKeyFor(originOrURL)
	if origin == "" {
		origin = originOrURL
	}
	delete(r.byPartition[partition], origin)
}
