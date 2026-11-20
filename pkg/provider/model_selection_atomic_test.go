package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

const atomicSelectionSeed = "# Keep this header\n" +
	"model:\n" +
	"  default: old-model\n" +
	"  provider: custom:old\n" +
	"  base_url: https://old.example.test/v1\n" +
	"  api_mode: chat_completions\n" +
	"  api_key: ${OLD_MODEL_KEY}\n" +
	"  key_env: OLD_MODEL_KEY\n" +
	"  api_key_env: OLD_MODEL_KEY\n" +
	"  context_length: 32000\n" +
	"  model_slots:\n" +
	"    fast: \"keep-this\" # Keep this slot\n" +
	"auxiliary:\n" +
	"  vision:\n" +
	"    api_key: ${VISION_KEY}\n" +
	"display:\n" +
	"  label: \"off\" # Keep these quotes\n"

var atomicSelectionResult = ModelSwitchResult{Success: true, NewModel: "new-model", TargetProvider: "custom:new",
	BaseURL: "https://new.example.test/v1", APIMode: "anthropic_messages", IsGlobal: true}

func atomicSelectionExpected() map[string]any {
	return map[string]any{"default": "new-model", "provider": "custom:new", "base_url": "https://new.example.test/v1",
		"api_mode": "anthropic_messages", "model_slots": map[string]any{"fast": "keep-this"}}
}

func loadYAMLFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Default profile home plus a selected profile config seeded with the full route, default left untouched
func atomicSelectionProfile(t *testing.T) string {
	t.Helper()
	h := newTestHome(t)
	defaultConfig := filepath.Join(h, "config.yaml")
	writeConfigYAML(t, h, "model: default-profile-model\n")
	t.Setenv("OLD_MODEL_KEY", "synthetic-expanded-old-key")
	t.Setenv("VISION_KEY", "synthetic-expanded-vision-key")
	profile := filepath.Join(t.TempDir(), "selected-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(profile, "config.yaml")
	if err := os.WriteFile(path, []byte(atomicSelectionSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, _ := os.ReadFile(defaultConfig); string(data) != "model: default-profile-model\n" {
			t.Errorf("default profile config changed %q", data)
		}
	})
	return path
}

func swapSelectionPublish(t *testing.T, fn func(ctx context.Context, path string, doc map[string]any, extra string) error) {
	t.Helper()
	saved := selectionPublishFn
	selectionPublishFn = fn
	t.Cleanup(func() { selectionPublishFn = saved })
}

func TestPersistPublishesOnlyACompleteModelRoute(t *testing.T) {
	path := atomicSelectionProfile(t)
	var published []any
	swapSelectionPublish(t, func(ctx context.Context, target string, doc map[string]any, extra string) error {
		err := config.WriteConfigDocument(ctx, target, doc, extra)
		if target == path {
			published = append(published, loadYAMLFile(t, path)["model"])
		}
		return err
	})
	if err := PersistModelSelection(context.Background(), atomicSelectionResult, path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(published, []any{atomicSelectionExpected()}) {
		t.Fatalf("published %v", published)
	}
}

func TestFailedRoutePublicationLeavesOriginalBytes(t *testing.T) {
	path := atomicSelectionProfile(t)
	original, _ := os.ReadFile(path)
	swapSelectionPublish(t, func(ctx context.Context, target string, doc map[string]any, extra string) error {
		if model, _ := doc["model"].(map[string]any); target == path && model["provider"] == atomicSelectionResult.TargetProvider {
			return errors.New("synthetic publication failure")
		}
		return config.WriteConfigDocument(ctx, target, doc, extra)
	})
	if err := PersistModelSelection(context.Background(), atomicSelectionResult, path); err == nil || err.Error() != "synthetic publication failure" {
		t.Fatalf("err %v", err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(original) {
		t.Fatalf("bytes changed\n%s", now)
	}
}

func TestSettingSavedWhileRouteIsDecidedSurvives(t *testing.T) {
	path := atomicSelectionProfile(t)
	wrote := false
	saved := selectionClearsContextPinFn
	t.Cleanup(func() { selectionClearsContextPinFn = saved })
	selectionClearsContextPinFn = func(cm, am, cb, ab, cp, ap string) bool {
		if !wrote {
			wrote = true
			changes := config.Changes{Set: []config.KeyValue{{Key: "display.theme", Value: "dark"}}}
			if err := (config.FileBackend{}).WritePath(context.Background(), path, changes); err != nil {
				t.Fatal(err)
			}
		}
		return saved(cm, am, cb, ab, cp, ap)
	}
	if err := PersistModelSelection(context.Background(), atomicSelectionResult, path); err != nil {
		t.Fatal(err)
	}
	cfg := loadYAMLFile(t, path)
	display, _ := cfg["display"].(map[string]any)
	if !wrote || display["theme"] != "dark" || !reflect.DeepEqual(cfg["model"], atomicSelectionExpected()) {
		t.Fatalf("saved %v", cfg)
	}
}

func TestAnchoredModelRouteReusedLaterSavesTheNewRoute(t *testing.T) {
	h := newTestHome(t)
	path := filepath.Join(h, "config.yaml")
	writeConfigYAML(t, h, "model: &route\n  default: old-model\n  provider: custom:old\n  base_url: https://old.example.test/v1\n"+
		"  api_mode: chat_completions\nauxiliary:\n  vision: *route\n")
	if err := PersistModelSelection(context.Background(), atomicSelectionResult, path); err != nil {
		t.Fatal(err)
	}
	cfg := loadYAMLFile(t, path)
	route := atomicSelectionExpected()
	delete(route, "model_slots")
	aux, _ := cfg["auxiliary"].(map[string]any)
	if !reflect.DeepEqual(cfg["model"], route) || !reflect.DeepEqual(aux["vision"], route) {
		t.Fatalf("saved %v", cfg)
	}
}
