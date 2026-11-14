package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// External provider double contributing a fixed system prompt block
type promptBlockProvider struct {
	BaseProvider
	block string
}

func (p *promptBlockProvider) Name() string                       { return "blocky" }
func (p *promptBlockProvider) Available(ctx context.Context) bool { return true }
func (p *promptBlockProvider) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	return nil
}
func (p *promptBlockProvider) ToolSchemas(ctx context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (p *promptBlockProvider) SystemPromptBlock(ctx context.Context) (string, error) {
	return p.block, nil
}

func TestPromptMemoryGuidanceWhenMemoryToolLoaded(t *testing.T) {
	got := MemoryGuidanceFor(map[string]bool{"memory": true}, true, true)
	if got != BuildMemoryGuidance(true, true, false) {
		t.Fatalf("guidance %q", got)
	}
	if !strings.HasPrefix(got, "You have persistent memory") || !strings.Contains(got, "belongs in skills, not in memory") {
		t.Fatalf("guidance %q", got)
	}
}

func TestPromptNoMemoryGuidanceWhenBothStoresDisabled(t *testing.T) {
	got := MemoryGuidanceFor(map[string]bool{"memory": true, "skill_manage": true}, false, false)
	if got != "" || strings.Contains(got, MemoryGuidance) || strings.Contains(got, UserProfileGuidance) {
		t.Fatalf("guidance %q", got)
	}
}

func TestPromptProfileGuidanceWhenOnlyUserProfileEnabled(t *testing.T) {
	got := MemoryGuidanceFor(map[string]bool{"memory": true}, false, true)
	if got != BuildMemoryGuidance(false, true, false) || strings.Contains(got, MemoryGuidance) {
		t.Fatalf("guidance %q", got)
	}
	if !strings.Contains(got, "never target='memory'") {
		t.Fatalf("profile guidance %q", got)
	}
}

func TestPromptGuidanceNeedsTheMemoryToolAndRoutesToSkills(t *testing.T) {
	if MemoryGuidanceFor(map[string]bool{"skill_manage": true}, true, true) != "" {
		t.Fatal("no memory tool means no memory guidance")
	}
	got := MemoryGuidanceFor(map[string]bool{"memory": true, "skill_manage": true}, true, true)
	if got != MemoryGuidance || !strings.Contains(got, "Skills come first") {
		t.Fatalf("guidance %q", got)
	}
	if UserProfileGuidance != BuildMemoryGuidance(false, true, true) {
		t.Fatal("profile alias drifted")
	}
}

func TestPromptMemoryPartsStoreBlocksFollowFlags(t *testing.T) {
	store := NewStore(t.TempDir(), DefaultMemoryCharLimit, DefaultUserCharLimit, true, true)
	if _, err := store.Add(context.Background(), "memory", "Project uses Go"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(context.Background(), "user", "User prefers terse answers"); err != nil {
		t.Fatal(err)
	}
	if err := store.LoadFromDisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts := MemoryParts(context.Background(), nil, store, true, true, nil, nil)
	if len(parts) != 2 || !strings.Contains(parts[0], "Project uses Go") || !strings.Contains(parts[1], "User prefers terse answers") {
		t.Fatalf("parts %q", parts)
	}
	parts = MemoryParts(context.Background(), nil, store, false, true, nil, nil)
	if len(parts) != 1 || !strings.Contains(parts[0], "User prefers terse answers") {
		t.Fatalf("parts %q", parts)
	}
	if got := MemoryParts(context.Background(), nil, nil, true, true, nil, nil); len(got) != 0 {
		t.Fatalf("no store must yield no parts, got %q", got)
	}
}

func TestPromptMemoryPartsProviderBlockGatedOnToolExposure(t *testing.T) {
	ctx := context.Background()
	manager := NewMemoryManager(ctx, nil, nil)
	if err := manager.AddProvider(ctx, &promptBlockProvider{block: "# Blocky\nActive."}); err != nil {
		t.Fatal(err)
	}
	surface := []map[string]any{{"type": "function", "function": map[string]any{"name": "memory"}}}
	parts := MemoryParts(ctx, &tools.Env{}, nil, true, true, manager, surface)
	if len(parts) != 1 || parts[0] != "# Blocky\nActive." {
		t.Fatalf("parts %q", parts)
	}
	gated := &tools.Env{Disabled: []string{"memory"}}
	if got := MemoryParts(ctx, gated, nil, true, true, manager, surface); len(got) != 0 {
		t.Fatalf("gated toolset must withhold the provider block, got %q", got)
	}
	empty := &promptBlockProvider{block: "   "}
	quiet := NewMemoryManager(ctx, nil, nil)
	if err := quiet.AddProvider(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if got := MemoryParts(ctx, &tools.Env{}, nil, true, true, quiet, surface); len(got) != 0 {
		t.Fatalf("blank provider block must be skipped, got %q", got)
	}
}
