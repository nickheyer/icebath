package packages

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Environment markers of a target under the pinned interpreter, as PEP 508 names them
func markerEnv(target, short, full string) (map[string]string, error) {
	env := map[string]string{"python_version": short, "python_full_version": full, "implementation_name": "cpython", "implementation_version": full,
		"platform_python_implementation": "CPython", "platform_release": "", "platform_version": "", "extra": ""}
	switch {
	case strings.HasPrefix(target, "linux-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "linux", "Linux", "posix"
		env["platform_machine"] = "x86_64"
		if strings.HasPrefix(target, "linux-arm64") {
			env["platform_machine"] = "aarch64"
		}
		if strings.HasSuffix(target, "-bionic") {
			env["platform_release"] = "android"
		}
	case strings.HasPrefix(target, "darwin-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "darwin", "Darwin", "posix"
		env["platform_machine"] = "x86_64"
		if target == "darwin-arm64" {
			env["platform_machine"] = "arm64"
		}
	case strings.HasPrefix(target, "win32-"):
		env["sys_platform"], env["platform_system"], env["os_name"] = "win32", "Windows", "nt"
		env["platform_machine"] = "AMD64"
		if target == "win32-arm64" {
			env["platform_machine"] = "ARM64"
		}
	default:
		return nil, fmt.Errorf("unknown target %s", target)
	}
	return env, nil
}

// Whether a PEP 508 marker holds in the environment, an empty marker holding everywhere
func evalMarker(marker string, env map[string]string) (bool, error) {
	if strings.TrimSpace(marker) == "" {
		return true, nil
	}
	p := &markerParser{tokens: tokenize(marker), env: env}
	ok, err := p.or()
	if err != nil {
		return false, fmt.Errorf("invalid marker %q: %w", marker, err)
	}
	if p.pos != len(p.tokens) {
		return false, fmt.Errorf("invalid marker %q: unexpected %q", marker, p.tokens[p.pos])
	}
	return ok, nil
}

// Words, operators, parentheses and quoted strings of a marker
func tokenize(marker string) []string {
	var tokens []string
	for i := 0; i < len(marker); {
		c := marker[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '(' || c == ')':
			tokens = append(tokens, string(c))
			i++
		case c == '\'' || c == '"':
			end := strings.IndexByte(marker[i+1:], c)
			if end < 0 {
				tokens = append(tokens, marker[i:])
				return tokens
			}
			tokens = append(tokens, marker[i:i+end+2])
			i += end + 2
		case strings.ContainsRune("<>=!~", rune(c)):
			j := i
			for j < len(marker) && strings.ContainsRune("<>=!~", rune(marker[j])) {
				j++
			}
			tokens = append(tokens, marker[i:j])
			i = j
		default:
			j := i
			for j < len(marker) && (unicode.IsLetter(rune(marker[j])) || unicode.IsDigit(rune(marker[j])) || strings.ContainsRune("_.-", rune(marker[j]))) {
				j++
			}
			if j == i {
				j++
			}
			tokens = append(tokens, marker[i:j])
			i = j
		}
	}
	return tokens
}

type markerParser struct {
	tokens []string
	pos    int
	env    map[string]string
}

func (p *markerParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *markerParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *markerParser) or() (bool, error) {
	left, err := p.and()
	for err == nil && p.peek() == "or" {
		p.next()
		var right bool
		if right, err = p.and(); err == nil {
			left = left || right
		}
	}
	return left, err
}

func (p *markerParser) and() (bool, error) {
	left, err := p.expr()
	for err == nil && p.peek() == "and" {
		p.next()
		var right bool
		if right, err = p.expr(); err == nil {
			left = left && right
		}
	}
	return left, err
}

func (p *markerParser) expr() (bool, error) {
	if p.peek() == "(" {
		p.next()
		ok, err := p.or()
		if err != nil {
			return false, err
		}
		if p.next() != ")" {
			return false, fmt.Errorf("missing )")
		}
		return ok, nil
	}
	leftToken := p.next()
	op := p.next()
	if op == "not" {
		if p.next() != "in" {
			return false, fmt.Errorf("expected in after not")
		}
		op = "not in"
	}
	rightToken := p.next()
	left, leftVar, err := p.value(leftToken)
	if err != nil {
		return false, err
	}
	right, rightVar, err := p.value(rightToken)
	if err != nil {
		return false, err
	}
	version := slices.Contains([]string{"python_version", "python_full_version", "implementation_version"}, leftVar) ||
		slices.Contains([]string{"python_version", "python_full_version", "implementation_version"}, rightVar)
	switch op {
	case "in":
		return strings.Contains(right, left), nil
	case "not in":
		return !strings.Contains(right, left), nil
	case "===":
		return left == right, nil
	case "==", "!=", "<", "<=", ">", ">=", "~=":
	default:
		return false, fmt.Errorf("unknown operator %q", op)
	}
	if !version || !versionLike(left) || !versionLike(right) {
		switch op {
		case "==":
			return left == right, nil
		case "!=":
			return left != right, nil
		}
		return false, fmt.Errorf("%s compares non-versions %q and %q", op, left, right)
	}
	if strings.HasSuffix(right, ".*") && (op == "==" || op == "!=") {
		prefix := strings.TrimSuffix(right, ".*")
		matched := left == prefix || strings.HasPrefix(left, prefix+".")
		return matched == (op == "=="), nil
	}
	c := compareVersion(left, right)
	switch op {
	case "==":
		return c == 0, nil
	case "!=":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	parts := strings.Split(right, ".")
	prefix := strings.Join(parts[:max(1, len(parts)-1)], ".")
	return c >= 0 && (left == prefix || strings.HasPrefix(left, prefix+".")), nil
}

// Value of a quoted string or marker variable, with the variable's name
func (p *markerParser) value(token string) (string, string, error) {
	if len(token) >= 2 && (token[0] == '\'' || token[0] == '"') && token[len(token)-1] == token[0] {
		return token[1 : len(token)-1], "", nil
	}
	if value, ok := p.env[token]; ok {
		return value, token, nil
	}
	return "", "", fmt.Errorf("unknown marker variable %q", token)
}

func versionLike(s string) bool {
	return s != "" && unicode.IsDigit(rune(s[0]))
}

// Order of two versions by their numeric release parts, a pre-release before its release
func compareVersion(a, b string) int {
	release := func(v string) ([]int, bool) {
		var parts []int
		pre := false
		for _, part := range strings.Split(strings.TrimSuffix(v, ".*"), ".") {
			digits := part
			for i, r := range part {
				if !unicode.IsDigit(r) {
					digits, pre = part[:i], true
					break
				}
			}
			n, _ := strconv.Atoi(digits)
			parts = append(parts, n)
			if pre {
				break
			}
		}
		return parts, pre
	}
	ra, preA := release(a)
	rb, preB := release(b)
	for len(ra) < len(rb) {
		ra = append(ra, 0)
	}
	for len(rb) < len(ra) {
		rb = append(rb, 0)
	}
	if c := slices.Compare(ra, rb); c != 0 {
		return c
	}
	switch {
	case preA && !preB:
		return -1
	case preB && !preA:
		return 1
	}
	return 0
}

// Targets a wheel serves under the pinned interpreter, read from the compatibility tags of its file name
func wheelTargets(file, short string) []string {
	name := file[strings.LastIndex(file, "/")+1:]
	name = strings.ReplaceAll(name, "%2B", "+")
	if !strings.HasSuffix(name, ".whl") {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(name, ".whl"), "-")
	if len(parts) < 5 {
		return nil
	}
	pythons, abis, platforms := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	own := strings.ReplaceAll(short, ".", "")
	ownMinor, _ := strconv.Atoi(strings.TrimPrefix(own, "3"))
	compatible := false
	for _, abi := range strings.Split(abis, ".") {
		for _, tag := range strings.Split(pythons, ".") {
			switch {
			case abi == "none" && (tag == "py3" || tag == "py"+own || tag == "cp"+own):
				compatible = true
			case abi == "cp"+own && tag == "cp"+own:
				compatible = true
			case abi == "abi3" && strings.HasPrefix(tag, "cp3"):
				if minor, err := strconv.Atoi(strings.TrimPrefix(tag, "cp3")); err == nil && minor <= ownMinor {
					compatible = true
				}
			}
		}
	}
	if !compatible {
		return nil
	}
	served := map[string]bool{}
	for _, platform := range strings.Split(platforms, ".") {
		for _, target := range platformTargets(platform) {
			served[target] = true
		}
	}
	out := make([]string, 0, len(served))
	for target := range served {
		out = append(out, target)
	}
	slices.Sort(out)
	return out
}

// Targets one wheel platform tag runs on
func platformTargets(tag string) []string {
	arch := func(x64, arm64 string) []string {
		switch {
		case strings.HasSuffix(tag, "_x86_64"):
			return []string{x64}
		case strings.HasSuffix(tag, "_aarch64"):
			return []string{arm64}
		}
		return nil
	}
	switch {
	case tag == "any":
		return slices.Clone(Targets)
	case strings.HasPrefix(tag, "manylinux") || tag == "linux_x86_64" || tag == "linux_aarch64":
		return arch("linux-x64", "linux-arm64")
	case strings.HasPrefix(tag, "musllinux"):
		return arch("linux-x64-musl", "linux-arm64-musl")
	case strings.HasPrefix(tag, "android_") && strings.HasSuffix(tag, "_arm64_v8a"):
		return []string{Bionic}
	case strings.HasPrefix(tag, "macosx_"):
		switch {
		case strings.HasSuffix(tag, "_universal2"):
			return []string{"darwin-arm64", "darwin-x64"}
		case strings.HasSuffix(tag, "_arm64"):
			return []string{"darwin-arm64"}
		case strings.HasSuffix(tag, "_x86_64") || strings.HasSuffix(tag, "_intel"):
			return []string{"darwin-x64"}
		}
	case tag == "win_amd64":
		return []string{"win32-x64"}
	case tag == "win_arm64":
		return []string{"win32-arm64"}
	}
	return nil
}
