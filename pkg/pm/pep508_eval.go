package pm

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// UndefinedMarkerError is packaging's UndefinedComparison or UndefinedEnvironmentName raised while evaluating a marker
type UndefinedMarkerError struct {
	Class   string
	Message string
}

func (e *UndefinedMarkerError) Error() string { return e.Message }

// pep440Ord is one comparison key slot, Kind -1 and 1 standing for negative and positive infinity
type pep440Ord struct {
	kind  int
	value string
}

// Orders two normalized decimal strings numerically
func cmpDecimal(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// Orders two key slots, infinities bounding every value and values compared as decimals or text
func cmpOrd(a, b pep440Ord, numeric bool) int {
	if a.kind != b.kind {
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	if a.kind != 0 {
		return 0
	}
	if numeric {
		return cmpDecimal(a.value, b.value)
	}
	return strings.Compare(a.value, b.value)
}

// Release segments with trailing zeros removed as packaging's comparison key does
func trimmedRelease(release []string) []string {
	i := len(release)
	for i > 0 && release[i-1] == "0" {
		i--
	}
	return release[:i]
}

// Orders two versions like packaging Version comparison over epoch, release, pre, post, dev and local
func comparePEP440(a, b *pep440Version) int {
	if c := cmpDecimal(a.epoch, b.epoch); c != 0 {
		return c
	}
	ra, rb := trimmedRelease(a.release), trimmedRelease(b.release)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if c := cmpDecimal(ra[i], rb[i]); c != 0 {
			return c
		}
	}
	if len(ra) != len(rb) {
		if len(ra) < len(rb) {
			return -1
		}
		return 1
	}
	preKey := func(v *pep440Version) (pep440Ord, pep440Ord) {
		switch {
		case v.pre == nil && !v.hasPost && v.hasDev:
			return pep440Ord{kind: -1}, pep440Ord{}
		case v.pre == nil:
			return pep440Ord{kind: 1}, pep440Ord{}
		}
		return pep440Ord{value: v.pre[0]}, pep440Ord{value: v.pre[1]}
	}
	la, na := preKey(a)
	lb, nb := preKey(b)
	if c := cmpOrd(la, lb, false); c != 0 {
		return c
	}
	if la.kind == 0 {
		if c := cmpDecimal(na.value, nb.value); c != 0 {
			return c
		}
	}
	postKey := func(v *pep440Version) pep440Ord {
		if !v.hasPost {
			return pep440Ord{kind: -1}
		}
		return pep440Ord{value: v.post}
	}
	if c := cmpOrd(postKey(a), postKey(b), true); c != 0 {
		return c
	}
	devKey := func(v *pep440Version) pep440Ord {
		if !v.hasDev {
			return pep440Ord{kind: 1}
		}
		return pep440Ord{value: v.dev}
	}
	if c := cmpOrd(devKey(a), devKey(b), true); c != 0 {
		return c
	}
	if a.local == nil || b.local == nil {
		switch {
		case a.local == nil && b.local == nil:
			return 0
		case a.local == nil:
			return -1
		}
		return 1
	}
	for i := 0; i < len(a.local) && i < len(b.local); i++ {
		da, db := isASCIIDigits(a.local[i]), isASCIIDigits(b.local[i])
		switch {
		case da && db:
			if c := cmpDecimal(a.local[i], b.local[i]); c != 0 {
				return c
			}
		case da != db:
			if da {
				return 1
			}
			return -1
		default:
			if c := strings.Compare(a.local[i], b.local[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a.local) < len(b.local):
		return -1
	case len(a.local) > len(b.local):
		return 1
	}
	return 0
}

// Pre release or development release, packaging's is_prerelease
func (v *pep440Version) isPrerelease() bool { return v.pre != nil || v.hasDev }

// Release with trailing zeros dropped down to one segment, the range engine's trimmed release
func rangeTrimmedRelease(release []string) []string {
	end := len(release)
	for end > 1 && release[end-1] == "0" {
		end--
	}
	return release[:end]
}

// Decimal text of a normalized non negative integer plus one
func decimalIncrement(n string) string {
	digits := []byte(n)
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] != '9' {
			digits[i]++
			return string(digits)
		}
		digits[i] = '0'
	}
	return "1" + string(digits)
}

// Reports equal pre release segments, both absent counting as equal
func samePre(a, b []string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a[0] == b[0] && a[1] == b[1]
}

// Reports a version sharing the boundary version's epoch, release up to trailing zeros and pre release
func releaseFamily(boundary, v *pep440Version) bool {
	if v.epoch != boundary.epoch {
		return false
	}
	trimmed := rangeTrimmedRelease(boundary.release)
	if len(v.release) < len(trimmed) {
		return false
	}
	for i, segment := range trimmed {
		if v.release[i] != segment {
			return false
		}
	}
	for _, segment := range v.release[len(trimmed):] {
		if segment != "0" {
			return false
		}
	}
	return samePre(v.pre, boundary.pre)
}

// Reports a version in the boundary version's local family, same release, pre, post and dev
func localFamily(boundary, v *pep440Version) bool {
	return releaseFamily(boundary, v) && v.hasPost == boundary.hasPost && v.post == boundary.post &&
		v.hasDev == boundary.hasDev && v.dev == boundary.dev
}

// Where a range bound sits, on its version or just past the version's locals or its post releases
type boundKind int

const (
	boundExact boundKind = iota
	boundAfterLocals
	boundAfterPosts
)

// One end of a version range, a nil version standing for an unbounded end
type versionBound struct {
	version   *pep440Version
	kind      boundKind
	inclusive bool
}

// One interval of versions between a lower and an upper bound
type versionInterval struct{ lower, upper versionBound }

// Whether a version lies at or above a lower bound
func (b versionBound) above(v *pep440Version) bool {
	switch {
	case b.version == nil:
		return true
	case b.kind == boundAfterPosts:
		return comparePEP440(b.version, v) < 0 && !releaseFamily(b.version, v)
	case b.kind == boundAfterLocals:
		return comparePEP440(b.version, v) < 0 && !localFamily(b.version, v)
	case b.inclusive:
		return comparePEP440(b.version, v) <= 0
	}
	return comparePEP440(b.version, v) < 0
}

// Whether a version lies at or below an upper bound
func (b versionBound) below(v *pep440Version) bool {
	switch {
	case b.version == nil:
		return true
	case b.kind == boundAfterLocals:
		return comparePEP440(b.version, v) >= 0 || localFamily(b.version, v)
	case b.inclusive:
		return comparePEP440(b.version, v) >= 0
	}
	return comparePEP440(b.version, v) > 0
}

// Release dev0 version over the given epoch and release segments
func releaseDev0(epoch string, release []string) *pep440Version {
	return &pep440Version{epoch: epoch, release: release, dev: "0", hasDev: true}
}

// Smallest version of the next release prefix, 1.2 giving 1.3.dev0
func nextPrefixDev0(v *pep440Version) *pep440Version {
	release := append(append([]string{}, v.release[:len(v.release)-1]...), decimalIncrement(v.release[len(v.release)-1]))
	return releaseDev0(v.epoch, release)
}

// Packaging's 0.dev0, below which an exclusive upper bound admits nothing
var minimumVersion = releaseDev0("0", []string{"0"})

// Intervals a wildcard equality or inequality covers, 1.2.* spanning 1.2.dev0 up to 1.3.dev0
func wildcardIntervals(op string, base *pep440Version) []versionInterval {
	lower, upper := releaseDev0(base.epoch, base.release), nextPrefixDev0(base)
	if op == "==" {
		return []versionInterval{{versionBound{version: lower, inclusive: true}, versionBound{version: upper}}}
	}
	return []versionInterval{{versionBound{}, versionBound{version: lower}}, {versionBound{version: upper, inclusive: true}, versionBound{}}}
}

// Intervals a standard operator covers, the local flag telling whether the clause names a local label
func standardIntervals(op string, v *pep440Version, hasLocal bool) []versionInterval {
	switch op {
	case ">=":
		return []versionInterval{{versionBound{version: v, inclusive: true}, versionBound{}}}
	case "<=":
		return []versionInterval{{versionBound{}, versionBound{version: v, kind: boundAfterLocals, inclusive: true}}}
	case ">":
		if v.hasDev {
			next := *v
			next.dev, next.local = decimalIncrement(v.dev), nil
			return []versionInterval{{versionBound{version: &next, inclusive: true}, versionBound{}}}
		}
		if v.hasPost {
			next := *v
			next.post, next.dev, next.hasDev, next.local = decimalIncrement(v.post), "0", true, nil
			return []versionInterval{{versionBound{version: &next, inclusive: true}, versionBound{}}}
		}
		return []versionInterval{{versionBound{version: v, kind: boundAfterPosts}, versionBound{}}}
	case "<":
		bound := v
		if !v.isPrerelease() {
			dev0 := *v
			dev0.dev, dev0.hasDev, dev0.local = "0", true, nil
			bound = &dev0
		}
		if comparePEP440(bound, minimumVersion) <= 0 {
			return nil
		}
		return []versionInterval{{versionBound{}, versionBound{version: bound}}}
	case "~=":
		prefix := *v
		prefix.release = v.release[:len(v.release)-1]
		return []versionInterval{{versionBound{version: v, inclusive: true}, versionBound{version: nextPrefixDev0(&prefix)}}}
	}
	upper := versionBound{version: v, kind: boundAfterLocals, inclusive: true}
	if hasLocal {
		upper.kind = boundExact
	}
	if op == "==" {
		return []versionInterval{{versionBound{version: v, inclusive: true}, upper}}
	}
	return []versionInterval{{versionBound{}, versionBound{version: v}}, {versionBound{version: upper.version, kind: upper.kind}, versionBound{}}}
}

// Whether a version falls inside any of the sorted, non overlapping intervals
func intervalsContain(intervals []versionInterval, v *pep440Version) bool {
	for _, interval := range intervals {
		if !interval.lower.above(v) {
			return false
		}
		if interval.upper.below(v) {
			return true
		}
	}
	return false
}

// Parses a version that a validated specifier carries, failing when the text is not a version
func mustSpecVersion(text string) (*pep440Version, error) {
	v, ok, err := parsePEP440(text)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &PyError{Class: "InvalidVersion", Message: fmt.Sprintf("Invalid version: %s", pyrepr.StrRepr(text))}
	}
	return v, nil
}

// Direct comparison answer for simple clauses on a version without a local label, decided false when ranges must answer
func fastSpecifierMatch(s pySpecifier, v *pep440Version) (match, decided bool, err error) {
	if strings.HasSuffix(s.version, ".*") || v.local != nil {
		return false, false, nil
	}
	switch s.op {
	case ">=", "<=", "==", "!=", "<", ">":
	default:
		return false, false, nil
	}
	specV, err := mustSpecVersion(s.version)
	if err != nil {
		return false, false, err
	}
	c := comparePEP440(v, specV)
	switch s.op {
	case ">=":
		return c >= 0, true, nil
	case "<=":
		return c <= 0, true, nil
	case "==":
		return c == 0, true, nil
	case "!=":
		return c != 0, true, nil
	}
	if v.epoch == specV.epoch && slices.Equal(rangeTrimmedRelease(v.release), rangeTrimmedRelease(specV.release)) {
		return false, false, nil
	}
	if s.op == "<" {
		return c < 0, true, nil
	}
	return c > 0, true, nil
}

// Whether a version satisfies one clause with prereleases admitted, an unparsable version matching nothing
func (s pySpecifier) contains(item string) (bool, error) {
	if s.op == "===" {
		return pystr.Lower(item) == pystr.Lower(s.version), nil
	}
	parsed, ok, err := parsePEP440(item)
	if err != nil || !ok {
		return false, err
	}
	if match, decided, err := fastSpecifierMatch(s, parsed); err != nil || decided {
		return match, err
	}
	specV, err := mustSpecVersion(strings.TrimSuffix(s.version, ".*"))
	if err != nil {
		return false, err
	}
	if strings.HasSuffix(s.version, ".*") {
		return intervalsContain(wildcardIntervals(s.op, specV), parsed), nil
	}
	return intervalsContain(standardIntervals(s.op, specV, strings.Contains(s.version, "+")), parsed), nil
}

// Whether a version satisfies every clause of a specifier set with prereleases admitted
func specifierSetContains(specs []pySpecifier, item string) (bool, error) {
	for _, s := range specs {
		ok, err := s.contains(item)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

var markersRequiringVersion = map[string]bool{"implementation_version": true, "platform_release": true, "python_full_version": true, "python_version": true}

var markerVersionClause = regexp.MustCompile(`^(~=|===|==|!=|<=|>=|<|>)`)

// One comparison of a marker, version keys compared as specifiers and everything else as strings
func evalMarkerOp(lhs, op, rhs, key string) (bool, error) {
	if markersRequiringVersion[key] && markerVersionClause.MatchString(op) {
		if spec, err := parseSpecifier(op + rhs); err == nil {
			return spec.contains(lhs)
		}
	}
	switch op {
	case "in":
		return strings.Contains(rhs, lhs), nil
	case "not in":
		return !strings.Contains(rhs, lhs), nil
	case "<", ">":
		return false, nil
	case "<=", "==", ">=":
		return lhs == rhs, nil
	case "!=":
		return lhs != rhs, nil
	}
	return false, &UndefinedMarkerError{Class: "UndefinedComparison", Message: fmt.Sprintf("Undefined <Op(%s)> on %s and %s.", pyrepr.StrRepr(op), pyrepr.StrRepr(lhs), pyrepr.StrRepr(rhs))}
}

// Evaluates a parsed marker against a full environment the way packaging's _evaluate_markers does
func evaluateMarkerList(markers markerList, env map[string]string) (bool, error) {
	groups := [][]bool{{}}
	for _, marker := range markers {
		switch m := marker.(type) {
		case markerList:
			ok, err := evaluateMarkerList(m, env)
			if err != nil {
				return false, err
			}
			groups[len(groups)-1] = append(groups[len(groups)-1], ok)
		case markerItem:
			var lhs, rhs, key string
			var found bool
			if m.lhs.variable {
				key = m.lhs.value
				lhs, found = env[key]
				rhs = m.rhs.value
			} else {
				lhs = m.lhs.value
				key = m.rhs.value
				rhs, found = env[key]
			}
			if !found {
				return false, &UndefinedMarkerError{Class: "UndefinedEnvironmentName", Message: pyrepr.StrRepr(key)}
			}
			if key == "extras" || key == "dependency_groups" {
				lhs, rhs = CanonicalizeName(lhs), CanonicalizeName(rhs)
			}
			ok, err := evalMarkerOp(lhs, m.op, rhs, key)
			if err != nil {
				return false, err
			}
			groups[len(groups)-1] = append(groups[len(groups)-1], ok)
		case string:
			if m == "or" {
				groups = append(groups, []bool{})
			}
		}
	}
	for _, group := range groups {
		all := true
		for _, ok := range group {
			all = all && ok
		}
		if all {
			return true, nil
		}
	}
	return false, nil
}

// Keys packaging's default_environment reports for the running interpreter
var defaultEnvironmentKeys = map[string]bool{"implementation_name": true, "implementation_version": true, "os_name": true,
	"platform_machine": true, "platform_release": true, "platform_system": true, "platform_version": true,
	"python_full_version": true, "platform_python_implementation": true, "python_version": true, "sys_platform": true}

// Reports a marker naming an interpreter default key that the environment leaves unset
func markerNeedsDefaults(markers markerList, environment map[string]string) bool {
	for _, marker := range markers {
		switch m := marker.(type) {
		case markerList:
			if markerNeedsDefaults(m, environment) {
				return true
			}
		case markerItem:
			for _, operand := range []markerVar{m.lhs, m.rhs} {
				if _, set := environment[operand.value]; operand.variable && defaultEnvironmentKeys[operand.value] && !set {
					return true
				}
			}
		}
	}
	return false
}

// Marker evaluation in packaging's metadata context, the environment overriding defaults and extra canonicalized
func evaluateMarker(markers markerList, defaults, environment map[string]string) (bool, error) {
	env := map[string]string{"extra": ""}
	for k, v := range defaults {
		env[k] = v
	}
	for k, v := range environment {
		env[k] = v
	}
	if extra := env["extra"]; extra != "" {
		env["extra"] = CanonicalizeName(extra)
	}
	if full, ok := env["python_full_version"]; ok && strings.HasSuffix(full, "+") {
		env["python_full_version"] = full + "local"
	}
	return evaluateMarkerList(markers, env)
}

// InvalidMarkerError is packaging's InvalidMarker raised for marker text that does not parse
type InvalidMarkerError struct{ Message string }

func (e *InvalidMarkerError) Error() string { return e.Message }

// Parses a standalone marker expression like packaging Marker, extra and set membership literals canonicalized
func parseMarker(text string) (markerList, error) {
	t := &pepTokenizer{src: []rune(text), source: text}
	markers, err := parseMarkerTokens(t)
	if err == nil {
		_, _, err = t.expect("END", "end of marker expression")
	}
	if err != nil {
		if syntax, ok := err.(*ParserSyntaxError); ok {
			return nil, &InvalidMarkerError{Message: syntax.Error()}
		}
		return nil, err
	}
	return normalizeExtras(markers).(markerList), nil
}

// EvaluateMarker evaluates a PEP 508 marker string in process, the environment over defaults like Marker.evaluate
func EvaluateMarker(marker string, defaults, environment map[string]string) (bool, error) {
	markers, err := parseMarker(marker)
	if err != nil {
		return false, err
	}
	return evaluateMarker(markers, defaults, environment)
}

// MarkerHolds evaluates the requirement's marker over the environment laid on the defaults, true when it carries none
func (r *pyRequirement) MarkerHolds(defaults, environment map[string]string) (bool, error) {
	if len(r.Marker) == 0 {
		return true, nil
	}
	return evaluateMarker(r.Marker, defaults, environment)
}
