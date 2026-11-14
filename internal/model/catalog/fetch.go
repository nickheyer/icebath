package catalog

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type statusError struct {
	status int
	body   []byte
}

func (e *statusError) Error() string {
	return "catalog: the endpoint answered HTTP " + strconv.Itoa(e.status)
}

var errNotJSONObject = errors.New("catalog: the endpoint did not answer with a JSON object")

var (
	_ providers.CatalogHooks = (*Catalog)(nil)
	_ providers.CatalogFacts = (*Catalog)(nil)
)

type state struct {
	mu               sync.Mutex
	tables           *providerTables
	tablesKey        string
	tablesGeneration uint64
	xai              []string

	deepInfra deepInfraState
	pricing   pricingState
	caps      capsState
	manifest  manifestState
	lists     listState
	disk      diskCacheState
	picker    pickerState
}

func newState() *state {
	return &state{
		deepInfra: deepInfraState{catalogs: map[string][]wire.Value{}, failures: map[string]time.Time{}},
		pricing:   pricingState{catalogs: map[string]*PriceTable{}, retryAfter: map[string]time.Time{}, providerKeys: map[string]string{}},
		manifest:  manifestState{retryAfter: map[backoffKey]time.Time{}},
		lists:     listState{recommended: map[string]recommendedMemo{}, probeFailures: map[string]time.Time{}},
		disk:      diskCacheState{refreshing: map[string]bool{}},
	}
}

func timeoutPhases(d time.Duration) phases { return phases{connect: d, read: d, total: d} }

func withoutBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}

func (c *Catalog) userAgent() string { return providers.Net{Version: c.version}.UserAgent() }

func (c *Catalog) net(ctx context.Context, tlsURL string, timeout time.Duration) (providers.Net, error) {
	client, err := c.client(ctx, tlsURL, timeoutPhases(timeout))
	if err != nil {
		return providers.Net{}, err
	}
	return providers.Net{Client: client, Version: c.version}, nil
}

func (c *Catalog) getJSON(ctx context.Context, rawURL string, header http.Header, tlsURL string, timeout time.Duration) (wire.Value, error) {
	resp, err := c.get(ctx, rawURL, header, tlsURL, timeoutPhases(timeout))
	if err != nil {
		return wire.Value{}, err
	}
	if !resp.success() {
		return wire.Value{}, &statusError{status: resp.status, body: resp.body}
	}
	return resp.value()
}

func (c *Catalog) getJSONObject(ctx context.Context, rawURL string, header http.Header, tlsURL string, timeout time.Duration) (wire.Value, error) {
	payload, err := c.getJSON(ctx, rawURL, header, tlsURL, timeout)
	if err != nil {
		return wire.Value{}, err
	}
	if !payload.IsObject() {
		return wire.Value{}, errNotJSONObject
	}
	return payload, nil
}

func (c *Catalog) env(ctx context.Context, name string) string {
	value, err := c.secrets.Get(ctx, name, "")
	if err != nil {
		slog.Debug("environment value is unreadable for a catalog lookup", "name", name, "error", err)
		return ""
	}
	return strings.TrimSpace(value)
}

func (c *Catalog) modelConfig(ctx context.Context) config.ModelConfig {
	cfg := c.config(ctx)
	if cfg == nil {
		return config.ModelConfig{}
	}
	return cfg.Model.ModelConfig
}

func (c *Catalog) background(ctx context.Context, work func(ctx context.Context)) bool {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.wg.Add(1)
	}
	c.mu.Unlock()
	if closed {
		return false
	}
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(c.bg, cancel)
	go func() {
		defer c.wg.Done()
		defer cancel()
		defer stop()
		work(detached)
	}()
	return true
}

func dataItems(payload wire.Value) ([]wire.Value, bool) {
	data := payload.Get("data")
	if !data.IsArray() {
		return nil, false
	}
	return data.Elems(), true
}

func objectItems(payload wire.Value) []wire.Value {
	items, _ := dataItems(payload)
	out := make([]wire.Value, 0, len(items))
	for _, item := range items {
		if item.IsObject() {
			out = append(out, item)
		}
	}
	return out
}

func parseFloat(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.ReplaceAll(trimmed, "_", ""), 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func bearer(header http.Header, key string) http.Header {
	if key != "" {
		header.Set("Authorization", "Bearer "+key)
	}
	return header
}
