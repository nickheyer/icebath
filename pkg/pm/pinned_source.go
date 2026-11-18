package pm

import (
	"errors"
	"os"
	"path"
	"regexp"
	"strings"
)

// Release root of icebath's own repository, replaceable by tests
var releaseRoot = "https://github.com/nickheyer/icebath/releases/"

// Release tag this build was stamped with, empty for a source build
var buildTag string

// Stable or prerelease tag shape, an unprefixed version taking the v the release tags carry
var releaseTagRE = regexp.MustCompile(`^v?((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[a-z0-9]+(?:-[a-z0-9]+)*\.[0-9]+)?)$`)

// Records the version stamped into the binary so pool archives resolve to that release's copies
func SetBuildTag(version string) { buildTag = version }

// Asset URL on the release of this build, a source build reading the latest release
func ReleaseAssetURL(name string) string {
	if m := releaseTagRE.FindStringSubmatch(buildTag); m != nil {
		return releaseRoot + "download/v" + m[1] + "/" + name
	}
	return releaseRoot + "latest/download/" + name
}

// Asset name a pool archive is republished under, its sha256 keeping the name unique and free of special characters
func PoolAssetName(rawURL, sha string) string {
	return "termux-" + sha + path.Ext(urlFilename(rawURL))
}

// Whether the release workflow republishes a pin, true for Termux pool archives that the pool deletes on rebuild
var releaseCopied = func(rawURL string) bool {
	pkg, err := PackageOf(rawURL)
	return err == nil && pkg != ""
}

// Download source of a pin, npm tarballs following the user's registry and pool archives falling back to the release copy
func PinnedSource(rawURL, dest, sha string) (Source, error) {
	if !fullSHA256.MatchString(sha) {
		return Source{}, errors.New("A pinned input requires a full lowercase SHA256")
	}
	source := Source{URL: NPMRegistryURL(rawURL, environMap()), Dest: dest, SHA256: sha}
	if releaseCopied(rawURL) {
		source.Fallbacks = []string{ReleaseAssetURL(PoolAssetName(rawURL, sha))}
	}
	return source, nil
}

// Process environment as a map
func environMap() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}
