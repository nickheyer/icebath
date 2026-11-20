package mcp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPickerInstallByNameSuccess(t *testing.T) {
	dir, h := catalogFixture(t)
	writeManifest(t, dir, "demo", basicManifest("demo", nil))
	ui := &fakeUI{}
	if rc, err := InstallByName(context.Background(), &tools.Env{}, ui, "demo"); err != nil || rc != 0 {
		t.Fatalf("%d %v", rc, err)
	}
	if serverBlock(t, h, "demo") == nil {
		t.Fatal("not installed")
	}
	if rc, err := InstallByName(context.Background(), &tools.Env{}, ui, "nope"); err != nil || rc != 1 ||
		!strings.Contains(ui.out.String(), "'nope' is not in the catalog. Run `icebath mcp catalog`") {
		t.Fatalf("%d %v %s", rc, err, ui.out.String())
	}
}

func TestPickerRunPickerNonTTYFallsBack(t *testing.T) {
	dir, _ := catalogFixture(t)
	writeManifest(t, dir, "demo", basicManifest("demo", nil))
	ui := &fakeUI{}
	if err := RunPicker(context.Background(), &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ui.out.String(), "demo") || !strings.Contains(ui.out.String(), "MCP Catalog + configured servers:") {
		t.Fatal(ui.out.String())
	}
}

func TestPickerCustomMCPShownAlongsideCatalog(t *testing.T) {
	dir, h := catalogFixture(t)
	writeManifest(t, dir, "demo", basicManifest("demo", nil))
	writeManifest(t, dir, "future", basicManifest("future", map[string]any{"manifest_version": 2}))
	writeServers(t, h, map[string]any{"my-custom": map[string]any{"command": "npx", "args": []any{"-y", "my-custom-mcp"}, "enabled": true},
		"off-custom": map[string]any{"url": "https://x.example/mcp", "enabled": false}})
	ui := &fakeUI{}
	if err := ShowCatalog(context.Background(), ui); err != nil {
		t.Fatal(err)
	}
	out := ui.out.String()
	for _, want := range []string{
		"  demo               available                Demo MCP\n",
		"  my-custom          custom — enabled         npx\n",
		"  off-custom         custom — disabled        https://x.example/mcp\n",
		"  ⚠ 'future' requires a newer icebath — run `icebath update` to install this entry.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "my-custom") > strings.Index(out, "off-custom") {
		t.Fatal("custom rows not sorted")
	}
}

func TestPickerRowStatusesAndActions(t *testing.T) {
	dir, h := catalogFixture(t)
	writeManifest(t, dir, "demo", basicManifest("demo", nil))
	ctx := context.Background()
	rows, err := BuildPickerRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].Status != pickerStatusNotInstalled {
		t.Fatalf("%v %v", rows, err)
	}
	ui := &fakeUI{interacted: true, selects: []int{0, -1}}
	if err := RunPicker(ctx, &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if rows, _ := BuildPickerRows(ctx); rows[0].Status != pickerStatusEnabled {
		t.Fatalf("%v", rows)
	}
	ui = &fakeUI{interacted: true, selects: []int{0, 1, -1}}
	if err := RunPicker(ctx, &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if rows, _ := BuildPickerRows(ctx); rows[0].Status != pickerStatusDisabled || !strings.Contains(ui.out.String(), "'demo' is already enabled.") {
		t.Fatalf("%v %s", rows, ui.out.String())
	}
	ui = &fakeUI{interacted: true, selects: []int{0, -1}}
	if err := RunPicker(ctx, &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if serverBlock(t, h, "demo")["enabled"] != true || !strings.Contains(ui.out.String(), "'demo' enabled. Start a new icebath session") {
		t.Fatalf("%v %s", serverBlock(t, h, "demo"), ui.out.String())
	}
	ui = &fakeUI{interacted: true, selects: []int{0, 2, -1}, yesNo: []bool{true}}
	if err := RunPicker(ctx, &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if serverBlock(t, h, "demo") != nil || !strings.Contains(ui.out.String(), "Uninstalled 'demo'. Credentials in .env preserved") {
		t.Fatalf("%s", ui.out.String())
	}
	writeServers(t, h, map[string]any{"mine": map[string]any{"command": "x"}})
	var configured []string
	prev := pickerConfigure
	pickerConfigure = func(ctx context.Context, env *tools.Env, ui TerminalUI, name string) int {
		configured = append(configured, name)
		return 0
	}
	t.Cleanup(func() { pickerConfigure = prev })
	ui = &fakeUI{interacted: true, selects: []int{1, 0, 1, 1, 1, 1, 1, 2, -1}, yesNo: []bool{true}}
	if err := RunPicker(ctx, &tools.Env{}, ui); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(configured, []string{"mine"}) {
		t.Fatalf("configure calls %v", configured)
	}
	if _, ok := userServers(t, h)["mine"]; ok || !strings.Contains(ui.out.String(), "'mine' disabled.") ||
		!strings.Contains(ui.out.String(), "'mine' enabled.") || !strings.Contains(ui.out.String(), "Removed 'mine'") {
		t.Fatalf("%s", ui.out.String())
	}
}
