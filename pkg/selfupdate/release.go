//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

type semverParts struct {
	core [3]uint64
	pre  []string
}

var semverRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func parseSemver(text string) (semverParts, bool) {
	match := semverRE.FindStringSubmatch(text)
	if match == nil {
		return semverParts{}, false
	}
	var parts semverParts
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return semverParts{}, false
		}
		parts.core[i] = n
	}
	if match[4] != "" {
		parts.pre = strings.Split(match[4], ".")
	}
	return parts, true
}

// Semver precedence of two tags, build metadata ignored and unparseable versions ranked lowest
func CompareVersions(a, b string) int {
	pa, okA := parseSemver(a)
	pb, okB := parseSemver(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := 0; i < 3; i++ {
		if pa.core[i] != pb.core[i] {
			if pa.core[i] < pb.core[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0
	case len(pa.pre) == 0:
		return 1
	case len(pb.pre) == 0:
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePrerelease(pa.pre[i], pb.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(pa.pre) < len(pb.pre):
		return -1
	case len(pa.pre) > len(pb.pre):
		return 1
	}
	return 0
}

// Orders one prerelease identifier pair, numeric below alphanumeric
func comparePrerelease(a, b string) int {
	na, errA := strconv.ParseUint(a, 10, 64)
	nb, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA == nil && errB == nil:
		if na == nb {
			return 0
		}
		if na < nb {
			return -1
		}
		return 1
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

var fetchFailureRules = []struct {
	matches func(string) bool
	message string
}{
	{func(s string) bool {
		return hasHTTPCode(s, "429") || strings.Contains(strings.ToLower(s), "rate limit")
	},
		"✗ GitHub is rate limiting requests or having an outage (HTTP 429) — try again in 5 minutes."},
	{func(s string) bool { return hasHTTPCode(s, "500", "502", "503", "504") },
		"✗ GitHub appears to be having an outage — try again in a few minutes (https://www.githubstatus.com)."},
	{func(s string) bool {
		return strings.Contains(s, "Could not resolve host") || strings.Contains(s, "unable to access")
	}, "✗ Network error — cannot reach the remote repository."},
	{func(s string) bool {
		return strings.Contains(s, "could not read Username") || strings.Contains(s, "terminal prompts disabled")
	}, "✗ GitHub rejected the anonymous fetch (asked for a login) — this usually means a GitHub outage;" +
		" try again in a few minutes (https://www.githubstatus.com). If it persists, check" +
		" `git remote -v` points at a public repo."},
	{func(s string) bool { return strings.Contains(s, "Authentication failed") },
		"✗ Authentication failed — check your git credentials or SSH key."},
	{func(s string) bool {
		return strings.Contains(s, "Permission denied (publickey)") || strings.Contains(s, "Host key verification failed")
	}, "✗ SSH authentication failed — check your SSH key is added to GitHub, or switch" +
		" `origin` to HTTPS: `git remote set-url origin https://github.com/nickheyer/icebath.git`."},
}

// Reports whether a fetch failure text carries any of the HTTP status codes in curl or git form
func hasHTTPCode(detail string, codes ...string) bool {
	for _, code := range codes {
		if strings.Contains(detail, "HTTP "+code) || strings.Contains(detail, "returned error: "+code) {
			return true
		}
	}
	return false
}

// One line diagnosis of a release fetch failure text
func ClassifyFetchFailure(detail string) string {
	for _, rule := range fetchFailureRules {
		if rule.matches(detail) {
			return rule.message
		}
	}
	return "✗ Failed to fetch updates from origin."
}

func rootOrRepo(projectRoot string) string {
	if projectRoot == "" {
		return pm.RepoRoot()
	}
	return projectRoot
}
