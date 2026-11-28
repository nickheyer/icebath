package hooks

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	shellconfig "github.com/nickheyer/icebath/internal/plugins/defaults/hooks/shellhooks/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Seconds a post may take when its target gives none, and the most it may take
	DefaultPostTimeout = 10
	MaxPostTimeout     = 60
	// Times a post is tried, and the wait before the next try, longer each time
	attempts = 2
	backoff  = time.Second
	// Posts that may wait to be delivered, the next dropped once they all wait
	queueSize = 256
	// Longest icebath waits for the posts still waiting as it exits
	flushWait = 5 * time.Second
)

// One target of the outbound setting: where events are posted, which events, and how
type Target struct {
	// Its key among the targets of the setting
	Key    string
	URL    string
	Events []string
	Name   string
	// Key the posts are signed with, empty for unsigned posts
	Secret string
	// Variable named to hold the secret, and whether that variable is not set, which leaves the posts unsigned
	SecretEnv string
	Missing   bool
	Matcher   Matcher
	Timeout   int
}

// Name of the target, its address when it has none
func (t *Target) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.URL
}

// Targets of the profile's outbound setting, read again only when the setting changed
func (r *Registrant) Targets(cfg *config.Config) []*Target {
	posted, _ := encode(cfg.Get(shellconfig.Outbound), false)
	r.mu.Lock()
	p := r.state(cfg.Profile())
	if p.listed && p.posted == posted {
		targets := p.targets
		r.mu.Unlock()
		return targets
	}
	r.mu.Unlock()
	targets := r.parseTargets(cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	p.posted, p.targets, p.listed = posted, targets, true
	return targets
}

// Target of the key, nil when the setting holds none that can be used
func (r *Registrant) Target(cfg *config.Config, key string) *Target {
	for _, target := range r.Targets(cfg) {
		if target.Key == key {
			return target
		}
	}
	return nil
}

// Targets of the outbound setting by key, each malformed one warned about and left out
func (r *Registrant) parseTargets(cfg *config.Config) []*Target {
	raw := cfg.Get(shellconfig.Outbound)
	if raw == nil {
		return nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		r.warn(cfg.Profile(), fmt.Sprintf("%s must be a mapping of webhook targets by name; got %s", shellconfig.Outbound, kindOf(raw)))
		return nil
	}
	var out []*Target
	for _, key := range slices.Sorted(maps.Keys(table)) {
		if t := r.target(cfg, key, table[key]); t != nil {
			out = append(out, t)
		}
	}
	return out
}

// One target of the table, nil with a warning when it is malformed
func (r *Registrant) target(cfg *config.Config, key string, raw any) *Target {
	warn := func(format string, args ...any) {
		r.warn(cfg.Profile(), shellconfig.Outbound+"."+key+fmt.Sprintf(format, args...))
	}
	entry, ok := raw.(map[string]any)
	if !ok {
		warn(" must be a mapping with 'url' and 'events' keys; got %s", kindOf(raw))
		return nil
	}
	url, _ := entry[shellconfig.URL].(string)
	if url = strings.TrimSpace(url); url == "" {
		warn(" is missing a non-empty 'url'")
		return nil
	}
	lower := strings.ToLower(url)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		warn(".url must be http(s); got %s — skipped", show(url))
		return nil
	}
	if strings.HasPrefix(lower, "http://") {
		warn(".url uses plain http:// — payloads (including tool inputs) travel unencrypted. Prefer https.")
	}
	valid := strings.Join(hooks.Points(), ", ")
	listed, _ := entry[shellconfig.Posted].([]any)
	if len(listed) == 0 {
		warn(" needs a non-empty 'events' list (valid: %s)", valid)
		return nil
	}
	var events []string
	for _, item := range listed {
		if name, ok := item.(string); ok {
			if _, known := hooks.PointOf(name); known {
				events = append(events, name)
				continue
			}
		}
		warn(": unknown event %s ignored (valid: %s)", show(item), valid)
	}
	if len(events) == 0 {
		warn(" has no valid events — skipped")
		return nil
	}
	matcher := ""
	if value, given := entry[shellconfig.Matcher]; given && value != nil {
		if text, ok := value.(string); ok {
			matcher = text
		} else {
			warn(".matcher must be a string regex; ignoring")
		}
	}
	if matcher != "" && !toolScoped(events) {
		warn(".matcher=%s will be ignored — matcher is only honored for pre_tool_call / post_tool_call.", show(matcher))
		matcher = ""
	}
	timeout := DefaultPostTimeout
	if value, given := entry[shellconfig.Timeout]; given {
		if n, whole := config.Whole(value); whole {
			timeout = n
		} else {
			warn(".timeout must be an int (got %s); using default %ds", show(value), DefaultPostTimeout)
		}
	}
	name, _ := entry[shellconfig.Name].(string)
	t := &Target{Key: key, URL: url, Events: events, Name: strings.TrimSpace(name), Matcher: r.matcher(cfg.Profile(), "outbound webhook", matcher),
		Timeout: max(1, min(timeout, MaxPostTimeout))}
	if variable, _ := entry[shellconfig.SecretEnv].(string); strings.TrimSpace(variable) != "" {
		t.SecretEnv = strings.TrimSpace(variable)
		if t.Secret = cfg.Getenv(t.SecretEnv); t.Secret == "" {
			t.Missing = true
			warn(".secret_env=%s is not set in the environment — deliveries will be UNSIGNED", show(t.SecretEnv))
		}
	} else if text, ok := entry[shellconfig.Secret].(string); ok {
		t.Secret = text
	}
	return t
}

// Whether one of the events is fired for each tool call, so a matcher of tools applies
func toolScoped(events []string) bool {
	for _, event := range events {
		if point, _ := hooks.PointOf(event); point.Tool {
			return true
		}
	}
	return false
}

// Callbacks of the profile's outbound webhooks, one for each event of each address, which queue a post and answer nothing
func (r *Registrant) posts(cfg *config.Config) []hooks.Hook {
	var out []hooks.Hook
	seen := map[[2]string]bool{}
	for _, target := range r.Targets(cfg) {
		for _, event := range target.Events {
			if seen[[2]string{event, target.URL}] {
				continue
			}
			seen[[2]string{event, target.URL}] = true
			out = append(out, hooks.Hook{Point: event, Name: fmt.Sprintf("outbound_webhook[%s:%s]", event, target.Label()), Owner: "shellhooks",
				Run: r.post(cfg, event, target)})
		}
	}
	return out
}

// Callback that queues a post of the event to the target, for the tools its matcher names
func (r *Registrant) post(cfg *config.Config, event string, target *Target) func(context.Context, hooks.Event) (*hooks.Result, error) {
	point, _ := hooks.PointOf(event)
	return func(ctx context.Context, fired hooks.Event) (*hooks.Result, error) {
		fields := hooks.Payload(fired)
		if point.Tool {
			tool, _ := fields["tool_name"].(string)
			if ok, err := target.Matcher.Matches(tool); err != nil || !ok {
				return nil, err
			}
		}
		id := strs.Random(16)
		text, err := postBody(ctx, cfg, event, fields, id)
		if err != nil {
			slog.Warn(fmt.Sprintf("outbound webhook payload serialization failed (event=%s target=%s)", event, target.Label()), "error", err)
			return nil, nil
		}
		r.outbound.put(delivery(event, target, []byte(text), id))
		return nil, nil
	}
}

// Body of a post of the event: its name, the profile, the fields of the event, then the id and the time of the delivery
func postBody(ctx context.Context, cfg *config.Config, event string, fields map[string]any, id string) (string, error) {
	body := ordered{{"hook_event_name", event}, {"profile", cfg.Profile()}}
	for _, f := range common(ctx, cfg, fields) {
		if f.key != "profile" {
			body = append(body, f)
		}
	}
	body = append(body, field{"delivery_id", id}, field{"timestamp", stamp(time.Now())})
	return encode(body, false)
}

// One post waiting to be delivered
type sending struct {
	url, label, event string
	body              []byte
	headers           map[string]string
	timeout           int
}

// Post of the body to the target, signed with its secret when it has one
func delivery(event string, target *Target, body []byte, id string) *sending {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": "Icebath-Agent-Outbound-Webhook", "X-Icebath-Event": event,
		"X-Icebath-Delivery": id}
	if target.Secret != "" {
		mac := hmac.New(sha256.New, []byte(target.Secret))
		mac.Write(body)
		headers["X-Icebath-Signature-256"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return &sending{url: target.URL, label: target.Label(), event: event, body: body, headers: headers, timeout: target.Timeout}
}

// Posts waiting and the one worker that delivers them in order
type queue struct {
	posts   chan *sending
	mu      sync.Mutex
	started bool
	pending int
	idle    *sync.Cond
	// Client that never follows a redirect, so a redirected post never turns into a get without its body
	client *http.Client
}

func newQueue() *queue {
	q := &queue{posts: make(chan *sending, queueSize), client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	q.idle = sync.NewCond(&q.mu)
	return q
}

// Queues the post, starting the worker the first time; a post the full queue has no room for is dropped with a warning
func (q *queue) put(s *sending) {
	q.mu.Lock()
	if !q.started {
		q.started = true
		go q.work()
	}
	q.pending++
	q.mu.Unlock()
	select {
	case q.posts <- s:
	default:
		q.done()
		slog.Warn(fmt.Sprintf("outbound webhook queue full (%d pending) — dropping %s event for %s", queueSize, s.event, s.label))
	}
}

func (q *queue) done() {
	q.mu.Lock()
	q.pending--
	q.mu.Unlock()
	q.idle.Broadcast()
}

func (q *queue) work() {
	for s := range q.posts {
		func() {
			defer q.done()
			defer func() {
				if p := recover(); p != nil {
					slog.Warn(fmt.Sprintf("outbound webhook delivery crashed (target=%s)", s.label), "error", p)
				}
			}()
			q.deliver(s)
		}()
	}
}

// Waits until every queued post was delivered or the time passed; whether they all were
func (q *queue) flush(wait time.Duration) bool {
	timer := time.AfterFunc(wait, q.idle.Broadcast)
	defer timer.Stop()
	deadline := time.Now().Add(wait)
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.pending > 0 && time.Now().Before(deadline) {
		q.idle.Wait()
	}
	return q.pending == 0
}

// Posts with a retry: a failed connection and a server error tried once more, a refusal and a redirect final
func (q *queue) deliver(s *sending) {
	last := ""
	for attempt := 1; attempt <= attempts; attempt++ {
		status, location, err := q.send(s)
		switch {
		case err != nil:
			last = err.Error()
			if last == "" {
				last = fmt.Sprintf("%T", err)
			}
		case status >= 200 && status < 300:
			slog.Debug(fmt.Sprintf("outbound webhook delivered: %s -> %s (HTTP %d)", s.event, s.label, status))
			return
		case status >= 300 && status < 400:
			last = fmt.Sprintf("HTTP %d", status)
			slog.Warn(fmt.Sprintf("outbound webhook target redirected (event=%s target=%s): %s -> %s — redirects are not followed; update the configured url",
				s.event, s.label, last, cmp.Or(location, "?")))
			return
		case status >= 400 && status < 500:
			last = fmt.Sprintf("HTTP %d", status)
			slog.Warn(fmt.Sprintf("outbound webhook rejected (event=%s target=%s): %s — not retrying", s.event, s.label, last))
			return
		default:
			last = fmt.Sprintf("HTTP %d", status)
		}
		if attempt < attempts {
			time.Sleep(backoff * time.Duration(attempt))
		}
	}
	slog.Warn(fmt.Sprintf("outbound webhook delivery failed after %d attempt(s) (event=%s target=%s): %s", attempts, s.event, s.label, last))
}

// One post: the status and redirect of the answer, or why it failed
func (q *queue) send(s *sending) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.timeout)*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(s.body))
	if err != nil {
		return 0, "", err
	}
	for name, value := range s.headers {
		request.Header.Set(name, value)
	}
	response, err := q.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, "", errors.New("timed out")
		}
		return 0, "", err
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	return response.StatusCode, response.Header.Get("Location"), nil
}
