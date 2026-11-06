package home

import (
	"path/filepath"
	"testing"
)

func TestProfileCLISelectorPinsOnlyNamedProfiles(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(PatchUserHome(tmp))
	root := filepath.Join(tmp, dirName)
	t.Setenv(EnvHome, filepath.Join(root, ProfilesDir, "research"))
	cases := map[string]string{
		filepath.Join(root, ProfilesDir, "research"): "-p research ",
		filepath.Join(root, ProfilesDir, "default"):  "",
		root:                            "",
		filepath.Join(tmp, "elsewhere"): "",
		"":                              "",
	}
	for h, want := range cases {
		if got := ProfileCLISelector(h); got != want {
			t.Errorf("ProfileCLISelector(%q) = %q, want %q", h, got, want)
		}
	}
}
