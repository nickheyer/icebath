// Package skills is the skills hub: the registries skills are found in, the guard that scans a skill before it is installed or loaded,
// and the installs, updates and removals recorded in the lock of a profile's skills folder
package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/fsx"
)

type Hub struct {
	registry *skills.Registry

	mu sync.Mutex
	// Installs fetched and scanned that wait for the user's yes, by id
	staged map[string]*staged
	// Verdicts of the guard by the content and source they were given for, so an unchanged skill is not scanned again
	verdicts map[string]skills.Verdict
	// The index icebath ships, decoded on first use
	shipped *index
	loading sync.Mutex
}

// The hub, registered as the guard of the skills host: it brings no skills of its own
func Register(registry *skills.Registry) *Hub {
	h := &Hub{registry: registry, staged: map[string]*staged{}, verdicts: map[string]skills.Verdict{}}
	registry.Register(h)
	return h
}

func (h *Hub) Sources(*config.Config) []skills.Source {
	return nil
}

// How a line the hub says reads
type Tone int

const (
	Plain Tone = iota
	Dim
	Good
	Warn
	Bad
	Heading
	Accent
)

// One line the hub says as it works
type Line struct {
	Text string
	Tone Tone
}

// Where the lines of an operation go
type Say func(Line)

// One operation of the hub over a scope: its HTTP clients, its GitHub session and where its lines go
type run struct {
	hub   *Hub
	scope *skills.Scope
	cfg   *config.Config
	say   Say
	// Clients that check every address they connect to, one following redirects
	follow, direct *http.Client
	gh             *github
}

func (h *Hub) run(scope *skills.Scope, say Say) *run {
	if say == nil {
		say = func(Line) {}
	}
	r := &run{hub: h, scope: scope, cfg: scope.Config, say: say, follow: web.Client(scope.Config, 0, true), direct: web.Client(scope.Config, 0, false)}
	r.gh = newGitHub(r)
	return r
}

// Says a line, the format filled when there are arguments
func (r *run) line(tone Tone, format string, args ...any) {
	if len(args) > 0 {
		format = fmt.Sprintf(format, args...)
	}
	r.say(Line{Text: format, Tone: tone})
}

// Says an error line
func (r *run) fail(format string, args ...any) {
	r.line(Bad, "Error: "+format, args...)
}

// Folder of the hub's own state in the profile's skills folder
func (r *run) dir() string        { return filepath.Join(r.scope.Dir(), ".hub") }
func (r *run) quarantine() string { return filepath.Join(r.dir(), "quarantine") }
func (r *run) auditPath() string  { return filepath.Join(r.dir(), "audit.log") }
func (r *run) tapsPath() string   { return filepath.Join(r.dir(), "taps.json") }
func (r *run) cacheDir() string   { return filepath.Join(r.dir(), "index-cache") }
func (r *run) scanCache() string  { return filepath.Join(r.dir(), "scan-cache") }

// Makes the hub's folders and its empty state files where they are missing
func (r *run) ensure() error {
	for _, dir := range []string{r.dir(), r.quarantine(), r.cacheDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	for path, empty := range map[string]string{
		filepath.Join(r.dir(), "lock.json"): "{\"version\": 1, \"installed\": {}}\n",
		r.auditPath():                       "",
		r.tapsPath():                        "{\"taps\": []}\n",
	} {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(empty), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Adds a line to the hub's audit log; a failed write is only logged
func (r *run) audit(action, name, source, trust, verdict, extra string) {
	line := strings.Join([]string{time.Now().UTC().Format("2006-01-02T15:04:05Z"), action, name, source + ":" + trust, verdict}, " ")
	if extra != "" {
		line += " " + extra
	}
	err := os.MkdirAll(filepath.Dir(r.auditPath()), 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(r.auditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.WriteString(line + "\n")
			err = errors.Join(err, f.Close())
		}
	}
	if err != nil {
		slog.Debug("Could not write the skills hub audit log", "error", err)
	}
}

// Most bytes of an answer read
const maxBody = 128 << 20

// Seconds a registry gets to answer when its caller names none
const defaultTimeout = 20 * time.Second

// One GET of the hub
type request struct {
	query  url.Values
	header map[string]string
	// Longest it may take, defaultTimeout when zero
	timeout time.Duration
	follow  bool
	// Most bytes of the answer read, maxBody when zero
	limit int64
}

type response struct {
	status int
	header http.Header
	body   []byte
	// Whether the answer was longer than the request's limit, its body cut there
	over bool
}

// Whether the answer is a whole 200
func (r *response) ok() bool { return r != nil && r.status == http.StatusOK && !r.over }

// GET through a client that checks every address it connects to
func (r *run) get(ctx context.Context, raw string, q request) (*response, error) {
	if q.timeout == 0 {
		q.timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if len(q.query) > 0 {
		values := req.URL.Query()
		for name, list := range q.query {
			values[name] = list
		}
		req.URL.RawQuery = values.Encode()
	}
	req.Header.Set("User-Agent", provider.UserAgent("icebath"))
	for name, value := range q.header {
		req.Header.Set(name, value)
	}
	client := r.direct
	if q.follow {
		client = r.follow
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	limit := q.limit
	if limit == 0 {
		limit = maxBody
	}
	if resp.ContentLength > limit {
		return &response{status: resp.StatusCode, header: resp.Header, over: true}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return &response{status: resp.StatusCode, header: resp.Header, body: body[:limit], over: true}, nil
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// Answer of a GET read as JSON into v, false on any other status, a failure or an answer that is not that JSON
func (r *run) getJSON(ctx context.Context, raw string, q request, v any) bool {
	resp, err := r.get(ctx, raw, q)
	if err != nil {
		slog.Debug("Skills hub request failed", "url", raw, "error", err)
		return false
	}
	return resp.ok() && json.Unmarshal(resp.body, v) == nil
}

// Body of a GET answered with 200
func (r *run) getText(ctx context.Context, raw string, q request) (string, bool) {
	resp, err := r.get(ctx, raw, q)
	if err != nil {
		slog.Debug("Skills hub request failed", "url", raw, "error", err)
		return "", false
	}
	return string(resp.body), resp.ok()
}

// Statuses that redirect
var redirects = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// Redirects a guarded GET follows
const maxRedirects = 5

// Whether a URL may be fetched: a public address and no rule of the website blocklist
func (r *run) allowed(raw string) bool {
	if !web.SafeURL(r.cfg, raw) {
		slog.Warn("Blocked unsafe Skills Hub URL: " + raw)
		return false
	}
	if block := web.Blocked(r.cfg, raw); block != nil {
		slog.Info(fmt.Sprintf("Blocked Skills Hub fetch for %s by rule %s", block.Host, block.Rule))
		return false
	}
	return true
}

// GET of a URL anyone may have written, every redirect checked against the address rules and the website blocklist before it is followed;
// nil when blocked or failed
func (r *run) guarded(ctx context.Context, raw string, q request) *response {
	current := raw
	for range maxRedirects + 1 {
		if !r.allowed(current) {
			return nil
		}
		q.follow = false
		resp, err := r.get(ctx, current, q)
		if err != nil {
			slog.Debug("Skills Hub fetch failed", "url", current, "error", err)
			return nil
		}
		if !redirects[resp.status] {
			return resp
		}
		location := resp.header.Get("Location")
		base, err := url.Parse(current)
		if location == "" || err != nil {
			return nil
		}
		next, err := base.Parse(location)
		if err != nil {
			return nil
		}
		current, q.query = next.String(), nil
	}
	slog.Warn("Skills Hub fetch exceeded redirect limit for " + raw)
	return nil
}

// Body of a guarded GET answered with 200
func (r *run) fetch(ctx context.Context, raw string) ([]byte, bool) {
	resp := r.guarded(ctx, raw, request{})
	if !resp.ok() {
		return nil, false
	}
	return resp.body, true
}

// Seconds an entry of the index cache stays fresh
const cacheTTL = time.Hour

// Value the index cache holds under the key, read into v when written within the time given; false on a miss
func (r *run) cached(key string, ttl time.Duration, v any) bool {
	path := filepath.Join(r.cacheDir(), key+".json")
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > ttl {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}

// Keeps a value in the index cache, with the file that keeps search tools out of the hub's folder
func (r *run) keep(key string, v any) {
	data, err := json.Marshal(v)
	if err == nil {
		err = os.MkdirAll(r.cacheDir(), 0o755)
	}
	if err == nil {
		if ignore := filepath.Join(r.dir(), ".ignore"); !fsx.Exists(ignore) {
			os.WriteFile(ignore, []byte("# Exclude hub internals from search tools\n*\n"), 0o644)
		}
		err = fsx.Replace(filepath.Join(r.cacheDir(), key+".json"), data, 0o644)
	}
	if err != nil {
		slog.Debug("Could not write the skills hub cache", "key", key, "error", err)
	}
}
