//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// The llama.app latest pointer first, then the bucket tree, then GitHub releases when the bucket shows nothing
func (p *LlamaCpp) LatestVersions(target, locked string) ([]string, error) {
	if latest, ok := LlamaAppLatest(); ok {
		return append([]string{latest}, LlamaAppBucketVersions()...), nil
	}
	if bucket := LlamaAppBucketVersions(); len(bucket) > 0 {
		return bucket, nil
	}
	return GithubReleaseTags("ggml-org/llama.cpp", "b")
}

// Digest GitHub's release API publishes for the asset, so pinning skips the download
func (p *LlamaCpp) KnownSHA256(version, url string) (string, error) {
	digests, err := githubReleaseDigests("ggml-org/llama.cpp", "b"+version)
	if err != nil {
		return "", err
	}
	return digests[url[strings.LastIndex(url, "/")+1:]], nil
}

// Asset name to sha256 digest of one GitHub release, empty when the release API cannot be read
func githubReleaseDigests(repo, tag string) (map[string]string, error) {
	key := [2]string{repo, tag}
	releaseDigestMu.Lock()
	cached, ok := releaseDigestCache[key]
	releaseDigestMu.Unlock()
	if ok {
		return cached, nil
	}
	data, err := getJSON(fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag))
	if err != nil {
		return map[string]string{}, nil
	}
	release, isMap := data.(map[string]any)
	if !isMap {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("the GitHub release reply is %s, expected a mapping", errchain.Kind(data))}
	}
	digests := map[string]string{}
	assets := []any{}
	if raw, present := release["assets"]; present {
		if assets, err = pyIterItems(raw); err != nil {
			return nil, err
		}
	}
	for _, item := range assets {
		asset, isAsset := item.(map[string]any)
		if !isAsset {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("a GitHub release asset is %s, expected a mapping", errchain.Kind(item))}
		}
		digest := ""
		if raw := asset["digest"]; pyrepr.Truthy(raw) {
			text, isText := raw.(string)
			if !isText {
				return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("a GitHub release asset digest is %s, expected text", errchain.Kind(raw))}
			}
			_, digest, _ = strings.Cut(text, "sha256:")
		}
		if digest == "" {
			continue
		}
		name, present := asset["name"]
		if !present {
			return nil, missingKey("a GitHub release asset has no name")
		}
		digests[pyrepr.Str(name)] = digest
	}
	releaseDigestMu.Lock()
	releaseDigestCache[key] = digests
	releaseDigestMu.Unlock()
	return digests, nil
}
