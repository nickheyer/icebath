package provider

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Offline picker home whose staged local models come from the real models dir
func invLocalHome(t *testing.T) (context.Context, string) {
	t.Helper()
	pickerTestHome(t, nil)
	h := invTestHome(t)
	inventoryStagedModelsFn = stagedModelIDs
	return context.Background(), h
}

func invStage(t *testing.T, h string, names ...string) {
	t.Helper()
	dir := filepath.Join(h, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name+".gguf"), append([]byte("GGUF"), make([]byte, 64)...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func invLoadContext(t *testing.T, ctx context.Context) PickerConfigContext {
	t.Helper()
	pc, err := loadPickerContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func TestNoStagedModelsNoRow(t *testing.T) {
	ctx, _ := invLocalHome(t)
	if row := localRuntimeRow(ctx, invLoadContext(t, ctx)); row != nil {
		t.Fatalf("row %v", row)
	}
}

func TestStagedModelsMakeASelectableRow(t *testing.T) {
	ctx, h := invLocalHome(t)
	invStage(t, h, "Qwen3-4B-Instruct-2507-UD-Q8_K_XL", "Some-Other-Model")
	row := localRuntimeRow(ctx, invLoadContext(t, ctx))
	if row == nil || row["slug"] != "llamacpp" || row["authenticated"] != true || row["total_models"] != 2 {
		t.Fatalf("row %v", row)
	}
	if !containsString(rowModels(row), "Qwen3-4B-Instruct-2507-UD-Q8_K_XL") {
		t.Fatalf("models %v", row["models"])
	}
}

func TestRowMarksCurrentWhenConfigPointsAtLlamacpp(t *testing.T) {
	ctx, h := invLocalHome(t)
	invStage(t, h, "M")
	row := localRuntimeRow(ctx, invLoadContext(t, ctx).WithOverrides("llamacpp", "", ""))
	if row == nil || row["is_current"] != true {
		t.Fatalf("row %v", row)
	}
}

func TestFullPayloadIncludesLocalRow(t *testing.T) {
	ctx, h := invLocalHome(t)
	invStage(t, h, "Local-Model-X")
	payload, err := buildModelsPayload(ctx, invLoadContext(t, ctx), ModelsPayloadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := invFind(t, payloadRows(payload), "llamacpp")
	if got := rowModels(row); len(got) != 1 || got[0] != "Local-Model-X" {
		t.Fatalf("models %v", got)
	}
}

func TestExplicitOnlyFilterKeepsLocalRowOnAnyProfile(t *testing.T) {
	ctx, h := invLocalHome(t)
	invStage(t, h, "Qwen3.8-27B-UD-Q5_K_XL")
	pc := invLoadContext(t, ctx)
	row := localRuntimeRow(ctx, pc)
	if row == nil {
		t.Fatal("no local row")
	}
	pc.CurrentProvider = "anthropic"
	kept := filterExplicitProviderRows(ctx, []map[string]any{row}, pc)
	if len(kept) == 0 || kept[0]["slug"] != "llamacpp" {
		t.Fatalf("explicit-only filter dropped the local-runtime row %v", kept)
	}
}

const invManagedURL = "http://127.0.0.1:18434/v1"

// Managed llama-server endpoint with two staged models and an otherwise empty offline home
func invManagedContext(t *testing.T) (context.Context, PickerConfigContext) {
	t.Helper()
	ctx, _ := invLocalHome(t)
	inventoryStagedModelsFn = func() []string {
		out := []string{"Qwen-A-UD-Q4_K_M", "Qwen-B-UD-Q4_K_M"}
		sort.Strings(out)
		return out
	}
	inventoryStateEndpointFn = func(string) (string, any, bool, error) { return invManagedURL, "k", true, nil }
	return ctx, invLoadContext(t, ctx)
}

func invExplicitRows(t *testing.T, ctx context.Context, pc PickerConfigContext) []map[string]any {
	t.Helper()
	payload, err := buildModelsPayload(ctx, pc, ModelsPayloadOptions{ExplicitOnly: true, ProbeCustomProviders: true})
	if err != nil {
		t.Fatal(err)
	}
	return payloadRows(payload)
}

func TestManagedCustomSessionShowsOnlyTheLocalRow(t *testing.T) {
	ctx, pc := invManagedContext(t)
	pc.CurrentProvider, pc.CurrentModel, pc.CurrentBaseURL = "custom", "Qwen-A-UD-Q4_K_M", invManagedURL
	rows := invExplicitRows(t, ctx, pc)
	if containsString(invSlugs(rows), "custom") {
		t.Fatalf("managed endpoint leaked a duplicate 'Custom endpoint' group %v", invSlugs(rows))
	}
	local := invFind(t, rows, "llamacpp")
	if local["is_current"] != true || local["name"] != "Local" {
		t.Fatalf("local %v", local)
	}
}

func TestExternalCustomEndpointKeepsItsRow(t *testing.T) {
	ctx, pc := invManagedContext(t)
	pc.CurrentProvider, pc.CurrentModel, pc.CurrentBaseURL = "custom", "some-model", "http://my-vllm-box:8000/v1"
	rows := invExplicitRows(t, ctx, pc)
	if custom := invFind(t, rows, "custom"); custom["is_current"] != true {
		t.Fatalf("custom %v", custom)
	}
	if local := invFind(t, rows, "llamacpp"); local["is_current"] != false {
		t.Fatalf("local %v", local)
	}
}

func TestRemoteProviderSessionUnaffected(t *testing.T) {
	ctx, pc := invManagedContext(t)
	rows := invExplicitRows(t, ctx, pc)
	if local := invFind(t, rows, "llamacpp"); local["is_current"] != false {
		t.Fatalf("local %v", local)
	}
	for _, r := range rows {
		if r["is_current"] == true && r["slug"] == "custom" {
			t.Fatal("custom row is current")
		}
	}
}
