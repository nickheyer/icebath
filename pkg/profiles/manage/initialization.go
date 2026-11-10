package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Fresh hidden staging dir beside the final profile dir, discarding a leftover from a crashed create
func cloneStagingDir(profileDir string) (string, error) {
	staging := filepath.Join(filepath.Dir(profileDir), fmt.Sprintf(".%s.staging-%d", filepath.Base(profileDir), os.Getpid()))
	if err := os.MkdirAll(filepath.Dir(profileDir), 0o777); err != nil {
		return "", err
	}
	if st, err := os.Lstat(staging); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || st.Mode().IsRegular() {
			if err := os.Remove(staging); err != nil {
				return "", err
			}
		} else if st.IsDir() {
			os.RemoveAll(staging)
		}
	}
	return staging, nil
}

// Deletes the keychain entry a failed full clone made for its vault copy, never one of the source
func discardUnpublishedVaultKey(ref map[string]any) {
	if ref != nil {
		vaultDeleteKey(ref)
	}
}

// Seeds the files a fresh profile owns from day one on the staging tree before it is published
func finishProfileLayout(ctx context.Context, profileDir string, noSkills, cloneAll bool, description, soul *string) error {
	if soul != nil {
		if err := os.WriteFile(filepath.Join(profileDir, "SOUL.md"), []byte(*soul), 0o666); err != nil {
			return err
		}
	}
	seedFileIfMissing(filepath.Join(profileDir, ".env"), profiles.PlaceholderEnv, 0o600)
	seedFileIfMissing(filepath.Join(profileDir, "SOUL.md"), config.DefaultIdentity+"\n", 0)
	if noSkills {
		seedFileIfMissing(filepath.Join(profileDir, profiles.NoBundledSkillsMarker),
			"This profile opted out of bundled-skill seeding (`icebath profile create --no-skills`).\n"+
				"Delete this file to re-enable sync on the next `icebath update`.\n", 0)
	}
	if !cloneAll {
		if err := profiles.MigrateProfileConfigIfOutdated(ctx, profileDir); err != nil {
			return err
		}
	}
	if description != nil && pystr.Strip(*description) != "" {
		desc := pystr.Strip(*description)
		auto := false
		profiles.WriteProfileMeta(ctx, profileDir, profiles.MetaUpdate{Description: &desc, DescriptionAuto: &auto})
	}
	return nil
}
