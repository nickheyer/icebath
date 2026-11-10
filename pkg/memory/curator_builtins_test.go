package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Writes config.yaml for the skills scope and drops the cached tree
func curWriteConfig(t *testing.T, h, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
}

func curPruneBuiltins(t *testing.T, h string) {
	curWriteConfig(t, h, "skills:\n  disabled:\n    - disabled-fixture\ncurator:\n  prune_builtins: true\n")
}

func curWriteBundledAndAgent(t *testing.T, ctx context.Context, h string) string {
	t.Helper()
	dir := filepath.Join(h, "skills")
	for _, n := range []string{"bundled-fixture", "disabled-fixture", "agent-fixture"} {
		curWriteSkill(t, dir, n, "")
	}
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-fixture:deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	curWriteConfig(t, h, "skills:\n  disabled:\n    - disabled-fixture\n")
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "disabled-fixture")
	scope.MarkAgentCreated(ctx, "agent-fixture")
	return dir
}

func curSetRecord(t *testing.T, ctx context.Context, name string, fields map[string]any) {
	t.Helper()
	scope, _, _ := curatorScope(ctx)
	data, err := scope.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := scope.GetRecord("never-recorded-name")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		rec.Set(k, v)
	}
	data.Set(name, rec)
	if !scope.SaveUsage(ctx, data) {
		t.Fatal("save usage failed")
	}
}

func TestCuratorBundledSkillsAreOffLimitsUnlessOptedIn(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, "ship", "")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("ship:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, _, _ := curatorScope(ctx)
	curWriteConfig(t, h, "curator: {}\n")
	if ok, _ := scope.IsCurationEligible("ship", ""); ok {
		t.Fatal("bundled skill eligible without prune_builtins")
	}
	if cur := config.AsMap(config.Defaults()["curator"]); cur["prune_builtins"] != false {
		t.Fatalf("default curator.prune_builtins %v", cur["prune_builtins"])
	}
	curWriteConfig(t, h, "curator:\n  prune_builtins: true\n")
	if ok, _ := scope.IsCurationEligible("ship", ""); !ok {
		t.Fatal("bundled skill ineligible with prune_builtins on")
	}
}

func TestCuratorLLMCandidateListOmitsBundledAndDisabledSkills(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	curWriteBundledAndAgent(t, ctx, h)
	curPruneBuiltins(t, h)
	scope, _, _ := curatorScope(ctx)
	names, err := scope.ListAgentCreatedSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "agent-fixture,bundled-fixture,disabled-fixture" {
		t.Fatalf("aging set %v", names)
	}
	listing, err := curatorRenderCandidateList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "agent-fixture") || strings.Contains(listing, "bundled-fixture") || strings.Contains(listing, "disabled-fixture") {
		t.Fatalf("listing:\n%s", listing)
	}
}

func TestCuratorLLMPromptDoesNotInviteBundledWritesWhenPruneBuiltinsOn(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := curWriteBundledAndAgent(t, ctx, h)
	curPruneBuiltins(t, h)
	var prompts []string
	curatorLLMReview = func(_ context.Context, prompt string) *pyjson.Object {
		prompts = append(prompts, prompt)
		return curatorLLMMeta("s", nil)
	}
	on := true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, Consolidate: &on, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "agent-fixture") || strings.Contains(prompts[0], "bundled-fixture") || strings.Contains(prompts[0], "disabled-fixture") {
		t.Fatalf("prompts %q", prompts)
	}
	if err := os.RemoveAll(filepath.Join(dir, "agent-fixture")); err != nil {
		t.Fatal(err)
	}
	prompts = nil
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, Consolidate: &on, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 0 {
		t.Fatal("LLM pass ran with no curation candidates")
	}
}

func TestCuratorPruneBuiltinsStillArchivesBundledViaDeterministicPass(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := curWriteBundledAndAgent(t, ctx, h)
	curPruneBuiltins(t, h)
	old := pydatetime.ISOFormatUTC(time.Now().Add(-200 * 24 * time.Hour))
	curSetRecord(t, ctx, "bundled-fixture", map[string]any{"last_used_at": old, "created_at": old, "use_count": 1})
	listing, err := curatorRenderCandidateList(ctx)
	if err != nil || strings.Contains(listing, "bundled-fixture") {
		t.Fatalf("listing %q %v", listing, err)
	}
	counts, err := ApplyCuratorTransitions(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if curatorCount(counts, "archived") < 1 {
		t.Fatalf("counts %v", counts)
	}
	if _, err := os.Stat(filepath.Join(dir, "bundled-fixture")); !os.IsNotExist(err) {
		t.Fatal("bundled skill not archived")
	}
	scope, _, _ := curatorScope(ctx)
	if sup, _ := scope.ReadSuppressedNames(); !sup["bundled-fixture"] {
		t.Fatalf("suppression list %v", sup)
	}
}

func TestCuratorProtectedBuiltinNeverArchivedEvenWhenStale(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	name := "sentinel-protected-skill"
	prev := skills.ProtectedBuiltinSkills
	skills.ProtectedBuiltinSkills = map[string]bool{name: true}
	t.Cleanup(func() { skills.ProtectedBuiltinSkills = prev })
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, name, "")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte(name+":abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	curPruneBuiltins(t, h)
	curSetRecord(t, ctx, name, map[string]any{"last_used_at": pydatetime.ISOFormatUTC(time.Now().Add(-500 * 24 * time.Hour))})
	counts, err := ApplyCuratorTransitions(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if curatorCount(counts, "archived") != 0 {
		t.Fatalf("counts %v", counts)
	}
	scope, _, _ := curatorScope(ctx)
	names, _ := scope.ListAgentCreatedSkillNames()
	sup, _ := scope.ReadSuppressedNames()
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil || sup[name] || strings.Contains(","+strings.Join(names, ",")+",", ","+name+",") {
		t.Fatalf("protected skill touched: names %v suppressed %v %v", names, sup, err)
	}
}

func TestCuratorPreseededNeverUsedBuiltinIsReanchoredNotStaled(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, "bundled-helper", "")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-helper:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	curPruneBuiltins(t, h)
	old := time.Now().Add(-365 * 24 * time.Hour)
	curSetRecord(t, ctx, "bundled-helper", map[string]any{"created_at": pydatetime.ISOFormatUTC(old), "state": "stale"})
	t0 := time.Now().UTC()
	counts, err := ApplyCuratorTransitions(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if curatorCount(counts, "marked_stale") != 0 || curatorCount(counts, "archived") != 0 || curatorCount(counts, "seeded") != 1 {
		t.Fatalf("counts %v", counts)
	}
	rec := curRecord(t, ctx, "bundled-helper")
	created, _, err := pydatetime.ParseISOFormat(curGet(rec, "created_at").(string))
	if curGet(rec, "state") != "active" || curGet(rec, "first_seen_at") == nil || err != nil || !created.After(old) {
		t.Fatalf("record %v", rec)
	}
	counts, err = ApplyCuratorTransitions(ctx, t0.Add(15*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if curatorCount(counts, "marked_stale") != 1 || curGet(curRecord(t, ctx, "bundled-helper"), "state") != "stale" {
		t.Fatalf("counts %v", counts)
	}
}

func TestCuratorPruneBuiltinsNeverTouchesHubSkills(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, "hubskill", "")
	if err := os.MkdirAll(filepath.Join(dir, ".hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hub", "lock.json"), []byte(`{"version": 1, "installed": {"hubskill": {"install_path": "hubskill"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	curPruneBuiltins(t, h)
	scope, env, _ := curatorScope(ctx)
	if ok, _ := scope.IsCurationEligible("hubskill", ""); ok {
		t.Fatal("hub skill eligible")
	}
	if ok, _, _ := ArchiveSkill(ctx, env, "hubskill"); ok {
		t.Fatal("hub skill archived")
	}
	if _, err := os.Stat(filepath.Join(dir, "hubskill")); err != nil {
		t.Fatal(err)
	}
}

func TestCuratorStateAtomicWriteNoTmpLeftovers(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	st := pyjson.NewObject()
	st.Set("paused", true)
	SaveCuratorState(ctx, st)
	entries, err := os.ReadDir(filepath.Dir(curatorStateFile(ctx)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover %s", e.Name())
		}
	}
}

func TestCuratorCLIPinRefusesBundledSkill(t *testing.T) {
	ctx, h := curTestHome(t, nil)
	dir := filepath.Join(h, "skills")
	curWriteSkill(t, dir, "ship-skill", "")
	if err := os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("ship-skill:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := curRunCLI(t, ctx, "pin", CuratorArgs{Skill: "ship-skill"}); code != 1 {
		t.Fatalf("rc %d: %s", code, out)
	}
}

func TestCuratorReviewRuntimeAuxiliaryCredentials(t *testing.T) {
	curTestHome(t, nil)
	main := map[string]any{"provider": "openrouter", "default": "openai/gpt-5.5"}
	for _, tc := range []struct {
		name                       string
		cfg                        map[string]any
		provider, model, key, base string
	}{
		{"passes curator slot credentials", map[string]any{"model": main, "auxiliary": map[string]any{"curator": map[string]any{"provider": "custom", "model": "local-mini", "api_key": "sk-curator-only", "base_url": "http://localhost:11434/v1"}}}, "custom", "local-mini", "sk-curator-only", "http://localhost:11434/v1"},
		{"strips blank credentials", map[string]any{"model": main, "auxiliary": map[string]any{"curator": map[string]any{"provider": "openrouter", "model": "x/y", "api_key": "   ", "base_url": ""}}}, "openrouter", "x/y", "", ""},
		{"ignores slot credentials on main", map[string]any{"model": main, "auxiliary": map[string]any{"curator": map[string]any{"provider": "auto", "model": "", "api_key": "must-not-leak", "base_url": "http://curator-slot-ignored/"}}}, "openrouter", "openai/gpt-5.5", "", ""},
		{"legacy auxiliary carries credentials", map[string]any{"model": main, "curator": map[string]any{"auxiliary": map[string]any{"provider": "custom", "model": "m", "api_key": "legacy-key", "base_url": "http://legacy/v1"}}}, "custom", "m", "legacy-key", "http://legacy/v1"},
		{"provider only falls back", map[string]any{"model": main, "auxiliary": map[string]any{"curator": map[string]any{"provider": "openrouter", "model": ""}}}, "openrouter", "openai/gpt-5.5", "", ""},
		{"model only falls back", map[string]any{"model": main, "auxiliary": map[string]any{"curator": map[string]any{"provider": "auto", "model": "gpt-5.4-mini"}}}, "openrouter", "openai/gpt-5.5", "", ""},
	} {
		b, err := curatorResolveReviewRuntime(tc.cfg)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if b.provider != tc.provider || b.model != tc.model || b.apiKey != tc.key || b.baseURL != tc.base {
			t.Fatalf("%s: %+v", tc.name, b)
		}
	}
}
