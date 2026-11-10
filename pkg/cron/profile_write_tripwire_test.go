package cron

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
)

type tripwireEntry struct {
	kind  string
	mtime int64
	size  int64
}

// Snapshot of the default profile tree and the active testprof home
type profileTripwire struct {
	root, profile string
	baseline      map[string]tripwireEntry
}

// Every path under the default root except the profiles subtree, files with their mtime and size
func (w *profileTripwire) snapshot(t *testing.T) map[string]tripwireEntry {
	t.Helper()
	snap := map[string]tripwireEntry{}
	profiles := filepath.Join(w.root, "profiles")
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == w.root {
			return nil
		}
		if path == profiles {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(w.root, path)
		if d.IsDir() {
			snap[rel] = tripwireEntry{kind: "dir"}
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		snap[rel] = tripwireEntry{kind: "file", mtime: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func (w *profileTripwire) assertDefaultUntouched(t *testing.T) {
	t.Helper()
	now := w.snapshot(t)
	var created, deleted, modified []string
	for rel, entry := range now {
		before, existed := w.baseline[rel]
		if !existed {
			created = append(created, rel)
		} else if before != entry {
			modified = append(modified, rel)
		}
	}
	for rel := range w.baseline {
		if _, still := now[rel]; !still {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(created)
	sort.Strings(deleted)
	sort.Strings(modified)
	var problems []string
	if len(created) > 0 {
		problems = append(problems, "created in default profile: "+strings.Join(created, ", "))
	}
	if len(modified) > 0 {
		problems = append(problems, "modified in default profile: "+strings.Join(modified, ", "))
	}
	if len(deleted) > 0 {
		problems = append(problems, "deleted from default profile: "+strings.Join(deleted, ", "))
	}
	if len(problems) > 0 {
		t.Fatalf("Cross-profile write leak: the testprof profile was active but the default profile tree at %s changed: %s", w.root, strings.Join(problems, "; "))
	}
}

// Activates <root>/profiles/testprof over a default tree seeded like a real install and arms the tripwire
func armProfileTripwire(t *testing.T) (*profileTripwire, context.Context) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath")
	profile := filepath.Join(root, "profiles", "testprof")
	seed := map[string]string{
		"config.yaml": "model:\n  default: default-model\n", filepath.Join("memories", "MEMORY.md"): "default profile memory\n",
		filepath.Join("cron", "jobs.json"): "[]\n", ".env": "",
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range seed {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", profile)
	w := &profileTripwire{root: root, profile: profile}
	w.baseline = w.snapshot(t)
	t.Cleanup(func() { w.assertDefaultUntouched(t) })
	return w, home.WithOverride(context.Background(), profile)
}

func exerciseSessionDB(t *testing.T, w *profileTripwire, ctx context.Context) {
	db, err := state.OpenHome(home.Dir(ctx), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "20260823_000000_tripwire", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(ctx, "20260823_000000_tripwire", map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.profile, "state.db")); err != nil {
		t.Fatalf("the session database did not write to the active profile home: %v", err)
	}
}

func exerciseConfigSave(t *testing.T, w *profileTripwire, ctx context.Context) {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg["model"] = "testprof-model"
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.profile, "config.yaml")); err != nil {
		t.Fatalf("the config save did not write the active profile's config.yaml: %v", err)
	}
}

func exerciseMemoryStore(t *testing.T, w *profileTripwire, ctx context.Context) {
	store := memory.NewStore(home.MemoriesDir(home.Dir(ctx)), memory.DefaultMemoryCharLimit, memory.DefaultUserCharLimit, true, true)
	if err := store.LoadFromDisk(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := store.Add(ctx, "memory", "testprof remembers something")
	if err != nil || !result.Success() {
		t.Fatalf("memory add failed: %v %v", result.Data, err)
	}
	raw, err := os.ReadFile(filepath.Join(w.profile, "memories", "MEMORY.md"))
	if err != nil || !strings.Contains(string(raw), "testprof remembers something") {
		t.Fatalf("the memory store did not persist to the active profile's memories: %q %v", raw, err)
	}
}

func exerciseCronStore(t *testing.T, w *profileTripwire, ctx context.Context) {
	s := cronStoreFrom(ctx)
	job := &Job{ID: "tripwire-job", Name: "tripwire", Prompt: "noop", Enabled: true,
		Schedule: &Schedule{Kind: "interval", Minutes: 60, HasMinutes: true, Display: "every 60m"}}
	if err := s.SaveJobs([]*Job{job}, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.profile, "cron", "jobs.json")); err != nil {
		t.Fatalf("the jobs save did not write the active profile's cron/jobs.json: %v", err)
	}
	loaded, err := cronStoreFrom(ctx).LoadJobs()
	if err != nil || !reflect.DeepEqual(jobIDs(loaded), []string{"tripwire-job"}) {
		t.Fatalf("the jobs load did not read back from the active profile's store: %v %v", jobIDs(loaded), err)
	}
	if raw, err := os.ReadFile(filepath.Join(w.root, "cron", "jobs.json")); err != nil || string(raw) != "[]\n" {
		t.Fatalf("the default profile's jobs.json changed: %q %v", raw, err)
	}
}

func TestNoDefaultProfileWrites(t *testing.T) {
	surfaces := []struct {
		name     string
		exercise func(*testing.T, *profileTripwire, context.Context)
	}{
		{"session_db", exerciseSessionDB}, {"config_save", exerciseConfigSave},
		{"memory_store", exerciseMemoryStore}, {"cron_store", exerciseCronStore},
	}
	for _, surface := range surfaces {
		t.Run(surface.name, func(t *testing.T) {
			w, ctx := armProfileTripwire(t)
			surface.exercise(t, w, ctx)
			w.assertDefaultUntouched(t)
		})
	}
}
