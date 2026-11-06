package manage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Options of a profile create, nil pointers standing for arguments the caller did not pass
type CreateOptions struct {
	CloneFrom     *string
	CloneAll      bool
	CloneConfig   bool
	NoSkills      bool
	Description   *string
	CloneChannels bool
	SyncImports   bool
	Soul          *string
}

// Directory a clone copies from, the named profile or the active home when no name is given
func resolveCloneSource(ctx context.Context, cloneFrom *string) (string, error) {
	if err := config.RequireFileTooling(ctx, "Profile clone"); err != nil {
		return "", err
	}
	label := "active"
	var sourceDir string
	if cloneFrom == nil {
		sourceDir = home.Dir(ctx)
	} else {
		canon, err := profiles.CanonValid(*cloneFrom)
		if err != nil {
			return "", err
		}
		if canon != "" {
			label = canon
		}
		if sourceDir, err = profiles.GetProfileDir(canon); err != nil {
			return "", err
		}
	}
	if st, err := os.Stat(sourceDir); err != nil || !st.IsDir() {
		return "", &profiles.NotFoundError{Msg: fmt.Sprintf("Source profile '%s' does not exist at %s", label, sourceDir)}
	}
	return sourceDir, nil
}

// Removes a file like Path.unlink with missing_ok, refusing a directory
func unlinkMissingOK(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if st.IsDir() {
		return &os.PathError{Op: "unlink", Path: path, Err: syscall.EISDIR}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Full clone minus infrastructure and history, then strips runtime files, single use grants and the vault key
func cloneAllInto(sourceDir, profileDir, canon string) (map[string]any, error) {
	if err := copytreeKeepJunctions(sourceDir, profileDir, cloneAllCopytreeIgnore(sourceDir), false); err != nil {
		return nil, err
	}
	materialized, err := materializeSymlinkedFiles(profileDir)
	if err != nil {
		return nil, err
	}
	if len(materialized) > 0 {
		slog.Default().Info(fmt.Sprintf("profile %s: materialized symlinked %s so the clone never writes through to %s",
			canon, pyrepr.Repr(materialized), sourceDir))
	}
	for _, sub := range profiles.ProfileDirs {
		if err := os.MkdirAll(filepath.Join(profileDir, sub), 0o777); err != nil {
			return nil, err
		}
	}
	for _, stale := range profiles.CloneAllStrip {
		if err := unlinkMissingOK(filepath.Join(profileDir, filepath.FromSlash(stale))); err != nil {
			return nil, err
		}
	}
	stripped := provider.StripClonedSingleUseOAuthGrants(profileDir)
	if len(stripped.Pool)+len(stripped.Providers)+len(stripped.Files) > 0 {
		slog.Default().Info(fmt.Sprintf("profile %s: dropped cloned single-use OAuth grants {'pool': %s, 'providers': %s, 'files': %s} (inherits the root grant instead)",
			canon, pyrepr.Repr(stripped.Pool), pyrepr.Repr(stripped.Providers), pyrepr.Repr(stripped.Files)))
	}
	ownerDir, err := profiles.GetProfileDir(canon)
	if err != nil {
		return nil, err
	}
	ref, err := vaultOwnCopiedKey(filepath.Join(profileDir, "vault"), filepath.Join(ownerDir, "vault"))
	if err != nil {
		return nil, &profiles.ValueError{Msg: fmt.Sprintf("Cannot copy the vault from %s: %v", sourceDir, err)}
	}
	return ref, nil
}

// Fresh layout, seeding a model block without a source or cloning config, skills, plugins and identity files
func bootstrapProfileDir(ctx context.Context, profileDir, sourceDir string, syncImports bool) error {
	if err := os.MkdirAll(profileDir, 0o777); err != nil {
		return err
	}
	for _, sub := range profiles.ProfileDirs {
		if err := os.MkdirAll(filepath.Join(profileDir, sub), 0o777); err != nil {
			return err
		}
	}
	if sourceDir == "" {
		return seedModelConfig(ctx, profileDir)
	}
	for _, rel := range profiles.CloneConfigFiles {
		if err := cloneFile(sourceDir, profileDir, rel); err != nil {
			return err
		}
	}
	if err := stripMultiplexFlag(ctx, filepath.Join(profileDir, "config.yaml")); err != nil {
		return err
	}
	sourceSkills := filepath.Join(sourceDir, "skills")
	if st, err := os.Stat(sourceSkills); err == nil && st.IsDir() {
		if err := copytreeKeepJunctions(sourceSkills, filepath.Join(profileDir, "skills"), nonExportableEntries, true); err != nil {
			return err
		}
	}
	for _, rel := range profiles.CloneSubdirFiles {
		if err := cloneFile(sourceDir, profileDir, rel); err != nil {
			return err
		}
	}
	if err := clonePlugins(sourceDir, profileDir); err != nil {
		return err
	}
	if _, err := memory.CloneMemoryProviderConfig(sourceDir, profileDir, memory.ExternalMemoryProvider(memory.LoadProfileConfigDict(sourceDir))); err != nil {
		return err
	}
	if syncImports {
		return cloneFile(sourceDir, profileDir, "import-sync.json")
	}
	return nil
}

// Copies the active home's model block into a fresh profile config, never failing creation over the seed
func seedModelConfig(ctx context.Context, profileDir string) error {
	configPath := filepath.Join(profileDir, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || exists {
		return err
	}
	source := filepath.Join(home.Dir(ctx), "config.yaml")
	srcExists, err := config.ConfigExists(ctx, source)
	if err != nil || !srcExists {
		return nil
	}
	raw, err := config.ReadUserConfigRaw(ctx, source)
	if err != nil {
		return nil
	}
	if seed := provider.LaunchModelSeed(ctx, raw); len(seed) > 0 {
		_ = config.AtomicConfigWrite(ctx, profileDir, configPath, seed, "")
	}
	return nil
}

// Creates a profile in a hidden staging sibling published with one rename and returns its directory
func CreateProfile(ctx context.Context, name string, o CreateOptions) (string, error) {
	if o.NoSkills && (o.CloneFrom != nil || o.CloneConfig || o.CloneAll) {
		return "", &profiles.ValueError{Msg: "--no-skills is mutually exclusive with --clone / --clone-from / --clone-all " +
			"(cloning explicitly copies skills from the source profile)."}
	}
	if o.SyncImports && !(o.CloneConfig || o.CloneAll) {
		return "", &profiles.ValueError{Msg: "--sync-imports requires --clone or --clone-from (there is no import " +
			"manifest to carry over without a source profile)."}
	}
	cloning := o.CloneFrom != nil || o.CloneAll || o.CloneConfig
	if o.CloneChannels && !cloning {
		return "", &profiles.ValueError{Msg: "--clone-channels only applies to a clone (--clone, --clone-from or --clone-all)."}
	}
	canon, err := profiles.CanonValid(name)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return "", &profiles.ValueError{Msg: "Cannot create a profile named 'default' — it is the built-in profile (~/.icebath)."}
	}
	profileDir, err := profiles.GetProfileDir(canon)
	if err != nil {
		return "", err
	}
	if pathExists(profileDir) && !home.ProfileHasIdentity(profileDir) {
		if !home.ProfileDeleted(profileDir) {
			return "", &profiles.ExistsError{Msg: fmt.Sprintf("Cannot create profile '%s': %s exists but carries no profile identity "+
				"file, so it is not listed as a profile. Move or remove that directory first.", canon, profileDir)}
		}
		zero := time.Duration(0)
		unowned, err := WaitProfileUnowned(profileDir, &zero)
		if err != nil {
			return "", err
		}
		if !unowned {
			return "", &profiles.ExistsError{Msg: fmt.Sprintf("Cannot create profile '%s': a running gateway still owns %s", canon, profileDir)}
		}
		if err := pyfs.RmTree(profileDir, nil); err != nil {
			return "", err
		}
	}
	if pathExists(profileDir) {
		return "", profiles.ProfileExistsError(canon)
	}
	sourceDir := ""
	if cloning {
		if sourceDir, err = resolveCloneSource(ctx, o.CloneFrom); err != nil {
			return "", err
		}
	}
	if sourceDir != "" && o.CloneChannels {
		label := ""
		if o.CloneFrom != nil {
			label = *o.CloneFrom
		}
		if label == "" {
			label = profiles.GetActiveProfileName(ctx)
		}
		if label == "" {
			label = "default"
		}
		refusal, err := CloneChannelsRefusal(ctx, sourceDir, label)
		if err != nil {
			return "", err
		}
		if refusal != "" {
			return "", &profiles.ValueError{Msg: refusal}
		}
	}
	if err := home.ClearProfileDeleted(profileDir); err != nil {
		return "", err
	}
	staging, err := cloneStagingDir(profileDir)
	if err != nil {
		return "", err
	}
	var cloneKey map[string]any
	published := false
	defer func() {
		if !published {
			discardUnpublishedVaultKey(cloneKey)
			os.RemoveAll(staging)
		}
	}()
	if o.CloneAll && sourceDir != "" {
		if cloneKey, err = cloneAllInto(sourceDir, staging, canon); err != nil {
			return "", err
		}
	} else if err := bootstrapProfileDir(ctx, staging, sourceDir, o.SyncImports); err != nil {
		return "", err
	}
	if sourceDir != "" && !o.CloneChannels {
		stripped, err := stripChannelSettingsFn(ctx, staging, o.CloneAll, sourceDir)
		if err != nil {
			return "", err
		}
		if stripped.Len() > 0 {
			slog.Default().Info(fmt.Sprintf("profile %s: cloned without messaging channels %s", canon, pyrepr.Repr(stripped)))
		}
	}
	if err := finishProfileLayout(ctx, staging, o.NoSkills, o.CloneAll, o.Description, o.Soul); err != nil {
		return "", err
	}
	if err := os.Rename(staging, profileDir); err != nil {
		return "", err
	}
	published = true
	maybeRegisterGatewayService(canon)
	notifyMultiplexer(canon)
	return profileDir, nil
}

// Reports whether a path exists following symlinks, like Path.exists
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Asks a live default multiplexer to serve or unroute a profile now instead of at its next rescan
var notifyMultiplexer = func(canon string) { gatewayNotifyProfilesChanged(canon) }

// Settles a deleted profile's durable identity, false when the settlement is still pending
func purgeIdentity(ctx context.Context, canon string) bool {
	return purgeProfileIdentity(ctx, canon, liveDefaultMultiplexer())
}

// Reports whether a live default gateway recorded a served profile set
var liveDefaultMultiplexer = func() bool {
	_, recorded, err := gatewayRecordedServedProfiles()
	return err == nil && recorded
}

// Seeds bundled skills into a profile home, nil with a warning when the sync fails or runs past 60 seconds
func SeedProfileSkills(ctx context.Context, profileDir string, quiet bool) map[string]any {
	sctx, cancel := context.WithTimeout(home.WithOverride(ctx, profileDir), 60*time.Second)
	defer cancel()
	result, err := skills.SyncSkills(sctx, true)
	if err == nil {
		return result
	}
	if !quiet {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(stdout, "⚠ Skill seeding timed out (60s)")
		} else {
			fmt.Fprintf(stdout, "⚠ Skill seeding failed: %v\n", err)
		}
	}
	return nil
}

// Seeds the placeholder .env into every named profile lacking one, never copying the default's secrets
func BackfillProfileEnvs(quiet bool) ([]string, error) {
	dirs, err := profiles.IterNamedProfileDirs(true)
	if err != nil {
		return nil, err
	}
	backfilled := []string{}
	for _, entry := range dirs {
		envPath := filepath.Join(entry, ".env")
		if pathExists(envPath) {
			continue
		}
		err := os.WriteFile(envPath, []byte(profiles.PlaceholderEnv), 0o666)
		if err == nil {
			err = os.Chmod(envPath, 0o600)
		}
		if err != nil {
			if !quiet {
				fmt.Fprintf(stdout, "⚠ Could not seed .env for profile '%s': %s\n", filepath.Base(entry), pyfs.OSErrorText(err))
			}
			continue
		}
		backfilled = append(backfilled, filepath.Base(entry))
	}
	return backfilled, nil
}
