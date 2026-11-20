package cron

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// TestCatalog test_seed_registers_all_entries
func TestCatalogSeedRegistersAllEntries(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	ctx := home.WithOverride(context.Background(), h)
	s := openCronStore(h, time.Now)
	created, err := SeedCatalogSuggestions(ctx, s.AddSuggestion, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != len(Catalog) {
		t.Fatalf("created %d of %d", len(created), len(Catalog))
	}
	if got := pendingCount(t, s); got != min(len(Catalog), MaxPendingSuggestions) {
		t.Fatalf("pending %d", got)
	}
}

// TestCommandHandler test_bare_lists_pending
func TestCommandHandlerBareListsPending(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	ctx := home.WithOverride(context.Background(), h)
	addTestSuggestion(t, openCronStore(h, time.Now), "c1", "Daily thing", "catalog", "0 9 * * *")
	out, err := HandleSuggestionsCommand(ctx, "", nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Daily thing") {
		t.Fatalf("output %q", out)
	}
}
