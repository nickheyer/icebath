package pm

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Copy of a version without its local label
func (v *pep440Version) public() *pep440Version {
	out := *v
	out.local = nil
	return &out
}

// Copy holding only the epoch and release, packaging's base_version
func (v *pep440Version) base() *pep440Version {
	return &pep440Version{epoch: v.epoch, release: v.release}
}

// Pre release or development release, packaging's is_prerelease
func (v *pep440Version) isPrerelease() bool { return v.pre != nil || v.hasDev }

// Segments packaging's _version_split yields for the canonical text of a public version
func (v *pep440Version) split() []string {
	out := append([]string{v.epoch}, v.release...)
	if v.pre != nil {
		out = append(out, v.pre[0]+v.pre[1])
	}
	if v.hasPost {
		out = append(out, "post"+v.post)
	}
	if v.hasDev {
		out = append(out, "dev"+v.dev)
	}
	return out
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

// Prefix match of a wildcard equality, both sides padded with zero release segments
func wildcardEqual(prospective *pep440Version, spec string) (bool, error) {
	specV, err := mustSpecVersion(strings.TrimSuffix(spec, ".*"))
	if err != nil {
		return false, err
	}
	left, right := prospective.public().split(), specV.split()
	digits := func(parts []string) int {
		n := 0
		for n < len(parts) && isASCIIDigits(parts[n]) {
			n++
		}
		return n
	}
	pad := func(parts []string, have, want int) []string {
		if want <= have {
			return parts
		}
		out := append([]string{}, parts[:have]...)
		for i := have; i < want; i++ {
			out = append(out, "0")
		}
		return append(out, parts[have:]...)
	}
	ld, rd := digits(left), digits(right)
	left = pad(left, ld, rd)
	if len(left) > len(right) {
		left = left[:len(right)]
	}
	if len(left) != len(right) {
		return false, nil
	}
	for i := range left {
		if left[i] != right[i] {
			return false, nil
		}
	}
	return true, nil
}

// Whether a version satisfies one clause with prereleases admitted, an unparsable version matching nothing
func (s pySpecifier) contains(item string) (bool, error) {
	if s.op == "===" {
		return strings.ToLower(item) == strings.ToLower(s.version), nil
	}
	prospective, ok, err := parsePEP440(item)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	switch s.op {
	case "==", "!=":
		var equal bool
		if strings.HasSuffix(s.version, ".*") {
			if equal, err = wildcardEqual(prospective, s.version); err != nil {
				return false, err
			}
		} else {
			specV, err := mustSpecVersion(s.version)
			if err != nil {
				return false, err
			}
			candidate := prospective
			if specV.local == nil {
				candidate = prospective.public()
			}
			equal = comparePEP440(candidate, specV) == 0
		}
		return equal == (s.op == "=="), nil
	case "~=":
		specV, err := mustSpecVersion(s.version)
		if err != nil {
			return false, err
		}
		if comparePEP440(prospective.public(), specV) < 0 {
			return false, nil
		}
		prefix := strings.Join(specV.release[:len(specV.release)-1], ".") + ".*"
		if specV.epoch != "0" {
			prefix = specV.epoch + "!" + prefix
		}
		return wildcardEqual(prospective, prefix)
	case "<=", ">=":
		specV, err := mustSpecVersion(s.version)
		if err != nil {
			return false, err
		}
		c := comparePEP440(prospective.public(), specV)
		if s.op == "<=" {
			return c <= 0, nil
		}
		return c >= 0, nil
	case "<":
		specV, err := mustSpecVersion(s.version)
		if err != nil {
			return false, err
		}
		if comparePEP440(prospective, specV) >= 0 {
			return false, nil
		}
		if !specV.isPrerelease() && prospective.isPrerelease() && comparePEP440(prospective.base(), specV.base()) == 0 {
			return false, nil
		}
		return true, nil
	case ">":
		specV, err := mustSpecVersion(s.version)
		if err != nil {
			return false, err
		}
		if comparePEP440(prospective, specV) <= 0 {
			return false, nil
		}
		if !specV.hasPost && prospective.hasPost && comparePEP440(prospective.base(), specV.base()) == 0 {
			return false, nil
		}
		if prospective.local != nil && comparePEP440(prospective.base(), specV.base()) == 0 {
			return false, nil
		}
		return true, nil
	}
	return false, &PyError{Class: "ValueError", Message: "unknown specifier operator " + pyrepr.StrRepr(s.op)}
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
