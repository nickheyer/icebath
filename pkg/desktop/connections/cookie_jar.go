package connections

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// JarCookie is one stored cookie of an OAuth partition
type JarCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain"`
	HostOnly bool      `json:"hostOnly"`
	Path     string    `json:"path"`
	Secure   bool      `json:"secure"`
	HTTPOnly bool      `json:"httpOnly"`
	SameSite string    `json:"sameSite"`
	Session  bool      `json:"session"`
	Expires  time.Time `json:"expirationDate"`
	Creation time.Time `json:"creation"`
}

func (c JarCookie) expired(now time.Time) bool { return !c.Session && !c.Expires.After(now) }

// PartitionJar is the persistent cookie store of one Electron session partition, sealed at rest
type PartitionJar struct {
	mu      sync.Mutex
	dir     string
	seal    func(string) (any, error)
	open    func(any) string
	loaded  bool
	cookies []JarCookie
}

type storedJar struct {
	Version int            `json:"version"`
	Cookies []storedCookie `json:"cookies"`
}

type storedCookie struct {
	JarCookie
	Sealed any `json:"sealed"`
}

func (j *PartitionJar) file() string { return filepath.Join(j.dir, "icebath-cookies.json") }

// Loads the jar once, importing what Electron's Chromium store held when no jar was written yet
func (j *PartitionJar) loadLocked() {
	if j.loaded {
		return
	}
	j.loaded = true
	raw, err := os.ReadFile(j.file())
	if err != nil {
		j.cookies = j.importChromium()
		if len(j.cookies) > 0 {
			j.saveLocked()
		}
		return
	}
	var stored storedJar
	if json.Unmarshal(raw, &stored) != nil {
		return
	}
	now := time.Now()
	for _, c := range stored.Cookies {
		cookie := c.JarCookie
		cookie.Value = j.open(c.Sealed)
		if cookie.Value == "" || cookie.expired(now) {
			continue
		}
		j.cookies = append(j.cookies, cookie)
	}
}

// Writes the persistent cookies owner only, session cookies staying in memory as Chromium keeps them
func (j *PartitionJar) saveLocked() {
	stored := storedJar{Version: 1, Cookies: []storedCookie{}}
	for _, c := range j.cookies {
		if c.Session {
			continue
		}
		sealed, err := j.seal(c.Value)
		if err != nil || sealed == nil {
			continue
		}
		entry := storedCookie{JarCookie: c, Sealed: sealed}
		entry.Value = ""
		stored.Cookies = append(stored.Cookies, entry)
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return
	}
	if os.MkdirAll(j.dir, 0o700) != nil {
		return
	}
	_ = hardening.WriteSecretFileAtomic(j.file(), data, hardening.SecretFileOptions{})
}

// Chromium microseconds since 1601 as a time
func chromiumTime(us int64) time.Time {
	return time.Unix(0, 0).Add(time.Duration(us-11644473600000000) * time.Microsecond)
}

// Cookies of the Chromium store Electron kept for this partition, values decrypted through safeStorage
func (j *PartitionJar) importChromium() []JarCookie {
	var path string
	for _, candidate := range []string{filepath.Join(j.dir, "Network", "Cookies"), filepath.Join(j.dir, "Cookies")} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			path = candidate
			break
		}
	}
	if path == "" {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return nil
	}
	defer db.Close()
	version := 0
	_ = db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'version'`).Scan(&version)
	rows, err := db.Query(`SELECT host_key, name, value, encrypted_value, path, expires_utc, is_secure, is_httponly, has_expires, is_persistent, samesite, creation_utc FROM cookies`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	now := time.Now()
	out := []JarCookie{}
	for rows.Next() {
		var host, name, value, cookiePath string
		var encrypted []byte
		var expires, creation, secure, httpOnly, hasExpires, persistent, sameSite int64
		if rows.Scan(&host, &name, &value, &encrypted, &cookiePath, &expires, &secure, &httpOnly, &hasExpires, &persistent, &sameSite, &creation) != nil {
			continue
		}
		if len(encrypted) > 0 {
			plain := j.open(SecretEnvelope{Encoding: safeStorageEncoding, Value: base64.StdEncoding.EncodeToString(encrypted)})
			if version >= 24 && len(plain) >= sha256.Size {
				sum := sha256.Sum256([]byte(host))
				if bytes.Equal([]byte(plain[:sha256.Size]), sum[:]) {
					plain = plain[sha256.Size:]
				}
			}
			value = plain
		}
		if value == "" {
			continue
		}
		c := JarCookie{Name: name, Value: value, Path: cookiePath, Secure: secure != 0, HTTPOnly: httpOnly != 0,
			SameSite: chromiumSameSite(sameSite), Session: persistent == 0 || hasExpires == 0, Creation: chromiumTime(creation)}
		if strings.HasPrefix(host, ".") {
			c.Domain = strings.TrimPrefix(host, ".")
		} else {
			c.Domain, c.HostOnly = host, true
		}
		if !c.Session {
			c.Expires = chromiumTime(expires)
		}
		if c.Session || c.expired(now) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func chromiumSameSite(v int64) string {
	switch v {
	case 0:
		return "no_restriction"
	case 1:
		return "lax"
	case 2:
		return "strict"
	}
	return "unspecified"
}

// Hosts Chromium treats as secure contexts for cookies over plain http
func trustworthyHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

type jarURL struct {
	scheme string
	host   string
	path   string
}

func parseJarURL(raw string) (jarURL, bool) {
	parsed, err := parseURL(raw)
	if err != nil {
		return jarURL{}, false
	}
	scheme := strings.TrimSuffix(parsed.Protocol(), ":")
	if scheme != "http" && scheme != "https" && scheme != "ws" && scheme != "wss" {
		return jarURL{}, false
	}
	path := parsed.Pathname()
	if path == "" {
		path = "/"
	}
	return jarURL{scheme: scheme, host: strings.ToLower(parsed.Hostname()), path: path}, true
}

func (u jarURL) secure() bool {
	return u.scheme == "https" || u.scheme == "wss" || trustworthyHost(u.host)
}

func domainMatches(host, domain string, hostOnly bool) bool {
	if hostOnly {
		return host == domain
	}
	return host == domain || strings.HasSuffix(host, "."+domain) && net.ParseIP(host) == nil
}

// RFC 6265 default path of a request path
func defaultCookiePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/"
	}
	i := strings.LastIndex(path, "/")
	if i == 0 {
		return "/"
	}
	return path[:i]
}

// Get is every live cookie a request to rawURL carries, longer paths first then older ones
func (j *PartitionJar) Get(rawURL string) []JarCookie {
	u, ok := parseJarURL(rawURL)
	if !ok {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	now := time.Now()
	out := []JarCookie{}
	for _, c := range j.cookies {
		if c.expired(now) || !domainMatches(u.host, c.Domain, c.HostOnly) || !cookiePathMatches(c.Path, u.path) || c.Secure && !u.secure() {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if len(out[a].Path) != len(out[b].Path) {
			return len(out[a].Path) > len(out[b].Path)
		}
		return out[a].Creation.Before(out[b].Creation)
	})
	return out
}

// GetDomain is every live cookie whose domain is the domain or one of its subdomains
func (j *PartitionJar) GetDomain(domain string) []JarCookie {
	domain = strings.TrimPrefix(strings.ToLower(domain), ".")
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	now := time.Now()
	out := []JarCookie{}
	for _, c := range j.cookies {
		if !c.expired(now) && (c.Domain == domain || strings.HasSuffix(c.Domain, "."+domain)) {
			out = append(out, c)
		}
	}
	return out
}

// All is every live cookie of the partition
func (j *PartitionJar) All() []JarCookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	now := time.Now()
	out := []JarCookie{}
	for _, c := range j.cookies {
		if !c.expired(now) {
			out = append(out, c)
		}
	}
	return out
}

// CookieHeader is the Cookie header value a request to rawURL carries, empty for none
func (j *PartitionJar) CookieHeader(rawURL string) string {
	pairs := []string{}
	for _, c := range j.Get(rawURL) {
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	return strings.Join(pairs, "; ")
}

func (j *PartitionJar) putLocked(c JarCookie, now time.Time) {
	for i, existing := range j.cookies {
		if existing.Name == c.Name && existing.Domain == c.Domain && existing.HostOnly == c.HostOnly && existing.Path == c.Path {
			c.Creation = existing.Creation
			j.cookies = append(j.cookies[:i], j.cookies[i+1:]...)
			break
		}
	}
	if c.expired(now) {
		return
	}
	if c.Creation.IsZero() {
		c.Creation = now
	}
	j.cookies = append(j.cookies, c)
}

// SetFromResponse stores the Set-Cookie headers a response from rawURL carried
func (j *PartitionJar) SetFromResponse(rawURL string, header http.Header) {
	u, ok := parseJarURL(rawURL)
	if !ok {
		return
	}
	cookies := (&http.Response{Header: header}).Cookies()
	if len(cookies) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	now := time.Now()
	changed := false
	for _, hc := range cookies {
		c, ok := jarCookieFromHTTP(hc, u, now)
		if !ok {
			continue
		}
		j.putLocked(c, now)
		changed = true
	}
	if changed {
		j.saveLocked()
	}
}

// RFC 6265 storage model of one Set-Cookie for a request URL, false when the cookie is refused
func jarCookieFromHTTP(hc *http.Cookie, u jarURL, now time.Time) (JarCookie, bool) {
	c := JarCookie{Name: hc.Name, Value: hc.Value, Secure: hc.Secure, HTTPOnly: hc.HttpOnly, Session: true}
	switch hc.SameSite {
	case http.SameSiteLaxMode:
		c.SameSite = "lax"
	case http.SameSiteStrictMode:
		c.SameSite = "strict"
	case http.SameSiteNoneMode:
		c.SameSite = "no_restriction"
	default:
		c.SameSite = "unspecified"
	}
	domain := strings.TrimPrefix(strings.ToLower(hc.Domain), ".")
	if domain == "" {
		c.Domain, c.HostOnly = u.host, true
	} else {
		if !domainMatches(u.host, domain, false) {
			return c, false
		}
		if suffix, _ := publicsuffix.PublicSuffix(domain); suffix == domain && domain != u.host {
			return c, false
		}
		c.Domain = domain
	}
	c.Path = hc.Path
	if !strings.HasPrefix(c.Path, "/") {
		c.Path = defaultCookiePath(u.path)
	}
	if c.Secure && !u.secure() {
		return c, false
	}
	if strings.HasPrefix(c.Name, "__Secure-") && !c.Secure {
		return c, false
	}
	if strings.HasPrefix(c.Name, "__Host-") && (!c.Secure || !c.HostOnly || c.Path != "/") {
		return c, false
	}
	switch {
	case hc.MaxAge < 0:
		c.Session, c.Expires = false, now.Add(-time.Second)
	case hc.MaxAge > 0:
		c.Session, c.Expires = false, now.Add(time.Duration(hc.MaxAge)*time.Second)
	case !hc.Expires.IsZero():
		c.Session, c.Expires = false, hc.Expires
	}
	return c, true
}

// ReplaceAll makes the jar hold exactly the cookies a sign-in window's storage holds
func (j *PartitionJar) ReplaceAll(cookies []JarCookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	now := time.Now()
	next := make([]JarCookie, 0, len(cookies))
	for _, c := range cookies {
		c.Domain = strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if c.Path == "" {
			c.Path = "/"
		}
		if c.Name == "" || c.expired(now) {
			continue
		}
		if c.Creation.IsZero() {
			c.Creation = now
		}
		next = append(next, c)
	}
	if jarCookiesEqual(j.cookies, next) {
		return
	}
	j.cookies = next
	j.saveLocked()
}

func jarCookiesEqual(a, b []JarCookie) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(c JarCookie) string {
		return c.Name + "\x00" + c.Domain + "\x00" + c.Path + "\x00" + c.Value + "\x00" + c.Expires.String()
	}
	seen := map[string]int{}
	for _, c := range a {
		seen[key(c)]++
	}
	for _, c := range b {
		if seen[key(c)] == 0 {
			return false
		}
		seen[key(c)]--
	}
	return true
}

// Remove deletes the cookies named name that a request to rawURL would carry
func (j *PartitionJar) Remove(rawURL, name string) {
	u, ok := parseJarURL(rawURL)
	if !ok {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.loadLocked()
	kept := j.cookies[:0]
	for _, c := range j.cookies {
		if c.Name == name && domainMatches(u.host, c.Domain, c.HostOnly) && cookiePathMatches(c.Path, u.path) {
			continue
		}
		kept = append(kept, c)
	}
	j.cookies = kept
	j.saveLocked()
}

// Partition jar of a session partition name, created on first use
func (s *Service) partitionJar(partition string) *PartitionJar {
	st := s.auth.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	if jar := st.jars[partition]; jar != nil {
		return jar
	}
	jar := &PartitionJar{
		dir: filepath.Join(s.userData, "Partitions", strings.TrimPrefix(partition, "persist:")),
		seal: func(value string) (any, error) {
			return EncryptDesktopSecretStrict(value, s.safe, true)
		},
		open: func(secret any) string {
			encoding, raw, ok := secretFields(toJSONValue(secret))
			value, _ := raw.(string)
			if !ok || value == "" {
				return ""
			}
			if encoding != safeStorageEncoding {
				return value
			}
			plain, err := s.safe.DecryptString(nodeBase64Decode(value))
			if err != nil {
				return ""
			}
			return plain
		},
	}
	st.jars[partition] = jar
	return jar
}
