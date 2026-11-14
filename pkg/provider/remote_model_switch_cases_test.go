package provider

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/home"
)

// A switch to a new endpoint stores null over inherited endpoint bound keys a profile unset cannot reach
func TestModelSwitchMasksInheritedEndpointCredentials(t *testing.T) {
	p := setupBootPlane(t, true)
	p.Upper = map[string]any{"model": map[string]any{"default": "old", "provider": "custom:old", "base_url": "https://old.example/v1",
		"api_key": "${OLD_KEY}", "key_env": "OLD_KEY", "api_key_env": "OLD_KEY", "context_length": 123456}}
	path := filepath.Join(p.Home, "config.yaml")
	ctx := context.Background()
	if err := PersistModelSelection(ctx, ModelSwitchResult{Success: true, NewModel: "new", TargetProvider: "custom", ProviderChanged: true,
		BaseURL: "https://new.example/v1", APIMode: "chat_completions", IsGlobal: true}, path); err != nil {
		t.Fatal(err)
	}
	patches := p.Patches()
	if len(patches) == 0 {
		t.Fatal("nothing sent")
	}
	set, _ := patches[len(patches)-1].Body["set"].(map[string]any)
	for _, key := range []string{"api_key", "key_env", "api_key_env", "context_length"} {
		if v, has := set["model."+key]; !has || v != nil {
			t.Fatalf("model.%s sent %v in %v", key, v, patches[len(patches)-1].Body)
		}
	}
	raw, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := raw["model"].(map[string]any)
	if model["base_url"] != "https://new.example/v1" {
		t.Fatalf("model %v", model)
	}
	for _, key := range []string{"api_key", "key_env", "api_key_env", "context_length"} {
		if model[key] != nil {
			t.Fatalf("model.%s %v", key, model[key])
		}
	}
}

// A lock landing after the preflight guard, seen by a poll or by the re-read after a 409, refuses the whole assignment
func TestDashboardModelSwitchRefusesWhenALockLandsAfterTheGuard(t *testing.T) {
	for _, conflictFirst := range []bool{false, true} {
		name := map[bool]string{false: "poll", true: "cas-retry"}[conflictFirst]
		t.Run(name, func(t *testing.T) {
			p := setupBootPlane(t, true)
			url := "https://new.example/v1"
			p.Upper = map[string]any{"model": map[string]any{"default": "old", "provider": "custom:old", "base_url": "https://old.example/v1",
				"api_key": "${OLD_KEY}", "key_env": "OLD_KEY", "api_key_env": "OLD_KEY", "context_length": 123456},
				"providers": map[string]any{"known-new": map[string]any{"base_url": url}}}
			ctx := home.WithOverride(context.Background(), p.Home)
			if _, err := config.ReadRawConfig(ctx, p.Home); err != nil {
				t.Fatal(err)
			}
			realSave := configSaveFn
			t.Cleanup(func() { configSaveFn = realSave })
			configSaveFn = func(ctx context.Context, cfg map[string]any) error {
				p.Mu.Lock()
				p.UpperLocks = []map[string]any{{"path": "model.api_key", "level": "tenant"}}
				if conflictFirst {
					p.Profile("default").Version++
				}
				p.Mu.Unlock()
				if !conflictFirst {
					remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
				}
				return realSave(ctx, cfg)
			}
			prepared := &PreparedMainAssignment{BaseURL: url, Result: ModelSwitchResult{Success: true, NewModel: "new", TargetProvider: "custom",
				ProviderChanged: true, BaseURL: url, APIMode: "chat_completions", IsGlobal: true}}
			_, err := ApplyModelAssignmentSync(ctx, "main", "custom", "new", "", url, "", AuxReasoningEffort{}, prepared)
			if http := configRefusalHTTP(err); http == nil || http.StatusCode != 403 {
				t.Fatalf("err %v http %+v", err, http)
			}
			if n := len(p.Patches()); !conflictFirst && n != 0 {
				t.Fatalf("%d patches", n)
			}
			p.Mu.Lock()
			model, has := p.Profile("default").Values["model"]
			p.Mu.Unlock()
			if m, _ := model.(map[string]any); has && model != nil && len(m) != 0 {
				t.Fatalf("half route saved %v", model)
			}
		})
	}
}
