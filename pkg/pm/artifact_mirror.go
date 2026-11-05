package pm

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

//go:embed artifact-mirror.json
var artifactMirrorLayout []byte

// Origin and key prefix of the public content addressed mirror
var mirrorLayout = func() struct {
	Origin string `json:"origin"`
	Prefix string `json:"prefix"`
} {
	var layout struct {
		Origin string `json:"origin"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(trimBOM(artifactMirrorLayout), &layout); err != nil {
		panic(err)
	}
	return layout
}()

// MirrorKeyPrefix is the object key prefix every mirrored input lives under
var MirrorKeyPrefix = mirrorLayout.Prefix

// MirrorPublicPrefix is the public URL prefix of mirrored inputs
var MirrorPublicPrefix = mirrorLayout.Origin + "/" + MirrorKeyPrefix

// Mirror object key of a pinned input, refusing anything but a full lowercase sha256
func ObjectKey(sha string) (string, error) {
	if !fullSHA256.MatchString(sha) {
		return "", errors.New("A pinned input requires a full lowercase SHA256")
	}
	return MirrorKeyPrefix + sha, nil
}

// Public mirror URL of a pinned input
func MirrorURL(sha string) (string, error) {
	if _, err := ObjectKey(sha); err != nil {
		return "", err
	}
	return MirrorPublicPrefix + sha, nil
}

// Download source of a pin, served from the user's npm registry when it is an npm tarball, the mirror as fallback
func PinnedSource(rawURL, dest, sha string) (Source, error) {
	archive, err := MirrorURL(sha)
	if err != nil {
		return Source{}, err
	}
	rawURL = NPMRegistryURL(rawURL, environMap())
	source := Source{URL: rawURL, Dest: dest, SHA256: sha}
	if rawURL != archive {
		source.Fallbacks = []string{archive}
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
