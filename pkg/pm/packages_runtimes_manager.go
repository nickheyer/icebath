//go:build !icebath_core && !icebath_noextras

package pm

import (
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	if latest := tags["latest"]; anyutil.IsTrue(latest) {
		return []string{anyutil.String(latest)}, nil
	}
	return nil, nil
}
