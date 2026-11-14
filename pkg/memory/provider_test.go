package memory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestProviderTrivialPromptVariants(t *testing.T) {
	for _, text := range []string{"hi", "HI!", "hey.", "hello", "yo", "sup~", "thanks :)", "done???", "ok", "yes.", "k", "", "   ", "/help", "lgtm"} {
		if !IsTrivialPrompt(text) {
			t.Fatalf("expected trivial %q", text)
		}
	}
}

func TestProviderSubstantiveAndPrefixCollisionsPassThrough(t *testing.T) {
	for _, text := range []string{"k8s", "yolo", "hive", "note", "supper", "hind", "hello world", "ok so what's next",
		"what's my name", "hey can you check the logs", "continue the migration plan"} {
		if IsTrivialPrompt(text) {
			t.Fatalf("expected non trivial %q", text)
		}
	}
}

func TestProviderTrivialPromptUnicodePunctuationAndCase(t *testing.T) {
	for _, text := range []string{"thanks’", "ok…", "Thank You!!", "go ahead ", "cool —", "K"} {
		if !IsTrivialPrompt(text) {
			t.Fatalf("expected trivial %q", text)
		}
	}
}

func TestProviderCoreSentinelsNameTheBuiltinStore(t *testing.T) {
	for _, name := range []string{"builtin", "Default", "none", "", "  built-in  ", "NONE"} {
		if !IsCoreMemoryProvider(name) {
			t.Fatalf("expected core sentinel %q", name)
		}
	}
	for _, name := range []string{"honcho", "byterover", "builtins"} {
		if IsCoreMemoryProvider(name) {
			t.Fatalf("expected plugin name %q", name)
		}
	}
}

func TestProviderRecallStatusDefaultsToTheBrainGlyph(t *testing.T) {
	s := NewRecallStatus("Hindsight", 3)
	if s.Glyph != IndicatorGlyph || s.ProviderLabel != "Hindsight" || s.Count != 3 {
		t.Fatal(s)
	}
	if PreCompressCheckpointAPIVersion != 2 {
		t.Fatal(PreCompressCheckpointAPIVersion)
	}
}

// Provider implementing only the required surface on top of the optional hook defaults
type provtMinimal struct {
	BaseProvider
}

func (provtMinimal) Name() string                       { return "minimal" }
func (provtMinimal) Available(ctx context.Context) bool { return true }
func (provtMinimal) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	return nil
}
func (provtMinimal) ToolSchemas(ctx context.Context) ([]map[string]any, error) { return nil, nil }

func TestProviderBaseDefaultsAreInertAndRefuseTools(t *testing.T) {
	var p Provider = provtMinimal{}
	ctx := t.Context()
	if r := p.UnavailableReason(ctx); r != "" {
		t.Fatal(r)
	}
	if b, err := p.SystemPromptBlock(ctx); b != "" || err != nil {
		t.Fatal(b, err)
	}
	if r, err := p.Prefetch(ctx, "q", "s"); r != "" || err != nil {
		t.Fatal(r, err)
	}
	if s, err := p.RecallStatus(ctx); s != nil || err != nil {
		t.Fatal(s, err)
	}
	if r, err := p.OnPreCompress(ctx, nil, true); r != "" || err != nil {
		t.Fatal(r, err)
	}
	if p.CheckpointAPIVersion() != 1 {
		t.Fatal(p.CheckpointAPIVersion())
	}
	if sig := p.IdentitySignature(ctx); len(sig) != 0 || sig == nil {
		t.Fatal(sig)
	}
	if paths, err := p.BackupPaths(); len(p.ConfigSchema()) != 0 || len(paths) != 0 || err != nil {
		t.Fatal("non empty defaults")
	}
	_, err := p.HandleToolCall(ctx, "x_tool", nil, ToolCallOptions{})
	var nh *NotHandledError
	if !errors.As(err, &nh) || nh.Tool != "x_tool" {
		t.Fatal(err)
	}
	nh.Provider = "minimal"
	if err.Error() != "Provider minimal does not handle tool x_tool" {
		t.Fatal(err.Error())
	}
}

func TestProviderContextThreadInheritsTheSpawnerProfile(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(home.WithOverride(context.WithValue(t.Context(), key{}, "v"), "/profiles/alpha"))
	var seenHome, seenValue atomic.Value
	release := make(chan struct{})
	th := SpawnContextThread(parent, "memory-test", func(ctx context.Context) {
		<-release
		seenHome.Store(home.Override(ctx))
		seenValue.Store(ctx.Value(key{}))
		if ctx.Err() != nil {
			seenValue.Store("cancelled")
		}
	})
	if th.Alive() {
		t.Fatal("alive before start")
	}
	th.Join(time.Second)
	th.Start()
	th.Start()
	if !th.Alive() {
		t.Fatal("not alive after start")
	}
	cancel()
	th.Join(10 * time.Millisecond)
	if !th.Alive() {
		t.Fatal("join returned a finished thread early")
	}
	close(release)
	th.Join(-1)
	if th.Alive() {
		t.Fatal("alive after join")
	}
	if seenHome.Load() != "/profiles/alpha" || seenValue.Load() != "v" {
		t.Fatal(seenHome.Load(), seenValue.Load())
	}
}

func TestProviderCtxBoundKeepsValuesWithoutCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(home.WithOverride(t.Context(), "/h"))
	cancel()
	bound := CtxBound(parent)
	if bound.Err() != nil || home.Override(bound) != "/h" {
		t.Fatal(bound.Err(), home.Override(bound))
	}
	if CtxBound(nil) == nil {
		t.Fatal("nil context")
	}
}
