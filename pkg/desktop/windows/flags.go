package windows

import (
	"regexp"
	"slices"
)

// FeatureFlags are the launch facts the renderer reads before its first paint.
type FeatureFlags struct {
	LocalModels        bool `json:"localModels"`
	AssumeFreshInstall bool `json:"assumeFreshInstall"`
}

var canaryTag = regexp.MustCompile(`\+canary\.20\d{6}T\d{6}Z$`)

// IsCanaryTag recognizes canary build stamps
func IsCanaryTag(tag string) bool { return canaryTag.MatchString(tag) }

// LocalModelsEnabled follows the platform, or --local in argv on any platform
func LocalModelsEnabled(goos, goarch string, argv []string) bool {
	return goos == "windows" || goos == "darwin" || (goos == "linux" && (goarch == "amd64" || goarch == "arm64")) || slices.Contains(argv, "--local")
}

// ResolveFeatureFlags answers the feature flag query, fresh install only for unpackaged runs started with --fresh-install
func ResolveFeatureFlags(goos, goarch string, argv []string, packaged bool) FeatureFlags {
	return FeatureFlags{
		LocalModels:        LocalModelsEnabled(goos, goarch, argv),
		AssumeFreshInstall: !packaged && slices.Contains(argv, "--fresh-install"),
	}
}
