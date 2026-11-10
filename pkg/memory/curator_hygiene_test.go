package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestCuratorRecentViewActivityPreventsFalseStaleTransition(t *testing.T) {
	ctx, h := curTestHome(t, map[string]any{"stale_after_days": 30, "archive_after_days": 90})
	d := filepath.Join(h, "skills", "recently-viewed")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: recently-viewed\ndescription: test skill\n---\n\n# recently-viewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	data, err := pyjson.Loads(`{"recently-viewed": {"created_at": "` + pydatetime.ISOFormatUTC(now.Add(-60*24*time.Hour)) +
		`", "last_viewed_at": "` + pydatetime.ISOFormatUTC(now.Add(-24*time.Hour)) + `", "view_count": 1, "state": "active"}}`)
	if err != nil {
		t.Fatal(err)
	}
	scope, _, _ := curatorScope(ctx)
	if !scope.SaveUsage(ctx, data.(*pyjson.Object)) {
		t.Fatal("save usage failed")
	}
	counts, err := ApplyCuratorTransitions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if curatorCount(counts, "marked_stale") != 0 || curGet(curRecord(t, ctx, "recently-viewed"), "state") != "active" {
		t.Fatalf("counts %v", counts)
	}
}

func curSnapshotNames(t *testing.T, skillsDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(skillsDir, ".curator_backups"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestCuratorPruneOnlyPassTakesNoSnapshotButAgesOldOnesOut(t *testing.T) {
	ctx, dir := curBackupHome(t, 5)
	if err := os.MkdirAll(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "SKILL.md"), []byte("---\nname: alpha\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, ok, err := SnapshotSkills(ctx, "old", nil); !ok || err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	if n := len(curSnapshotNames(t, dir)); n != 3 {
		t.Fatalf("%d snapshots", n)
	}
	curatorBackupConfig = func(context.Context) map[string]any { return map[string]any{"keep": 1} }
	off, on := false, true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, Consolidate: &off}); err != nil {
		t.Fatal(err)
	}
	if n := len(curSnapshotNames(t, dir)); n != 1 {
		t.Fatalf("prune only pass must apply retention without adding a snapshot, %d left", n)
	}
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Synchronous: true, Consolidate: &on}); err != nil {
		t.Fatal(err)
	}
	backups, err := ListCuratorBackups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, b := range backups {
		r, _ := b.Get("reason")
		s, _ := r.(string)
		reasons = append(reasons, s)
	}
	if !strings.Contains(strings.Join(reasons, ","), "pre-curator-run") || len(reasons) > 2 {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestCuratorOnlyOneProcessClaimsADuePass(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	due := func() {
		st := pyjson.NewObject()
		st.Set("last_run_at", "2020-01-01T00:00:00+00:00")
		SaveCuratorState(ctx, st)
	}
	due()
	started, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	curatorConfig = func(ctx context.Context) map[string]any {
		if curatorExists(curatorRunClaimPath(ctx)) && blocked.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return map[string]any{}
	}
	done := make(chan *pyjson.Object)
	go func() { done <- MaybeRunCurator(ctx, nil, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first pass never claimed")
	}
	second := MaybeRunCurator(ctx, nil, nil)
	close(release)
	first := <-done
	if second != nil {
		t.Fatal("a second launch ran the pass concurrently")
	}
	if first == nil {
		t.Fatal("first pass did not run")
	}
	if curatorExists(curatorRunClaimPath(ctx)) {
		t.Fatal("claim not released after the pass")
	}
	due()
	if MaybeRunCurator(ctx, nil, nil) == nil {
		t.Fatal("the next due pass could not claim again")
	}
}

func curReportInput(started time.Time, counts string, before, after []*pyjson.Object, names map[string]bool, meta *pyjson.Object) curatorRunReportInput {
	c, err := pyjson.Loads(counts)
	if err != nil {
		panic(err)
	}
	return curatorRunReportInput{startedAt: started, elapsed: 12.345, autoCounts: c.(*pyjson.Object), beforeReport: before, beforeNames: names, afterReport: after, llmMeta: meta}
}

func curMeta() *pyjson.Object {
	return curatorEntry("final", "short summary of the pass", "summary", "short summary", "model", "test-model", "provider", "test-provider", "tool_calls", []any{}, "error", nil)
}

func TestCuratorWriteRunReportCreatesBothFiles(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	runDir := curatorWriteRunReport(ctx, curReportInput(time.Now().UTC(), `{"checked": 5, "marked_stale": 1, "archived": 0, "reactivated": 0}`, nil, nil, map[string]bool{}, curMeta()))
	if runDir == "" {
		t.Fatal("no run dir")
	}
	for _, f := range []string{"run.json", "REPORT.md"} {
		if _, err := os.Stat(filepath.Join(runDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if filepath.Dir(runDir) != curatorReportsRoot(ctx) {
		t.Fatalf("run dir %s outside the reports root", runDir)
	}
	var payload map[string]any
	raw, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil || json.Unmarshal(raw, &payload) != nil {
		t.Fatalf("run.json %s %v", raw, err)
	}
}

func TestCuratorSameSecondRerunsGetUniqueDirs(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	in := curReportInput(time.Date(2026, 4, 29, 5, 33, 34, 0, time.UTC), `{"checked": 0, "marked_stale": 0, "archived": 0, "reactivated": 0}`, nil, nil, map[string]bool{}, curMeta())
	a, b := curatorWriteRunReport(ctx, in), curatorWriteRunReport(ctx, in)
	if a == "" || b == "" || a == b || !strings.HasPrefix(filepath.Base(b), filepath.Base(a)) {
		t.Fatalf("dirs %q %q", a, b)
	}
}

func curToolCall(args map[string]any) any {
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return curatorEntry("name", "skill_manage", "arguments", string(raw))
}

func curRows(names ...string) []*pyjson.Object {
	var out []*pyjson.Object
	for i := 0; i < len(names); i += 2 {
		out = append(out, curatorEntry("name", names[i], "state", names[i+1], "pinned", false))
	}
	return out
}

func curRunJSON(t *testing.T, runDir string) (map[string]any, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(runDir, "REPORT.md"))
	if err != nil {
		t.Fatal(err)
	}
	return payload, string(md)
}

func TestCuratorClassifyConsolidatedViaWriteFileEvidence(t *testing.T) {
	curTestHome(t, nil)
	cons, pruned := curatorClassifyRemoved([]string{"axolotl-training"}, nil, map[string]bool{"training-platforms": true, "keeper": true},
		[]any{curToolCall(map[string]any{"action": "write_file", "name": "training-platforms", "file_path": "references/axolotl-training.md", "file_content": "# Axolotl\n..."})})
	if len(cons) != 1 || curGet(cons[0], "name") != "axolotl-training" || curGet(cons[0], "into") != "training-platforms" || len(pruned) != 0 {
		t.Fatalf("consolidated %v pruned %v", cons, pruned)
	}
}

func TestCuratorReportMDSplitsConsolidatedAndPrunedSections(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	before := curRows("absorbed-skill", "active", "dead-skill", "stale", "keeper", "active")
	meta := curatorEntry("final", "Consolidated absorbed-skill into umbrella. Pruned dead-skill.", "summary", "1 consolidated, 1 pruned", "model", "m", "provider", "p", "error", nil,
		"tool_calls", []any{curToolCall(map[string]any{"action": "create", "name": "umbrella", "content": "# umbrella\n\nAbsorbed absorbed-skill."})})
	in := curReportInput(time.Now().UTC(), `{"checked": 3, "marked_stale": 0, "archived": 0, "reactivated": 0}`, before, curRows("keeper", "active", "umbrella", "active"),
		map[string]bool{"absorbed-skill": true, "dead-skill": true, "keeper": true}, meta)
	payload, md := curRunJSON(t, curatorWriteRunReport(ctx, in))
	var consNames []string
	for _, e := range payload["consolidated"].([]any) {
		consNames = append(consNames, e.(map[string]any)["name"].(string))
	}
	pruned, _ := json.Marshal(payload["pruned_names"])
	archived, _ := json.Marshal(payload["archived"])
	for _, e := range payload["pruned"].([]any) {
		if m, ok := e.(map[string]any); !ok || m["name"] == nil {
			t.Fatalf("pruned entry %v", e)
		}
	}
	counts := payload["counts"].(map[string]any)
	if strings.Join(consNames, ",") != "absorbed-skill" || string(pruned) != `["dead-skill"]` || (string(archived) != `["absorbed-skill","dead-skill"]` && string(archived) != `["dead-skill","absorbed-skill"]`) ||
		counts["consolidated_this_run"] != float64(1) || counts["pruned_this_run"] != float64(1) {
		t.Fatalf("payload %v", payload)
	}
	if !strings.Contains(md, "absorbed-skill") || !strings.Contains(md, "dead-skill") {
		t.Fatalf("REPORT.md:\n%s", md)
	}
}

func TestCuratorReconcileModelBlockVisibleInFullReport(t *testing.T) {
	ctx, _ := curTestHome(t, nil)
	final := "Processed 3 clusters. Absorbed anthropic-api into llm-providers.\n\n## Structured summary (required)\n```yaml\nconsolidations:\n  - from: anthropic-api\n    into: llm-providers\n    reason: duplicate content, now a subsection\nprunings:\n  - name: stale-thing\n    reason: pre-curator junk, no overlap with anything\n```\n"
	meta := curatorEntry("final", final, "summary", "1 consolidated, 1 pruned", "model", "m", "provider", "p", "error", nil,
		"tool_calls", []any{curToolCall(map[string]any{"action": "create", "name": "llm-providers", "content": "# llm-providers\nIncludes anthropic-api"})})
	in := curReportInput(time.Now().UTC(), `{"checked": 2, "marked_stale": 0, "archived": 0, "reactivated": 0}`, curRows("anthropic-api", "active", "stale-thing", "stale"),
		curRows("llm-providers", "active"), map[string]bool{"anthropic-api": true, "stale-thing": true}, meta)
	payload, md := curRunJSON(t, curatorWriteRunReport(ctx, in))
	cons := payload["consolidated"].([]any)[0].(map[string]any)
	pruned := payload["pruned"].([]any)[0].(map[string]any)
	if cons["name"] != "anthropic-api" || cons["into"] != "llm-providers" || cons["reason"] != "duplicate content, now a subsection" || cons["source"] != "model+audit" ||
		pruned["name"] != "stale-thing" || pruned["reason"] != "pre-curator junk, no overlap with anything" {
		t.Fatalf("payload %v", payload)
	}
	if !strings.Contains(md, "duplicate content, now a subsection") || !strings.Contains(md, "pre-curator junk") {
		t.Fatalf("REPORT.md:\n%s", md)
	}
}

func TestCuratorThreadedLLMPassKeepsCallersProfileScope(t *testing.T) {
	_, root := curTestHome(t, nil)
	profile := filepath.Join(t.TempDir(), "profiles", "served")
	curWriteSkill(t, filepath.Join(profile, "skills"), "candidate", "")
	type seenScope struct{ home, secret string }
	seen := make(chan seenScope, 1)
	curatorLLMReview = func(ctx context.Context, _ string) *pyjson.Object {
		secret, _, _ := provider.GetSecret(ctx, "CURATOR_PROBE_KEY")
		seen <- seenScope{home.Dir(ctx), secret}
		return curatorLLMMeta("stub", nil)
	}
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := provider.WithSecretScope(home.WithOverride(context.Background(), profile), map[string]string{"CURATOR_PROBE_KEY": "served"}, profile)
	scope, _, _ := curatorScope(ctx)
	scope.MarkAgentCreated(ctx, "candidate")
	on := true
	if _, err := RunCuratorReview(ctx, CuratorRunOptions{Consolidate: &on}); err != nil {
		t.Fatal(err)
	}
	var got seenScope
	select {
	case got = <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("background review pass never ran")
	}
	if got.home != profile || got.secret != "served" {
		t.Fatalf("background pass scope %+v", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := LoadCuratorState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if s, _ := curGet(st, "last_run_summary").(string); strings.HasSuffix(s, "llm: stub") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("profile state never recorded the pass: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", ".curator_state")); !os.IsNotExist(err) {
		t.Fatal("background pass wrote the root home's state")
	}
}
