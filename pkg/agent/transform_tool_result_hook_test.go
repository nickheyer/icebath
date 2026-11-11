package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Compiled-in plugin prefixing every tool result with its tool name, the transform_result_canon plugin of the case
type transformCanonPlugin struct{}

func (transformCanonPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "transform_result_canon"}
}

func (transformCanonPlugin) Register(ctx *plugins.Context) error {
	ctx.RegisterHook("transform_tool_result", func(_ context.Context, kw map[string]any) (any, error) {
		return fmt.Sprintf("CANON[%v]%v", kw["tool_name"], kw["result"]), nil
	}, false)
	return nil
}

func TestTransformToolResultIntegrationWithRealPlugin(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n  - transform_result_canon\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plugins.SwapBundled(transformCanonPlugin{}))
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{InvokeLifecycleHook: LifecycleInvokeHook})
	t.Cleanup(func() { plugins.InstallServices(previous) })
	ctx := context.Background()
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, false)
	r := tools.NewRegistry()
	r.Register(tools.Def{Name: "some_tool", Toolset: "test", Handler: func(context.Context, *tools.Call) string { return `{"payload": 42}` },
		Schema: map[string]any{"name": "some_tool", "description": "some_tool", "parameters": map[string]any{"type": "object"}}})
	d := tools.NewDispatcher(r)
	installToolPluginBridge(d)
	out := d.DispatchWith(tools.WithDispatcher(ctx, d), &tools.Call{Name: "some_tool", Args: map[string]any{}, TaskID: "t1", SessionID: "s1",
		ToolCallID: "tc1", Env: &tools.Env{Home: h}}, tools.DispatchOptions{SkipPreToolCallHook: true})
	if out != `CANON[some_tool]{"payload": 42}` {
		t.Fatalf("out %q", out)
	}
}
