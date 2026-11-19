package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session database in a fresh isolated home with the given cli rows created
func resumeTestDB(t *testing.T, rows ...state.NewSession) *state.SessionDB {
	t.Helper()
	h := goalHooksHome(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, row := range rows {
		if row.Source == "" {
			row.Source = "cli"
		}
		if err := db.CreateSession(row); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// CLI the way resume paths see it, on the ambient openrouter route
func restoreStandIn() *CLI {
	c := bareCLI()
	c.model, c.provider, c.requestedProvider = "ambient-model", "openrouter", "openrouter"
	c.baseURL, c.apiKey = "https://openrouter.ai/api/v1", "ambient-key"
	c.console = console()
	return c
}

// Session row with a model and an optional model config encoded as JSON text
func restoreRow(model any, modelConfig map[string]any) map[string]any {
	var cfg any
	if modelConfig != nil {
		raw, _ := json.Marshal(modelConfig)
		cfg = string(raw)
	}
	return map[string]any{"model": model, "model_config": cfg}
}

func TestRestoreSessionModelRestoresModelAndProvider(t *testing.T) {
	resumeTestDB(t)
	recordPrints(t)
	c := restoreStandIn()
	c.restoreSessionModel(restoreRow("glm-4.7", map[string]any{"gateway_runtime": map[string]any{"provider": "custom:feather", "base_url": "https://f/v1"}}), false)
	if c.model != "glm-4.7" || c.provider != "custom:feather" || c.requestedProvider != "custom:feather" || c.baseURL != "https://f/v1" {
		t.Fatalf("route %q %q %q %q", c.model, c.provider, c.requestedProvider, c.baseURL)
	}
	if c.explicitAPIKey != "" || c.explicitBaseURL != "https://f/v1" {
		t.Fatalf("explicit %q %q", c.explicitAPIKey, c.explicitBaseURL)
	}
}

func TestRestoreSessionModelExplicitCliFlagWins(t *testing.T) {
	c := restoreStandIn()
	c.model, c.explicitModelOverride = "cli-flag-model", true
	c.restoreSessionModel(restoreRow("glm-4.7", nil), false)
	if c.model != "cli-flag-model" || c.provider != "openrouter" {
		t.Fatalf("route %q %q", c.model, c.provider)
	}
}

func TestRestoreSessionModelNoStoredModelIsNoop(t *testing.T) {
	c := restoreStandIn()
	c.restoreSessionModel(restoreRow(nil, nil), false)
	if c.model != "ambient-model" {
		t.Fatalf("model %q", c.model)
	}
}

func TestRestoreSessionModelMatchingStateIsSilentNoop(t *testing.T) {
	rec := recordPrints(t)
	c := restoreStandIn()
	c.model, c.provider, c.requestedProvider = "glm-4.7", "custom:feather", "custom:feather"
	c.restoreSessionModel(restoreRow("glm-4.7", map[string]any{"gateway_runtime": map[string]any{"provider": "custom:feather"}}), false)
	if out := strings.Join(rec.plain(), "\n"); out != "" {
		t.Fatalf("printed %q", out)
	}
}
