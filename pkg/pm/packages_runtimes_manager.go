//go:build !icebath_core && !icebath_noextras

package pm

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func (p *Uv) LatestVersions(target, locked string) ([]string, error) {
	return GithubReleaseTags("astral-sh/uv", "")
}

// Keeps the locked minor line and takes the advertised asset's patch and build, bionic staying a manual pin
func (p *Python) LatestVersions(target, locked string) ([]string, error) {
	if target == bionicTarget || locked == "" || !strings.Contains(locked, "+") {
		return nil, nil
	}
	pyver, _ := partition(locked, "+")
	parts := strings.Split(pyver, ".")
	minor := strings.Join(parts[:min(2, len(parts))], ".")
	triple, ok := rustTriple[target]
	if !ok {
		return nil, missingKey("no download for target " + target)
	}
	return PbsVersions(minor, triple)
}

func (p *Nodejs) LatestVersions(target, locked string) ([]string, error) {
	return NodeLatestVersions()
}

func (p *Npm) LatestVersions(target, locked string) ([]string, error) {
	tags, err := NpmDistTags("npm")
	if err != nil {
		return nil, err
	}
	if latest := tags["latest"]; pyrepr.Truthy(latest) {
		return []string{pyrepr.Str(latest)}, nil
	}
	return nil, nil
}
