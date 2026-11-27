package packages

import (
	"fmt"

	"github.com/nickheyer/icebath/internal/config"
)

// Body of an upstream index page a package's newest versions are read from, remembered within one operation
func Fetch(cfg *config.Config, rawURL string) ([]byte, error) {
	return fetch(cfg, rawURL)
}

// One published release of a GitHub repository, with the names of its assets
type Release struct {
	Tag    string
	Assets []string
}

// Releases of up to pages pages of a GitHub repository, newest first as GitHub lists them, drafts and prereleases left out
func GitHubReleases(cfg *config.Config, repo string, pages int) ([]Release, error) {
	var out []Release
	err := releasePages(cfg, repo, pages, func(r release) bool {
		if r.Draft || r.Prerelease {
			return false
		}
		published := Release{Tag: r.Tag}
		for _, asset := range r.Assets {
			published.Assets = append(published.Assets, asset.Name)
		}
		out = append(out, published)
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("releases of %s: %w", repo, err)
	}
	return out, nil
}
