package install

import (
	"context"
	"fmt"
	"regexp"
)

var canaryReleaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+\+canary\.20\d{6}T\d{6}Z$`)

// Newest published desktop release of a channel, the latest stable tag for stable and the newest canary build for canary
func LatestDesktopRelease(ctx context.Context, r Repo, channel string) (*ReleaseTarget, error) {
	if channel == "stable" {
		tag := LatestStableRelease(ctx, r)
		if tag == "" {
			return nil, fmt.Errorf("no stable release of %s is published", r.Web)
		}
		return targetForTag(ctx, r, tag), nil
	}
	if channel != "canary" {
		return nil, fmt.Errorf("no %s release of %s is published", channel, r.Web)
	}
	releases, err := listReleases(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("could not list releases of %s: %w", r.Web, err)
	}
	for _, rel := range newestFirst(releases) {
		if !rel.Draft && canaryReleaseTagRE.MatchString(rel.Tag) {
			t := &ReleaseTarget{Tag: rel.Tag, Assets: rel.Assets, Repo: r}
			if IsFullSHA(rel.Target) {
				t.Commit = rel.Target
			} else if refs, err := LsRemote(ctx, r); err == nil {
				t.Commit = tagCommit(refs, rel.Tag)
			}
			return t, nil
		}
	}
	return nil, fmt.Errorf("no canary release of %s is published", r.Web)
}
