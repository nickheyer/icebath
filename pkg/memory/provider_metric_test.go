package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

type metricToolProvider struct {
	BaseProvider
	name, tool, result string
	boom               bool
}

func (p *metricToolProvider) Name() string                   { return p.name }
func (p *metricToolProvider) Available(context.Context) bool { return true }
func (p *metricToolProvider) Initialize(context.Context, string, InitOptions) error {
	return nil
}
func (p *metricToolProvider) ToolSchemas(context.Context) ([]map[string]any, error) {
	return []map[string]any{{"name": p.tool, "description": "", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}, nil
}
func (p *metricToolProvider) HandleToolCall(context.Context, string, map[string]any, ToolCallOptions) (string, error) {
	if p.boom {
		return "", errors.New("secret backend detail")
	}
	return p.result, nil
}

func TestBundledMemoryProvidersHookListsShippedProviders(t *testing.T) {
	if observability.BundledMemoryProvidersHook == nil {
		t.Fatal("bundled memory providers hook unset")
	}
	if got := observability.BundledMemoryProvidersHook(); !slices.Equal(got, []string{"byterover", "retaindb"}) {
		t.Fatalf("bundled providers %v", got)
	}
}

func TestMemoryProviderToolsReportBoundedProviderAndOp(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var rows [][4]string
	env := &tools.Env{Values: &sync.Map{}, Config: config.Tree{"telemetry": map[string]any{"shared_metrics": map[string]any{"enabled": true}}}}
	env.Notify = func(ev tools.Event) {
		if ev.Kind != "metric" || ev.Data["name"] != memoryOpMetricName {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		rows = append(rows, [4]string{ev.Data["provider"].(string), ev.Data["op"].(string), ev.Data["outcome"].(string), ev.Data["failure_class"].(string)})
	}
	for _, c := range []struct {
		p    *metricToolProvider
		args map[string]any
	}{
		{&metricToolProvider{name: "byterover", tool: "brv_query", result: `{"result": "x"}`}, map[string]any{"query": "q"}},
		{&metricToolProvider{name: "honcho", tool: "honcho_search", result: `{"results": []}`}, map[string]any{"query": "q"}},
		{&metricToolProvider{name: "acme-private-memory", tool: "acme_private_remember", boom: true}, map[string]any{"content": "c"}},
	} {
		manager := NewMemoryManager(ctx, env, nil)
		if err := manager.AddProvider(ctx, c.p); err != nil {
			t.Fatal(err)
		}
		manager.HandleToolCall(ctx, c.p.tool, c.args, "")
	}
	want := [][4]string{
		{"byterover", "search", "success", "none"},
		{"honcho", "search", "success", "none"},
		{"plugin", "add", "failed", "exception"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows\n%v\nwant\n%v", rows, want)
	}
}
