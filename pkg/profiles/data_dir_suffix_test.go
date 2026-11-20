package profiles

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestSuffixScopesDefaultHomeAndProfiles(t *testing.T) {
	spaced := " spaced "
	if runtime.GOOS == "windows" {
		spaced = " spaced"
	}
	for _, suffix := range []string{"", "-asdfasdf", "magic-test", spaced} {
		t.Run(suffix, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv(home.EnvHome, "")
			os.Unsetenv(home.EnvHome)
			t.Setenv("HOME", tmp)
			t.Setenv("USERPROFILE", tmp)
			t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))
			t.Setenv(home.EnvSuffix, suffix)
			ResetFileCache()
			home.ResetKeyCache()
			root := home.Root()
			profile := filepath.Join(root, "profiles", "coder")
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, "config.yaml"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			resolved, err := ResolveProfileEnv("coder")
			if err != nil {
				t.Fatal(err)
			}
			got := []string{home.Dir(context.Background()), home.Process(), root, ProfilesRoot(), ActiveProfilePath(), resolved}
			t.Setenv(home.EnvHome, profile)
			got = append(got, home.Dir(context.Background()), home.Root())
			os.Unsetenv(home.EnvHome)
			t.Setenv(home.EnvSuffix, suffix+"-changed")
			got = append(got, home.Root())
			base := filepath.Join(tmp, ".icebath")
			if runtime.GOOS == "windows" {
				base = filepath.Join(tmp, "AppData", "Local", "icebath")
			}
			want := base + suffix
			wantProfile := filepath.Join(want, "profiles", "coder")
			expected := []string{want, want, want, filepath.Join(want, "profiles"), filepath.Join(want, "active_profile"), wantProfile,
				wantProfile, want, want + "-changed"}
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("got %q\nwant %q", got, expected)
			}
		})
	}
}
