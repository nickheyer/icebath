package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const hostedWebSearchBase = "agent.web_search_provider:WebSearchProvider"

// Python plugin web search provider object proxied from the plugin host
type hostedWebSearchProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses WebSearchProvider as a provider, any other value unchanged
func adaptHostedWebSearchProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || (obj.Base != hostedWebSearchBase && !obj.HasMethod("search")) {
		return p
	}
	return hostedWebSearchProvider{obj: obj}
}

// Attribute of the hosted object, a failing read raising like the Python attribute access
func (h hostedWebSearchProvider) mustGet(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		panic(err)
	}
	return v
}

// Zero argument method call of the hosted object, a failing call raising like the Python call
func (h hostedWebSearchProvider) mustCall(method string) any {
	v, err := h.obj.Call(context.Background(), method, nil, nil)
	if err != nil {
		panic(err)
	}
	return v
}

func (h hostedWebSearchProvider) Name() string {
	s, _ := h.mustGet("name").(string)
	return s
}

func (h hostedWebSearchProvider) DisplayName() string {
	if s, ok := h.mustGet("display_name").(string); ok {
		return s
	}
	return h.Name()
}

func (h hostedWebSearchProvider) Available(*tools.Env) bool {
	return config.PyTruthy(h.mustCall("is_available"))
}
func (h hostedWebSearchProvider) KeylessAvailable(*tools.Env) bool {
	return config.PyTruthy(h.mustCall("is_keyless_available"))
}
func (h hostedWebSearchProvider) SupportsSearch() bool {
	return config.PyTruthy(h.mustCall("supports_search"))
}
func (h hostedWebSearchProvider) SupportsExtract() bool {
	return config.PyTruthy(h.mustCall("supports_extract"))
}

// Search result of the plugin decoded from its legacy dict contract
func (h hostedWebSearchProvider) Search(ctx context.Context, _ *tools.Env, query string, limit int) (*tools.WebSearchResponse, error) {
	v, err := h.obj.Call(ctx, "search", []any{query, limit}, nil)
	if err != nil {
		return nil, err
	}
	row, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("web provider %s search returned %s where a mapping was expected", h.Name(), errchain.Kind(v))
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	resp := &tools.WebSearchResponse{}
	if err := json.Unmarshal(raw, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Extract entries of the plugin, one page per returned dict row
func (h hostedWebSearchProvider) Extract(ctx context.Context, _ *tools.Env, urls []string, format string) ([]*tools.WebPage, error) {
	items := make([]any, len(urls))
	for i, u := range urls {
		items[i] = u
	}
	kwargs := map[string]any{"format": nil}
	if format != "" {
		kwargs["format"] = format
	}
	v, err := h.obj.Call(ctx, "extract", []any{items}, kwargs)
	if err != nil {
		return nil, err
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("web provider %s extract returned %s where a list was expected", h.Name(), errchain.Kind(v))
	}
	out := make([]*tools.WebPage, 0, len(rows))
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("web provider %s extract returned an entry that is %s where a mapping was expected", h.Name(), errchain.Kind(r))
		}
		out = append(out, hostedWebPage(row))
	}
	return out, nil
}

// Page entry of the plugin extract contract, raw content present only when the row carries the key
func hostedWebPage(row map[string]any) *tools.WebPage {
	text := func(key string) string {
		if s, ok := row[key].(string); ok {
			return s
		}
		return ""
	}
	page := &tools.WebPage{URL: text("url"), Title: text("title"), Content: text("content"), Error: text("error")}
	if raw, ok := row["raw_content"]; ok {
		page.HasRaw = true
		page.RawContent, _ = raw.(string)
	}
	if m, ok := row["metadata"].(map[string]any); ok {
		page.Metadata = m
	}
	if m, ok := row["blocked_by_policy"].(map[string]any); ok {
		page.BlockedByPolicy = m
	}
	return page
}

// Plugin facing registry that register_web_search_provider writes into, a plugin host object stored as the provider adapted from it
func WebSearchScopedRegistry() plugins.ScopedRegistry {
	return plugins.HostedScopedRegistry{Inner: tools.WebSearchRegistryScoped(), Adapt: adaptHostedWebSearchProvider,
		Object: func(held any) (*plugins.HostedObject, bool) {
			h, ok := held.(hostedWebSearchProvider)
			return h.obj, ok
		}}
}
