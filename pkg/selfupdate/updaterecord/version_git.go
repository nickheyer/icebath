package updaterecord

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Stripped stdout of a git query in the checkout, empty on a failure, a nonzero exit or no output
func runGitQueryText(repoDir string, args ...string) string {
	argv := append([]string{"git"}, args...)
	out, err := pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Dir: repoDir, Timeout: 3 * time.Second})
	if err != nil || out.Returncode != 0 {
		return ""
	}
	return strings.TrimSpace(pystr.DecodeUTF8Replace(out.Stdout))
}

// Version the nearest CalVer release shipped and the commits since it, false without such a release
func calverReleaseVersion(repoDir string) (string, int64, bool) {
	described := runGitQueryText(repoDir, "describe", "--tags", "--long", "--match", "v2[0-9][0-9][0-9].*", "HEAD")
	if described == "" {
		return "", 0, false
	}
	parts := strings.Split(described, "-")
	if len(parts) < 3 {
		return "", 0, false
	}
	tag, count := strings.Join(parts[:len(parts)-2], "-"), parts[len(parts)-2]
	distance := parseNonnegative(count)
	project, err := pm.TOMLLoads(runGitQueryText(repoDir, "show", tag+":pyproject.toml"))
	if err != nil {
		return "", 0, false
	}
	section, _ := project.Get("project")
	table, ok := section.(*pm.OrderedObject)
	if !ok {
		return "", 0, false
	}
	raw, _ := table.Get("version")
	version, isText := raw.(string)
	if distance == nil || !isText || !StableTagRE.MatchString("v"+version) {
		return "", 0, false
	}
	return version, *distance, true
}

// Integer parts of a stable version for release ordering
func releaseParts(version string) []int64 {
	var out []int64
	for _, part := range strings.Split(version, ".") {
		n, _ := strconv.ParseInt(part, 10, 64)
		out = append(out, n)
	}
	return out
}

// Whether release a orders before release b by numeric parts
func releaseLess(a, b string) bool {
	pa, pb := releaseParts(a), releaseParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// Identity of a checkout read from git itself, a status over three seconds reading as clean
func GitVersionInfo(repoDir string, includeUntracked bool) VersionInfo {
	commit := runGitQueryText(repoDir, "rev-parse", "HEAD")
	branch := runGitQueryText(repoDir, "branch", "--show-current")
	var commitDate *int64
	if raw := runGitQueryText(repoDir, "log", "-1", "--format=%ct", "HEAD"); raw != "" && pystr.StrIsDigit(raw) {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			commitDate = &n
		}
	}
	status := []string{"git", "--no-optional-locks", "status", "--porcelain"}
	if !includeUntracked {
		status = append(status, "-uno")
	}
	dirty := false
	if out, err := pysubprocess.RunWith(context.Background(), status, pysubprocess.Options{Dir: repoDir, Timeout: 3 * time.Second}); err == nil && out.Returncode == 0 {
		dirty = strings.TrimSpace(pystr.DecodeUTF8Replace(out.Stdout)) != ""
	}
	var releases []string
	for _, tag := range pystr.Splitlines(runGitQueryText(repoDir, "tag", "--merged", "HEAD", "--list", "v[0-9]*"), false) {
		if StableTagRE.MatchString(tag) {
			releases = append(releases, tag[1:])
		}
	}
	base := "unknown"
	var distance *int64
	if len(releases) > 0 {
		base = releases[0]
		for _, r := range releases[1:] {
			if releaseLess(base, r) {
				base = r
			}
		}
		distance = parseNonnegative(runGitQueryText(repoDir, "rev-list", "--count", "v"+base+"..HEAD"))
	} else if version, d, ok := calverReleaseVersion(repoDir); ok {
		base, distance = version, &d
	}
	short := runGitQueryText(repoDir, "rev-parse", "--short=7", "HEAD")
	display := derivedVersion(base, distance, dirty, short)
	if base == "unknown" && short != "" {
		display = "git." + short
		if dirty {
			display += ".dirty"
		}
	}
	return VersionInfo{BaseVersion: base, DerivedVersion: display, Distance: distance, Commit: optionalString(commit),
		Branch: optionalString(branch), Source: "git", Dirty: dirty, CommitDate: commitDate}
}

// ResetVersionInfoCache exports resetVersionInfoCache
func ResetVersionInfoCache() { resetVersionInfoCache() }
