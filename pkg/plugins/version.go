// Package plugins discovers, loads, installs and dispatches icebath plugins through one registration context
package plugins

import (
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// Version of the running binary, read from build info and overridable by the main package
var RunningVersion = buildVersion()

var versionComparatorRe = regexp.MustCompile(`^\s*(>=|<=|==|!=|>|<)\s*(.+?)\s*$`)
var versionSegmentRe = regexp.MustCompile(`^\d+`)

// Main module version from the build info with a leading v removed
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return ""
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

// Three integer segments of a version string, nil when a segment has no leading digits
func versionTuple(v string) []int {
	head := strings.TrimLeft(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(head, "-+"); i >= 0 {
		head = head[:i]
	}
	parts := strings.Split(head, ".")
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	out := make([]int, 0, 3)
	for _, p := range parts[:3] {
		m := versionSegmentRe.FindString(strings.TrimSpace(p))
		if m == "" {
			return nil
		}
		n, _ := strconv.Atoi(m)
		out = append(out, n)
	}
	return out
}

// Sign of a minus b over three segments
func compareVersions(a, b []int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Reports whether current satisfies every comma separated clause, unparseable versions passing
func VersionSatisfies(spec, current string) bool {
	cur := versionTuple(current)
	if cur == nil {
		return true
	}
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		op, target := ">=", clause
		if m := versionComparatorRe.FindStringSubmatch(clause); m != nil {
			op, target = m[1], m[2]
		}
		tgt := versionTuple(target)
		if tgt == nil {
			continue
		}
		c := compareVersions(cur, tgt)
		ok := true
		switch op {
		case ">=":
			ok = c >= 0
		case "<=":
			ok = c <= 0
		case "==":
			ok = c == 0
		case "!=":
			ok = c != 0
		case ">":
			ok = c > 0
		case "<":
			ok = c < 0
		}
		if !ok {
			return false
		}
	}
	return true
}

// Reports whether every clause of a requires spec parses strictly
func requiresSpecValid(spec string) bool {
	for _, clause := range strings.Split(spec, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		target := clause
		if m := versionComparatorRe.FindStringSubmatch(clause); m != nil {
			target = m[2]
		}
		if versionTuple(target) == nil {
			return false
		}
	}
	return true
}

// Load blocking reason when a requires_icebath spec rejects the running version, else empty
func RequiresVersionError(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || VersionSatisfies(spec, RunningVersion) {
		return ""
	}
	return "requires icebath " + spec + ", running " + RunningVersion
}
