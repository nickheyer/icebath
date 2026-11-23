package manage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Config sections the voice setup of the launch profile lives in
var voiceSections = []string{"stt", "tts", "voice"}

// Runs one best effort item of the credential mirror, a failure logged and reading as not done, a config outage raised to the step
func mirrorItem(label string, fn func() (bool, error)) bool {
	var done bool
	err := attempt(func() (err error) {
		done, err = fn()
		return err
	})
	if err == nil {
		return done
	}
	if configOutage(err) {
		panic(err)
	}
	pkgLog().Warn(fmt.Sprintf("profile-create %s failed", label), "error", err)
	return false
}

// Copies one launch file into the profile with owner only permissions when it is a file and wanted allows it
func mirrorSecret(path, launchHome, name string, wanted func(src, dst string) bool) (bool, error) {
	src, dst := filepath.Join(launchHome, name), filepath.Join(path, name)
	if !isFile(src) || !wanted(src, dst) {
		return false, nil
	}
	if err := pyfs.Copy2(src, dst); err != nil {
		return false, err
	}
	_ = os.Chmod(dst, 0o600)
	return true, nil
}

// Reports whether the launch .env has keys and the profile's is still the seeded comment only stub, so a clone's own secrets are never replaced
func envOverStub(src, dst string) bool {
	return len(config.LoadEnvFile(src)) > 0 && len(config.LoadEnvFile(dst)) == 0
}

// Copies the launch profile's speech and voice sections the new profile lacks, as the launch file writes them less every setting the config schema retired
func mirrorVoiceSections(ctx context.Context, path string) (bool, error) {
	launchRaw, err := config.ReadUserConfigRaw(ctx, config.GetConfigPath(home.Dir(ctx)))
	if err != nil {
		return false, err
	}
	sections := map[string]any{}
	for _, key := range voiceSections {
		if value := launchRaw[key]; truthy(value) {
			sections[key] = value
		}
	}
	for _, key := range voiceSections {
		if value, copied := sections[key]; copied && !truthy(value) {
			delete(sections, key)
		}
	}
	if len(sections) == 0 {
		return false, nil
	}
	scoped, unlock := provider.LockConfigMutation(provider.ProfileConfigScope(ctx, path, ""))
	defer unlock()
	profileRaw, err := config.ReadUserConfigRaw(scoped, config.GetConfigPath(home.Dir(scoped)))
	if err != nil {
		return false, err
	}
	merged := make(map[string]any, len(profileRaw)+len(sections))
	order := config.MappingKeys(profileRaw)
	for key, value := range profileRaw {
		merged[key] = value
	}
	missing := false
	for _, key := range voiceSections {
		value, wanted := sections[key]
		if _, held := profileRaw[key]; !wanted || held {
			continue
		}
		merged[key] = value
		order = append(order, key)
		missing = true
	}
	if !missing {
		return false, nil
	}
	config.SetMappingKeys(merged, order)
	if err := config.SaveConfig(scoped, home.Dir(scoped), merged, config.SaveOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

// Copies the launch profile's .env, logins and voice sections into a new profile, each item on its own, and returns the providers whose single use logins stayed in the root store
func MirrorLaunchCredentials(ctx context.Context, path string, mirrored *tuigateway.ProfileMirrored, shareAuth, cloneChannels bool) []string {
	launchHome := home.Dir(ctx)
	shareAuth = shareAuth && provider.NewStore(launchHome).GlobalPath() == ""
	var keptRoot []string
	mirrored.Env = mirrorItem(".env copy", func() (bool, error) { return mirrorSecret(path, launchHome, ".env", envOverStub) })
	if mirrored.Env && !cloneChannels {
		mirrorItem("channel strip", func() (bool, error) {
			_, err := StripChannelEnvFile(ctx, filepath.Join(path, ".env"), nil)
			return err == nil, err
		})
	}
	if !shareAuth {
		copied := mirrorItem("auth.json copy", func() (bool, error) {
			return mirrorSecret(path, launchHome, "auth.json", func(_, dst string) bool { return !pathExists(dst) })
		})
		mirrored.Auth = tuigateway.MirroredValue{Done: copied}
		if copied {
			mirrorItem("OAuth grant strip", func() (bool, error) {
				stripped := provider.StripClonedSingleUseOAuthGrants(path)
				seen := map[string]bool{}
				for _, name := range append(append([]string{}, stripped.Pool...), stripped.Providers...) {
					if !seen[name] {
						seen[name] = true
						keptRoot = append(keptRoot, name)
					}
				}
				sort.Strings(keptRoot)
				return true, nil
			})
		}
	}
	mirrored.Voice = mirrorItem("voice sections", func() (bool, error) { return mirrorVoiceSections(ctx, path) })
	return keptRoot
}
