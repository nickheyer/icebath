package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// File beside the binary naming the repository a release install updates from
const InstallRepoFile = ".install_repo"

// Records the repository an install's releases come from beside its binary
func WriteInstalledRepository(dir, repoURL string) error {
	return os.WriteFile(filepath.Join(dir, InstallRepoFile), []byte(strings.TrimSpace(repoURL)+"\n"), 0o644)
}

// Repository an install recorded, empty when it recorded none
func InstalledRepository(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, InstallRepoFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Releases an update chooses from, a pinned commit first, then the latest stable release, then the newest of a branch
type ReleaseQuery struct {
	Commit string
	Stable bool
	Branch string
}

// Order of a release against the installed build
const (
	ReleaseOlder = -1
	ReleaseSame  = 0
	ReleaseNewer = 1
)

// Release an update would move a binary install to and its order against the installed build
type ReleasePlan struct {
	Installed ReleaseInfo
	Target    *ReleaseTarget
	Order     int
	Ahead     int
}

// Failure of an install whose build names neither a release tag nor a commit an order can be read from
var ErrUnknownBuild = errors.New("the release this binary was built from is unknown")

// Selects the release the query names and orders it against the binary installed in dir
func PlanRelease(ctx context.Context, dir, repoURL string, q ReleaseQuery) (*ReleasePlan, error) {
	repo, err := ParseRepo(repoURL)
	if err != nil {
		return nil, err
	}
	var target *ReleaseTarget
	switch {
	case q.Commit != "":
		if target, err = releaseForCommit(ctx, repo, q.Commit); err != nil {
			return nil, fmt.Errorf("could not list releases of %s: %w", repo.Web, err)
		}
		if target == nil {
			return nil, fmt.Errorf("no published release of %s was built from commit %s", repo.Web, q.Commit)
		}
	case q.Stable:
		tag := LatestStableRelease(ctx, repo)
		if tag == "" {
			return nil, fmt.Errorf("could not look up the latest stable release of %s", repo.Web)
		}
		target = targetForTag(ctx, repo, tag)
	default:
		if target, err = releaseForBranch(ctx, repo, q.Branch); err != nil {
			return nil, fmt.Errorf("could not list releases of %s: %w", repo.Web, err)
		}
		if target == nil {
			return nil, fmt.Errorf("no published release of %s follows branch %s", repo.Web, q.Branch)
		}
	}
	installed := InstalledRelease(ctx, InstallBinaryPath(dir))
	order, ahead, err := orderRelease(ctx, repo, installed, target)
	if err != nil {
		return nil, err
	}
	return &ReleasePlan{Installed: installed, Target: target, Order: order, Ahead: ahead}, nil
}

// Commit relation the compare API reports from base to head
type commitComparison struct {
	Status  string `json:"status"`
	AheadBy int    `json:"ahead_by"`
}

// Relation of head to base as the repository's compare API answers it
func compareCommits(ctx context.Context, r Repo, base, head string) (commitComparison, error) {
	body, err := httpGet(ctx, r.API+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), true)
	if err != nil {
		return commitComparison{}, err
	}
	var cmp commitComparison
	err = json.Unmarshal(body, &cmp)
	return cmp, err
}

// Order and ahead count of target against installed, by ancestry with diverged as newer, else by stable tags
func orderRelease(ctx context.Context, r Repo, installed ReleaseInfo, target *ReleaseTarget) (int, int, error) {
	if installed.Commit != "" && installed.Commit == target.Commit {
		return ReleaseSame, 0, nil
	}
	var unordered error
	switch {
	case installed.Commit != "" && target.Commit != "":
		cmp, err := compareCommits(ctx, r, installed.Commit, target.Commit)
		switch {
		case err != nil:
			unordered = fmt.Errorf("could not compare commit %s with release %s of %s: %w", installed.Commit, target.Tag, r.Web, err)
		case cmp.Status == "identical":
			return ReleaseSame, 0, nil
		case cmp.Status == "ahead" || cmp.Status == "diverged":
			return ReleaseNewer, cmp.AheadBy, nil
		case cmp.Status == "behind":
			return ReleaseOlder, 0, nil
		default:
			unordered = fmt.Errorf("%s compared commit %s with release %s as %q", r.Web, installed.Commit, target.Tag, cmp.Status)
		}
	case installed.Commit != "":
		unordered = fmt.Errorf("the commit release %s of %s was built from is unknown", target.Tag, r.Web)
	}
	if ValidStableTag(installed.Tag) && ValidStableTag(target.Tag) {
		a, b := stableParts(installed.Tag), stableParts(target.Tag)
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				if b[k] > a[k] {
					return ReleaseNewer, -1, nil
				}
				return ReleaseOlder, 0, nil
			}
		}
		return ReleaseSame, 0, nil
	}
	if installed.Tag != "" && installed.Tag == target.Tag {
		return ReleaseSame, 0, nil
	}
	if unordered != nil {
		return ReleaseSame, 0, unordered
	}
	return ReleaseSame, 0, ErrUnknownBuild
}
