package dashboard

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Dashboard client over a home whose model is openrouter openai/gpt-5.5
func rmwLockClient(t *testing.T) *testClient {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	h := home.Root()
	cfg, err := config.Load(h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg["model"] = map[string]any{"provider": "openrouter", "default": "openai/gpt-5.5"}
	if err := config.SaveConfig(context.Background(), h, map[string]any(cfg), config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	return c
}

// Both writers start while the config mutation lock is held elsewhere and neither writes until it is released
func raceWritersAgainstMutationLock(t *testing.T, first, second func() *testResponse) (*testResponse, *testResponse) {
	t.Helper()
	path := home.ConfigPath(home.Root())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, unlock := provider.LockConfigMutation(context.Background())
	results := [2]*testResponse{}
	done := make(chan int, 2)
	go func() { results[0] = first(); done <- 0 }()
	go func() { results[1] = second(); done <- 1 }()
	select {
	case i := <-done:
		unlock()
		t.Fatalf("writer %d finished while the config mutation lock was held: %d %s", i, results[i].Status, results[i].Body)
	case <-time.After(1500 * time.Millisecond):
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, held) {
		unlock()
		t.Fatalf("config.yaml was written while the config mutation lock was held:\n%s", held)
	}
	unlock()
	for range 2 {
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("writer never finished")
		}
	}
	return results[0], results[1]
}

// Parsed config.yaml of the active home
func rmwOnDisk(t *testing.T) map[string]any {
	t.Helper()
	_, tree := readNullSectionConfig(t, home.ConfigPath(home.Root()))
	return tree.(map[string]any)
}

func TestCustomEndpointUpsertRacingConfigAutosaveKeepsBothWrites(t *testing.T) {
	c := rmwLockClient(t)
	autosave := func() *testResponse {
		return appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", pyjson.ObjectOf("display", pyjson.ObjectOf("personality", "canary"))))
	}
	upsert := func() *testResponse {
		return appJSON(t, c, "POST", "/api/providers/custom-endpoints", pyjson.ObjectOf(
			"id", "racebox", "name", "racebox", "base_url", "http://racebox:8000/v1", "model", "race-model", "discover_models", false))
	}
	r1, r2 := raceWritersAgainstMutationLock(t, autosave, upsert)
	if r1.Status != 200 {
		t.Fatalf("autosave %d %s", r1.Status, r1.Body)
	}
	if r2.Status != 200 {
		t.Fatalf("upsert %d %s", r2.Status, r2.Body)
	}
	onDisk := rmwOnDisk(t)
	if _, ok := onDisk["providers"].(map[string]any)["racebox"]; !ok {
		t.Fatalf("providers lost racebox: %#v", onDisk["providers"])
	}
	if got := onDisk["display"].(map[string]any)["personality"]; got != "canary" {
		t.Fatalf("display.personality = %#v", got)
	}
}

func TestCustomEndpointActivateRacingMoaSaveKeepsBothWrites(t *testing.T) {
	c := rmwLockClient(t)
	h := home.Root()
	cfg, err := config.Load(h, config.EnvRefLookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg["providers"] = map[string]any{"racebox": map[string]any{"base_url": "http://racebox:8000/v1", "model": "race-model", "api_key": "k"}}
	if err := config.SaveConfig(context.Background(), h, map[string]any(cfg), config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	activate := func() *testResponse {
		return c.request("POST", "/api/providers/custom-endpoints/racebox/activate", nil, sessionHeaders()...)
	}
	moa := func() *testResponse {
		return appJSON(t, c, "PUT", "/api/model/moa", pyjson.ObjectOf(
			"reference_models", []any{pyjson.ObjectOf("provider", "openrouter", "model", "openai/gpt-5.5")},
			"aggregator", pyjson.ObjectOf("provider", "openrouter", "model", "openai/gpt-5.5")))
	}
	r1, r2 := raceWritersAgainstMutationLock(t, activate, moa)
	if r1.Status != 200 {
		t.Fatalf("activate %d %s", r1.Status, r1.Body)
	}
	if r2.Status != 200 {
		t.Fatalf("moa %d %s", r2.Status, r2.Body)
	}
	onDisk := rmwOnDisk(t)
	if got := onDisk["model"].(map[string]any)["provider"]; got != "racebox" {
		t.Fatalf("model.provider = %#v", got)
	}
	if got := onDisk["moa"].(map[string]any)["aggregator"].(map[string]any)["model"]; got != "openai/gpt-5.5" {
		t.Fatalf("moa.aggregator.model = %#v", got)
	}
}
