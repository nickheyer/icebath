// Account usage windows, Portal credits state, plan limits and spend display for one profile home
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
)

type Translate func(key string, args map[string]string) string

type UnfilledError struct {
	Name string
}

func (e *UnfilledError) Error() string {
	return "usage display needs a " + e.Name + " and none was provided"
}

type Runtime struct {
	BaseURL string
	APIKey  auth.Secret
}

type Options struct {
	Home      string
	Config    *config.Store
	Auth      *auth.Store
	Portal    *portal.Account
	Registry  *providers.Registry
	Catalog   func() *catalog.Catalog
	HTTP      httpclient.Options
	Resolve   func(ctx context.Context, provider, baseURL string, apiKey auth.Secret) (Runtime, error)
	Translate Translate
}

type Tracker struct {
	home      string
	cfg       *config.Store
	auth      *auth.Store
	portal    *portal.Account
	registry  *providers.Registry
	catalogOf func() *catalog.Catalog
	resolve   func(ctx context.Context, provider, baseURL string, apiKey auth.Secret) (Runtime, error)
	translate Translate
	transport *httpclient.Transport
	client    *http.Client
	base      context.Context
	stop      context.CancelFunc
	wg        sync.WaitGroup

	versionWarned atomic.Bool

	mu            sync.Mutex
	closed        bool
	slots         map[slotKey]slot
	lastTry       map[slotKey]time.Time
	inflight      map[slotKey]bool
	shownBands    *recent[int]
	windowNotices *recent[windowMemory]
	planInflight  bool
	planLast      *planAttempt
}

var errClosed = errors.New("usage: tracker is closed")

func New(o Options) (*Tracker, error) {
	switch {
	case strings.TrimSpace(o.Home) == "":
		return nil, &UnfilledError{Name: "profile home"}
	case o.Config == nil:
		return nil, &UnfilledError{Name: "config store"}
	case o.Auth == nil:
		return nil, &UnfilledError{Name: "credential store"}
	case o.Portal == nil:
		return nil, &UnfilledError{Name: "portal account"}
	case o.Registry == nil:
		return nil, &UnfilledError{Name: "provider registry"}
	case o.Catalog == nil:
		return nil, &UnfilledError{Name: "catalog"}
	case o.Resolve == nil:
		return nil, &UnfilledError{Name: "runtime resolver"}
	case o.Translate == nil:
		return nil, &UnfilledError{Name: "translator"}
	}
	transport, err := httpclient.New(o.HTTP)
	if err != nil {
		return nil, fmt.Errorf("usage: http transport: %w", err)
	}
	base, stop := context.WithCancel(context.Background())
	return &Tracker{
		home: o.Home, cfg: o.Config, auth: o.Auth, portal: o.Portal, registry: o.Registry, catalogOf: o.Catalog,
		resolve: o.Resolve, translate: o.Translate, transport: transport, client: transport.Client(), base: base, stop: stop,
		slots: map[slotKey]slot{}, lastTry: map[slotKey]time.Time{}, inflight: map[slotKey]bool{},
		shownBands: newRecent[int](sessionMemoryMax), windowNotices: newRecent[windowMemory](sessionMemoryMax),
	}, nil
}

func (t *Tracker) Close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.stop()
	t.wg.Wait()
	t.transport.CloseIdleConnections()
}

func (t *Tracker) bind(ctx context.Context) context.Context { return home.WithOverride(ctx, t.home) }

func (t *Tracker) catalog() *catalog.Catalog { return t.catalogOf() }

func (t *Tracker) spawn(ctx context.Context, fn func(ctx context.Context)) bool {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return false
	}
	t.wg.Add(1)
	t.mu.Unlock()
	bg, cancel := context.WithCancel(context.WithoutCancel(t.bind(ctx)))
	release := context.AfterFunc(t.base, cancel)
	go func() {
		defer t.wg.Done()
		defer release()
		defer cancel()
		fn(bg)
	}()
	return true
}

const maxResponseBody = 16 << 20

func (t *Tracker) requestJSON(ctx context.Context, method, target string, h http.Header, body []byte, timeout time.Duration) (map[string]any, json.RawMessage, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, target, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: %s request for %s is not valid", method, hostOf(target))
	}
	req.Header = h.Clone()
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, nil, scrubURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, nil, &httpclient.StatusError{Code: resp.StatusCode, Status: resp.Status, Method: method, Host: req.URL.Host}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, nil, scrubURLError(err)
	}
	return decodeObject(data)
}

func decodeObject(data []byte) (map[string]any, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("usage: response is not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("usage: response has data after its JSON value")
	}
	obj, ok := asObject(value)
	if !ok {
		return nil, nil, errMalformed
	}
	if _, isMap := value.(map[string]any); isMap {
		return obj, json.RawMessage(trimmed), nil
	}
	return obj, json.RawMessage("{}"), nil
}

func hostOf(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "an unparsable address"
	}
	return parsed.Host
}

func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("usage: %s %s: %w", ue.Op, hostOf(ue.URL), ue.Err)
	}
	return err
}

var errMalformed = errors.New("usage: response has an unexpected shape")

func asObject(v any) (map[string]any, bool) {
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	if falsy(v) {
		return map[string]any{}, true
	}
	return nil, false
}

func objectAt(m map[string]any, key string) (map[string]any, error) {
	obj, ok := asObject(m[key])
	if !ok {
		return nil, errMalformed
	}
	return obj, nil
}

func falsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func toFloat(v any) (float64, error) {
	if f, ok := number(v); ok {
		return f, nil
	}
	if s, ok := v.(string); ok {
		return parseFloatText(s)
	}
	return 0, errMalformed
}

func display(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

type recent[V any] struct {
	limit int
	order []string
	vals  map[string]V
}

func newRecent[V any](limit int) *recent[V] {
	return &recent[V]{limit: limit, vals: map[string]V{}}
}

func (r *recent[V]) get(key string) (V, bool) {
	v, ok := r.vals[key]
	return v, ok
}

func (r *recent[V]) put(key string, v V) {
	r.del(key)
	r.order = append(r.order, key)
	r.vals[key] = v
	if len(r.order) > r.limit {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.vals, oldest)
	}
}

func (r *recent[V]) del(key string) {
	if _, ok := r.vals[key]; !ok {
		return
	}
	delete(r.vals, key)
	for i, k := range r.order {
		if k == key {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}
