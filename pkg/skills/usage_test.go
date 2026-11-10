package skills

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Scope over a temp home with an empty skills dir and prune_builtins off
func usageHome(t *testing.T) (*Scope, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(filepath.Join(h, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg["curator"] = map[string]any{"prune_builtins": false}
	return NewScope(h, cfg), filepath.Join(h, "skills")
}

func writeUsageSkill(t *testing.T, skillsDir, name, category string) string {
	t.Helper()
	d := filepath.Join(skillsDir, category, name)
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: test skill\n---\n\n# body\n"), 0o644)
	return d
}

// Lifecycle events recorded through a registered dispatch for the test duration
func captureLifecycle(t *testing.T) *[]map[string]any {
	t.Helper()
	var mu sync.Mutex
	events := &[]map[string]any{}
	RegisterLifecycleDispatch(LifecycleDispatch{
		HasHook: func(context.Context, string) bool { return true },
		InvokeHook: func(_ context.Context, _ string, kw map[string]any) []any {
			mu.Lock()
			*events = append(*events, kw)
			mu.Unlock()
			return nil
		},
	})
	t.Cleanup(func() {
		lifecycleMu.Lock()
		lifecycleDispatch = nil
		lifecycleMu.Unlock()
	})
	return events
}

func recGet(t *testing.T, s *Scope, name, key string) any {
	t.Helper()
	rec, err := s.GetRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := rec.Get(key)
	return v
}

func usageMap(t *testing.T, s *Scope) *pyjson.Object {
	t.Helper()
	u, err := s.LoadUsage()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUsageSaveLoadRoundTripAndCorruptFile(t *testing.T) {
	s, dir := usageHome(t)
	data := pyjson.NewObject()
	rec := pyjson.NewObject()
	rec.Set("use_count", int64(3))
	data.Set("a", rec)
	if !s.SaveUsage(context.Background(), data) {
		t.Fatal("save failed")
	}
	if got := recGet(t, s, "a", "use_count"); got != int64(3) {
		t.Fatalf("round trip %v", got)
	}
	r, _ := s.GetRecord("missing")
	if v, _ := r.Get("use_count"); v != int64(0) {
		t.Fatal("missing record not empty")
	}
	if v, _ := r.Get("state"); v != StateActive {
		t.Fatal("missing record state")
	}
	os.WriteFile(filepath.Join(dir, ".usage.json"), []byte("{not json"), 0o644)
	if u := usageMap(t, s); u.Len() != 0 {
		t.Fatal("corrupt file not empty")
	}
}

func TestBumpViewIncrementsAndBumpsStayIsolated(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	s.BumpView(ctx, "my-skill")
	s.BumpView(ctx, "my-skill")
	if recGet(t, s, "my-skill", "view_count") != int64(2) || recGet(t, s, "my-skill", "last_viewed_at") == nil {
		t.Fatal("view bump")
	}
	s.BumpView(ctx, "skill-a")
	s.BumpUse(ctx, "skill-b", "", "")
	s.BumpView(ctx, "skill-a")
	if recGet(t, s, "skill-a", "view_count") != int64(2) || recGet(t, s, "skill-a", "use_count") != int64(0) || recGet(t, s, "skill-b", "use_count") != int64(1) {
		t.Fatal("bumps corrupted other skills")
	}
}

func TestSkillReuseAndPostPatchReuseDerivedAtomically(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	events := captureLifecycle(t)
	s.RecordCreated(ctx, "private-skill-name", true, "task", "")
	s.BumpUse(ctx, "private-skill-name", "task", "")
	s.BumpUse(ctx, "private-skill-name", "task", "")
	s.BumpPatch(ctx, "private-skill-name", "patch", "task", "")
	s.BumpUse(ctx, "private-skill-name", "task", "")
	s.BumpUse(ctx, "private-skill-name", "task", "")
	var reused, rap []any
	for _, e := range *events {
		if e["action"] == "loaded" {
			reused = append(reused, e["reused"])
			rap = append(rap, e["reuse_after_patch"])
			if e["provenance"] != "agent_created" {
				t.Fatalf("provenance %v", e["provenance"])
			}
		}
	}
	if !sameAnys(reused, false, true, true, true) || !sameAnys(rap, false, false, true, false) {
		t.Fatalf("reused %v rap %v", reused, rap)
	}
	if recGet(t, s, "private-skill-name", "use_count") != int64(4) || recGet(t, s, "private-skill-name", "patch_generation") != int64(1) ||
		recGet(t, s, "private-skill-name", "last_reused_patch_generation") != int64(1) {
		t.Fatal("record counters")
	}
}

func sameAnys(got []any, want ...any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSkillStateEventsEmitOnlyForRealTransitions(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	events := captureLifecycle(t)
	s.RecordCreated(ctx, "my-skill", true, "", "")
	for _, st := range []string{StateStale, StateStale, StateArchived, StateArchived, StateActive, StateActive} {
		s.SetState(ctx, "my-skill", st)
	}
	var actions []any
	for _, e := range *events {
		actions = append(actions, e["action"])
	}
	if !sameAnys(actions, "created", "stale", "archived", "restored") {
		t.Fatalf("actions %v", actions)
	}
}

func TestSkillEventNotEmittedWhenUsageCannotCommit(t *testing.T) {
	s, dir := usageHome(t)
	events := captureLifecycle(t)
	os.Mkdir(filepath.Join(dir, ".usage.json"), 0o755)
	s.BumpUse(context.Background(), "private-skill-name", "", "")
	if len(*events) != 0 {
		t.Fatalf("events %v", *events)
	}
}

func TestInstalledLifecycleUsesPersistedProvenance(t *testing.T) {
	s, _ := usageHome(t)
	events := captureLifecycle(t)
	s.RecordInstalled(context.Background(), "private-installed-skill")
	if len(*events) != 1 || (*events)[0]["action"] != "installed" || (*events)[0]["provenance"] != "installed" {
		t.Fatalf("events %v", *events)
	}
}

func TestCreatedSkillDoesNotInheritStaleIdentity(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	events := captureLifecycle(t)
	rec := pyjson.NewObject()
	for _, kv := range [][2]any{{"created_by", "agent"}, {"use_count", int64(11)}, {"patch_count", int64(4)}, {"patch_generation", int64(4)},
		{"last_reused_patch_generation", int64(3)}, {"pinned", true}, {"state", StateArchived}} {
		rec.Set(kv[0].(string), kv[1])
	}
	data := pyjson.NewObject()
	data.Set("recreated", rec)
	s.SaveUsage(ctx, data)
	s.RecordCreated(ctx, "recreated", false, "", "")
	s.BumpUse(ctx, "recreated", "", "")
	want := map[string]any{"created_by": "learn", "use_count": int64(1), "patch_count": int64(0), "patch_generation": int64(0),
		"last_reused_patch_generation": int64(0), "pinned": false, "state": StateActive}
	for k, v := range want {
		if got := recGet(t, s, "recreated", k); got != v {
			t.Fatalf("%s = %v", k, got)
		}
	}
	var provs []any
	for _, e := range *events {
		provs = append(provs, e["provenance"])
	}
	last := (*events)[len(*events)-1]
	if !sameAnys(provs, "local", "local") || last["reused"] != false || last["reuse_after_patch"] != false {
		t.Fatalf("events %v", *events)
	}
}

func TestMalformedUsageCountersRecoverWithoutLosingPatchReuse(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	events := captureLifecycle(t)
	rec := pyjson.NewObject()
	rec.Set("view_count", "not-a-number")
	rec.Set("use_count", "not-a-number")
	rec.Set("patch_generation", int64(1))
	rec.Set("last_reused_patch_generation", int64(999))
	data := pyjson.NewObject()
	data.Set("damaged", rec)
	s.SaveUsage(ctx, data)
	s.BumpView(ctx, "damaged")
	s.BumpUse(ctx, "damaged", "", "")
	s.BumpPatch(ctx, "damaged", "patch", "", "")
	s.BumpUse(ctx, "damaged", "", "")
	if recGet(t, s, "damaged", "view_count") != int64(1) || recGet(t, s, "damaged", "use_count") != int64(2) ||
		recGet(t, s, "damaged", "patch_generation") != int64(2) || recGet(t, s, "damaged", "last_reused_patch_generation") != int64(2) {
		t.Fatal("counters")
	}
	var reused, rap []any
	for _, e := range *events {
		if e["action"] == "loaded" {
			reused = append(reused, e["reused"])
			rap = append(rap, e["reuse_after_patch"])
		}
	}
	if !sameAnys(reused, false, true) || !sameAnys(rap, false, true) {
		t.Fatalf("reused %v rap %v", reused, rap)
	}
}

func TestConcurrentBumpViewPreservesAllUpdates(t *testing.T) {
	s, _ := usageHome(t)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own := NewScope(s.Home, s.Config)
			for range 25 {
				own.BumpView(context.Background(), "shared-skill")
			}
		}()
	}
	wg.Wait()
	if got := recGet(t, s, "shared-skill", "view_count"); got != int64(150) {
		t.Fatalf("view_count %v", got)
	}
}

func TestRestoringFromArchiveClearsTimestampAndForget(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	s.SetState(ctx, "x", StateArchived)
	if recGet(t, s, "x", "archived_at") == nil {
		t.Fatal("archived_at unset")
	}
	s.SetState(ctx, "x", StateActive)
	if recGet(t, s, "x", "archived_at") != nil {
		t.Fatal("archived_at kept")
	}
	s.Forget(ctx, "x")
	if _, has := usageMap(t, s).Get("x"); has {
		t.Fatal("forget kept record")
	}
}

func TestAgentCreatedExcludesBundledAndHub(t *testing.T) {
	s, dir := usageHome(t)
	ctx := context.Background()
	writeUsageSkill(t, dir, "bundled-skill", "github")
	writeUsageSkill(t, dir, "my-skill", "")
	s.MarkAgentCreated(ctx, "my-skill")
	os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-skill:abc123\n"), 0o644)
	names, err := s.ListAgentCreatedSkillNames()
	if err != nil || len(names) != 1 || names[0] != "my-skill" {
		t.Fatalf("names %v %v", names, err)
	}
	os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled:abc\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".hub"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hub", "lock.json"), []byte(`{"installed": {"hubbed": {}}}`), 0o644)
	for name, want := range map[string]bool{"my-skill": true, "bundled": false, "hubbed": false} {
		if got, err := s.IsAgentCreated(name); got != want || err != nil {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}

func TestEndToEndTelemetryTrackedButLifecycleRefused(t *testing.T) {
	s, dir := usageHome(t)
	ctx := context.Background()
	for _, n := range []string{"bundled-one", "hub-one", "mine"} {
		writeUsageSkill(t, dir, n, "")
	}
	os.WriteFile(filepath.Join(dir, ".bundled_manifest"), []byte("bundled-one:abc\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".hub"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hub", "lock.json"), []byte(`{"installed": {"hub-one": {}}}`), 0o644)
	for _, n := range []string{"bundled-one", "hub-one"} {
		s.BumpView(ctx, n)
		s.BumpUse(ctx, n, "", "")
		s.BumpPatch(ctx, n, "patch", "", "")
		s.SetState(ctx, n, StateStale)
		s.SetState(ctx, n, StateArchived)
		s.SetPinned(ctx, n, true)
		if ok, _, _ := s.ArchiveSkill(ctx, n); ok {
			t.Fatalf("archive %s should refuse", n)
		}
	}
	data := usageMap(t, s)
	for _, n := range []string{"bundled-one", "hub-one"} {
		raw, has := data.Get(n)
		if !has {
			t.Fatalf("%s telemetry missing", n)
		}
		rec := raw.(*pyjson.Object)
		for k, v := range map[string]any{"view_count": int64(1), "use_count": int64(1), "patch_count": int64(1), "state": StateActive, "archived_at": nil, "pinned": false} {
			if got, _ := rec.Get(k); got != v {
				t.Fatalf("%s %s = %v", n, k, got)
			}
		}
		if cb, _ := rec.Get("created_by"); cb == "agent" {
			t.Fatal("created_by agent")
		}
		if !isFile(filepath.Join(dir, n, "SKILL.md")) {
			t.Fatal("dir moved")
		}
	}
	s.BumpView(ctx, "mine")
	if recGet(t, s, "mine", "view_count") != int64(1) {
		t.Fatal("mine view")
	}
}

func TestAdoptPreservesInactivityClockAndRefusesForeignSkills(t *testing.T) {
	s, dir := usageHome(t)
	ctx := context.Background()
	writeUsageSkill(t, dir, "legacy", "")
	os.WriteFile(filepath.Join(dir, ".usage.json"), []byte(`{"legacy": {"use_count": 5, "patch_count": 7, "last_used_at": "2026-04-29T00:00:00+00:00", "created_at": "2026-04-28T00:00:00+00:00"}}`), 0o644)
	r, _ := s.GetRecord("legacy")
	before := LatestActivityAt(r)
	if ok, msg, err := s.AdoptSkill(ctx, "legacy"); !ok || err != nil {
		t.Fatal(msg, err)
	}
	r, _ = s.GetRecord("legacy")
	if LatestActivityAt(r) != before || recGet(t, s, "legacy", "use_count") != int64(5) || recGet(t, s, "legacy", "patch_count") != int64(7) {
		t.Fatal("adopt reset history")
	}
	if ok, _, _ := s.AdoptSkill(ctx, ""); ok {
		t.Fatal("empty name adopted")
	}
	for _, kind := range []string{"bundled", "hub", "protected", "missing"} {
		ks, kdir := usageHome(t)
		ks.Config["curator"] = map[string]any{"prune_builtins": true}
		name := "no-such-skill"
		switch kind {
		case "bundled":
			name = "bundled-one"
			writeUsageSkill(t, kdir, name, "")
			os.WriteFile(filepath.Join(kdir, ".bundled_manifest"), []byte(name+":abc\n"), 0o644)
		case "hub":
			name = "hub-one"
			writeUsageSkill(t, kdir, name, "")
			os.MkdirAll(filepath.Join(kdir, ".hub"), 0o755)
			os.WriteFile(filepath.Join(kdir, ".hub", "lock.json"), []byte(`{"installed": {"hub-one": {}}}`), 0o644)
		case "protected":
			name = "sentinel-protected-skill"
			ProtectedBuiltinSkills[name] = true
			writeUsageSkill(t, kdir, name, "")
		}
		ok, _, _ := ks.AdoptSkill(ctx, name)
		delete(ProtectedBuiltinSkills, name)
		if ok || recGet(t, ks, name, "created_by") == "agent" {
			t.Fatalf("%s adopted", kind)
		}
	}
}

func TestForegroundCreateStampsLearnProvenance(t *testing.T) {
	s, _ := usageHome(t)
	ctx := context.Background()
	s.RecordCreated(ctx, "taught-skill", false, "", "")
	if recGet(t, s, "taught-skill", "created_by") != "learn" {
		t.Fatal("learn stamp")
	}
	if m, _ := s.IsCuratorManaged("taught-skill"); m {
		t.Fatal("learn skill managed")
	}
	s.RecordCreated(ctx, "agent-skill", true, "", "")
	if m, _ := s.IsCuratorManaged("agent-skill"); recGet(t, s, "agent-skill", "created_by") != "agent" || !m {
		t.Fatal("agent stamp")
	}
}

func TestSkillFileLockReentrantInChainAndExclusiveAcrossGoroutines(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".locks", "demo.lock")
	otherDone := make(chan struct{})
	ctx, release, err := SkillFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, inner, err := SkillFileLock(ctx, lockPath)
	if err != nil {
		t.Fatal(err)
	}
	inner()
	go func() {
		_, r, err := SkillFileLock(context.Background(), lockPath)
		if err == nil {
			r()
		}
		close(otherDone)
	}()
	select {
	case <-otherDone:
		t.Fatal("second goroutine acquired a held lock")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second goroutine never acquired")
	}
}

func TestSetLockedRoundTripAndStrictIsLocked(t *testing.T) {
	s, dir := usageHome(t)
	ctx := context.Background()
	os.Mkdir(filepath.Join(dir, "hand-written"), 0o755)
	if l, _ := s.IsLocked("hand-written"); l {
		t.Fatal("locked without sidecar")
	}
	if !s.SetLocked(ctx, "hand-written", true) {
		t.Fatal("set locked")
	}
	names, _ := s.LockedSkillNames()
	if l, _ := s.IsLocked("hand-written"); !l || len(names) != 1 || names[0] != "hand-written" || recGet(t, s, "hand-written", "pinned") != false {
		t.Fatal("lock round trip")
	}
	s.SetLocked(ctx, "hand-written", false)
	names, _ = s.LockedSkillNames()
	if l, _ := s.IsLocked("hand-written"); l || len(names) != 0 {
		t.Fatal("unlock round trip")
	}
	os.WriteFile(filepath.Join(dir, ".usage.json"), []byte("{not json"), 0o644)
	if _, err := s.IsLocked("anything"); err == nil {
		t.Fatal("corrupt sidecar read as unlocked")
	}
	if names, err := s.LockedSkillNames(); err != nil || len(names) != 0 {
		t.Fatal("listing not empty")
	}
	os.WriteFile(filepath.Join(dir, ".usage.json"), []byte(`{"a": "junk", "b": {"locked": "yes"}, "c": {}}`), 0o644)
	for name, want := range map[string]bool{"a": false, "b": true, "c": false, "missing": false} {
		if got, err := s.IsLocked(name); got != want || err != nil {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}

// Scope whose skills.external_dirs mounts a vault skill also symlinked into the local tree
func externalHome(t *testing.T) (*Scope, string) {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(h, "skills", "ext-linked"), 0o755)
	ext := filepath.Join(tmp, "vault")
	os.MkdirAll(filepath.Join(ext, "ext-skill"), 0o755)
	os.WriteFile(filepath.Join(ext, "ext-skill", "SKILL.md"), []byte("---\nname: ext-skill\ndescription: An external skill.\n---\n\n# External\n"), 0o644)
	if err := os.Symlink(filepath.Join(ext, "ext-skill", "SKILL.md"), filepath.Join(h, "skills", "ext-linked", "SKILL.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	cfg := config.Defaults()
	cfg["skills"] = map[string]any{"external_dirs": []any{ext}}
	return NewScope(h, cfg), h
}

func TestProvenanceTierExternal(t *testing.T) {
	s, h := externalHome(t)
	if p, _ := s.Provenance("ext-skill"); p != "external" {
		t.Fatalf("external only %s", p)
	}
	if a, _ := s.IsAgentCreated("ext-skill"); a {
		t.Fatal("external counted as agent created")
	}
	if names := s.ExternalSkillNames(); len(names) != 1 || !names["ext-skill"] {
		t.Fatalf("external names %v", names)
	}
	writeUsageSkill(t, filepath.Join(h, "skills"), "my-local", "")
	if p, _ := s.Provenance("my-local"); p != "agent" {
		t.Fatalf("local %s", p)
	}
	writeUsageSkill(t, filepath.Join(h, "skills"), "ext-skill", "")
	if p, _ := s.Provenance("ext-skill"); p != "agent" {
		t.Fatalf("local copy %s", p)
	}
}

func TestArchiveAndRestoreKeyOnSkillNameNotDirectoryName(t *testing.T) {
	s, skills := usageHome(t)
	ctx := context.Background()
	d := filepath.Join(skills, "training", "accelerate")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\nname: huggingface-accelerate\ndescription: x\n---\n"), 0o644)
	s.MarkAgentCreated(ctx, "huggingface-accelerate")
	if ok, msg, err := s.ArchiveSkill(ctx, "huggingface-accelerate"); !ok || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
	if !lexistsFollow(filepath.Join(skills, ".archive", "huggingface-accelerate", "SKILL.md")) {
		t.Fatal("archive not keyed on the skill name")
	}
	if ok, msg, err := s.RestoreSkill(ctx, "huggingface-accelerate"); !ok || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
	if err := os.Rename(filepath.Join(skills, "huggingface-accelerate"), filepath.Join(skills, ".archive", "accelerate")); err != nil {
		t.Fatal(err)
	}
	if ok, msg, err := s.RestoreSkill(ctx, "huggingface-accelerate"); !ok || err != nil {
		t.Fatalf("%q %v", msg, err)
	}
	if !lexistsFollow(filepath.Join(skills, "huggingface-accelerate", "SKILL.md")) {
		t.Fatal("legacy directory name archive not restored")
	}
	rows, err := s.LedgerListEntries("huggingface-accelerate", -1)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ledger rows %d %v", len(rows), err)
	}
}
