package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Plane fixture with a real profiles root under the user home and the default profile booted
func profilePlane(t *testing.T) *configplane.Plane {
	t.Helper()
	p := remoteplane.Setup(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	p.Home = h
	if _, err := remote.GetRemoteBackend().Version(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return p
}

// The staging home of a fresh profile is never read through the remote backend
func TestFreshProfileCreationSucceedsAgainstAHealthyPlane(t *testing.T) {
	p := profilePlane(t)
	p.Mu.Lock()
	before := len(p.Requests)
	p.Mu.Unlock()
	path, err := CreateProfile(context.Background(), "fresh", CreateOptions{NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		t.Fatalf("profile dir %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, ".env")); err != nil {
		t.Fatal(err)
	}
	if staged, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".fresh.staging-*")); len(staged) != 0 {
		t.Fatalf("staging left %v", staged)
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	for _, r := range p.Requests[before:] {
		if strings.HasPrefix(r.Profile, ".") {
			t.Fatalf("staging home read through the plane %q", r.Profile)
		}
	}
}

// The plane keys settings by profile name and has no rename, so nothing moves
func TestRenameIsRefusedBeforeAnythingMoves(t *testing.T) {
	p := profilePlane(t)
	old := filepath.Join(p.Home, "profiles", "old")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	p.Mu.Lock()
	p.Profile("old").Values, p.Profile("old").Version = map[string]any{"model": map[string]any{"default": "profile-model"}}, 1
	p.Profile("new").Values, p.Profile("new").Version = map[string]any{"model": map[string]any{"default": "target-model"}}, 1
	p.Mu.Unlock()
	_, err := RenameProfile(context.Background(), "old", "new")
	var verr *profiles.ValueError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("rename %v", err)
	}
	if st, err := os.Stat(old); err != nil || !st.IsDir() {
		t.Fatal("old profile moved")
	}
	if _, err := os.Stat(filepath.Join(p.Home, "profiles", "new")); !os.IsNotExist(err) {
		t.Fatal("new profile created")
	}
	p.Mu.Lock()
	oldValues := p.Profile("old").Values
	p.Mu.Unlock()
	if !reflect.DeepEqual(oldValues, map[string]any{"model": map[string]any{"default": "profile-model"}}) {
		t.Fatalf("old values %v", oldValues)
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches", n)
	}
}

// A poll changes a profile's roster model while the ignored local config file stays put
func TestRosterModelFollowsTheRemoteLayerNotTheLocalFile(t *testing.T) {
	p := profilePlane(t)
	named := filepath.Join(p.Home, "profiles", "roster")
	if err := os.MkdirAll(named, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named, "config.yaml"), []byte("model:\n  default: ignored-local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Mu.Lock()
	p.Profile("roster").Values, p.Profile("roster").Version = map[string]any{"model": map[string]any{"default": "model-A", "provider": "nous"}}, 1
	p.Mu.Unlock()
	roster := func() []any {
		infos, err := ListProfiles(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range infos {
			if info.Name == "roster" {
				return []any{info.Model, info.Provider}
			}
		}
		t.Fatal("no roster profile listed")
		return nil
	}
	if got := roster(); !reflect.DeepEqual(got, []any{"model-A", "nous"}) {
		t.Fatalf("roster %v", got)
	}
	p.Mu.Lock()
	p.Profile("roster").Values, p.Profile("roster").Version = map[string]any{"model": map[string]any{"default": "model-B", "provider": "openrouter"}}, 2
	p.Mu.Unlock()
	remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
	if got := roster(); !reflect.DeepEqual(got, []any{"model-B", "openrouter"}) {
		t.Fatalf("roster after poll %v", got)
	}
}
