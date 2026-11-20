package memory

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session store double that counts close calls like the MagicMock db
type insightsSessionDBDouble struct {
	path   string
	closes int
}

func (d *insightsSessionDBDouble) Path() string { return d.path }

func (d *insightsSessionDBDouble) Close() error {
	d.closes++
	return nil
}

// Engine double that records generate calls and renders days and source
type insightsEngineDouble struct {
	calls *[]map[string]any
}

func (e *insightsEngineDouble) Generate(_ context.Context, days int, source string) (*InsightsReport, error) {
	*e.calls = append(*e.calls, map[string]any{"days": days, "source": source})
	return &InsightsReport{Days: days, SourceFilter: source}, nil
}

func (e *insightsEngineDouble) FormatTerminal(r *InsightsReport) string {
	return fmt.Sprintf("days=%d source=%s", r.Days, r.SourceFilter)
}

func (e *insightsEngineDouble) Close() error { return nil }

// Home whose state.db exists so insights reaches the store open
func insightsExistingStore(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, h := agentCmdsHome(t)
	path := filepath.Join(h, "state.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return ctx, path
}

type insightsOpenCall struct {
	path string
	opts state.Options
}

func TestSubcommandInsightsOpensReadOnly(t *testing.T) {
	ctx, existingStore := insightsExistingStore(t)
	db := &insightsSessionDBDouble{}
	var ctor []insightsOpenCall
	var calls []map[string]any
	savedOpen, savedEngine := openInsightsSessionDB, newInsightsTerminalEngine
	t.Cleanup(func() { openInsightsSessionDB, newInsightsTerminalEngine = savedOpen, savedEngine })
	openInsightsSessionDB = func(path string, opts state.Options) (insightsSessionDB, error) {
		ctor = append(ctor, insightsOpenCall{path, opts})
		db.path = path
		return db, nil
	}
	var engineDB insightsSessionDB
	newInsightsTerminalEngine = func(d insightsSessionDB) (insightsTerminalEngine, error) {
		engineDB = d
		return &insightsEngineDouble{calls: &calls}, nil
	}
	CmdInsights(ctx, io.Discard, 30, "")
	if len(ctor) != 1 || ctor[0] != (insightsOpenCall{existingStore, state.Options{ReadOnly: true}}) {
		t.Fatalf("SessionDB calls %+v", ctor)
	}
	if engineDB != db || len(calls) != 1 || calls[0]["days"] != 30 || calls[0]["source"] != "" {
		t.Fatalf("engine over %v generated %v", engineDB, calls)
	}
	if db.closes != 1 {
		t.Fatalf("db closed %d times", db.closes)
	}
}
