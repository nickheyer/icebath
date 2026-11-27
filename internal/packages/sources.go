package packages

import (
	"bufio"
	"bytes"
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Registry the pins record npm tarballs against
const npmPublic = "https://registry.npmjs.org/"

// Releases of icebath itself, which carry copies of the Termux archives the pool deletes when it rebuilds
const releases = "https://github.com/nickheyer/icebath/releases/"

// Termux main package pool the bionic pins come from
const TermuxPool = "https://packages.termux.dev/apt/termux-main/"

var releaseTag = regexp.MustCompile(`^v?((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[a-z0-9]+(?:-[a-z0-9]+)*\.[0-9]+)?)$`)

// Download of a pinned archive: npm tarballs from the registry the user set, Termux pool archives with this release's copy as a mirror
func pinnedSource(rawURL, dest, sha string) (Source, error) {
	if !fullSHA256.MatchString(sha) {
		return Source{}, errors.New("A pinned input requires a full lowercase SHA256")
	}
	source := Source{URL: npmRegistryURL(rawURL), Dest: dest, SHA256: sha}
	if termuxPackage(rawURL) != "" {
		source.Fallbacks = []string{releaseAsset("termux-" + sha + path.Ext(rawURL))}
	}
	return source, nil
}

// Address of an asset on the release of this build, the latest release for a build without a release version
func releaseAsset(name string) string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if m := releaseTag.FindStringSubmatch(info.Main.Version); m != nil {
			return releases + "download/v" + m[1] + "/" + name
		}
	}
	return releases + "latest/download/" + name
}

// Package of the Termux pool a pinned address names, empty for any other source
func termuxPackage(rawURL string) string {
	u, err := url.Parse(rawURL)
	pool, _ := url.Parse(TermuxPool)
	prefix := pool.Path + "pool/main/"
	if err != nil || u.Scheme != "https" || u.Host != pool.Host || !strings.HasPrefix(u.Path, prefix) {
		return ""
	}
	parts := strings.Split(u.Path[len(prefix):], "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[1]
}

// Registry npm resolves from: npm_config_registry, else the registry of the user's npmrc, else the public one
func npmRegistry() string {
	value := ""
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "npm_config_registry") {
			value = strings.TrimSpace(v)
		}
	}
	if value == "" {
		npmrc := ""
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "npm_config_userconfig") {
				npmrc = v
			}
		}
		if npmrc == "" {
			if home, err := os.UserHomeDir(); err == nil {
				npmrc = filepath.Join(home, ".npmrc")
			}
		}
		if data, err := os.ReadFile(fsx.ExpandHome(npmrc)); err == nil {
			scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
			for scanner.Scan() {
				if key, candidate, ok := strings.Cut(scanner.Text(), "="); ok && strings.EqualFold(strings.TrimSpace(key), "registry") {
					value = strings.Trim(strings.TrimSpace(candidate), `"'`)
				}
			}
		}
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return strings.TrimRight(value, "/") + "/"
	}
	return npmPublic
}

// A public npm registry address moved onto the registry the user set, any other address unchanged
func npmRegistryURL(rawURL string) string {
	if rest, ok := strings.CutPrefix(rawURL, npmPublic); ok {
		return npmRegistry() + rest
	}
	return rawURL
}
