package home

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Directory holding named profiles under a root
const profilesDir = "profiles"

// Symlinks one resolution follows before it keeps the rest of the path as written
const maxLinkHops = 40

// References a foreign environment may hold in its home, %NAME%, ${NAME} and $NAME
var foreignReference = regexp.MustCompile(`%([^%]+)%|\$\{([^}]+)\}|\$([A-Za-z0-9_-]+)`)

// Absolute form of path with every existing link followed and a missing remainder kept as written
func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return followLinks(abs, 0), nil
}

// Longest existing prefix of abs with its links followed, a dangling link followed to its target text
func followLinks(abs string, hops int) string {
	rest := ""
	for cur := abs; ; {
		if real, err := realPath(cur); err == nil {
			return filepath.Join(real, rest)
		}
		if target, err := os.Readlink(cur); err == nil && hops < maxLinkHops {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			return followLinks(filepath.Join(target, rest), hops+1)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Join(cur, rest)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// Path of p relative to root when p is root or lies below it, compared lexically
func within(p, root string) (string, bool) {
	p, root = filepath.Clean(p), filepath.Clean(root)
	if samePath(p, root) {
		return ".", true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if len(p) > len(prefix) && samePath(p[:len(prefix)], prefix) {
		return p[len(prefix):], true
	}
	return "", false
}

// Root holding home, the platform default when home lives under it, the parent of a profiles directory, or home itself
func rootOf(native, home string) (string, error) {
	if home == "" {
		return native, nil
	}
	realHome, err := resolvePath(home)
	if err != nil {
		return "", err
	}
	realNative, err := resolvePath(native)
	if err != nil {
		return "", err
	}
	if _, under := within(realHome, realNative); under {
		return native, nil
	}
	if parent := filepath.Dir(home); filepath.Base(parent) == profilesDir {
		return filepath.Dir(parent), nil
	}
	return home, nil
}

// Root that owns the profile tree of this process, derived from ICEBATH_HOME
func DefaultRoot() (string, error) { return RootOf(os.Getenv(EnvHome)) }

// Root that owns the profile tree of home, the platform default for an empty home
func RootOf(home string) (string, error) {
	native, err := PlatformDefault()
	if err != nil {
		return "", err
	}
	if h := strings.TrimSpace(home); h != "" {
		return rootOf(native, expandHome(h))
	}
	return native, nil
}

// Error for a foreign environment whose root depends on a relative home, an undefined variable or a missing profile folder
var ErrRootOutsideEnvironment = errors.New("root depends on more than the environment names")

// Root a process started with environ resolves, ErrRootOutsideEnvironment when environ alone cannot name it
func RootForEnvironment(environ map[string]string) (string, error) {
	names := make(map[string]string, len(environ))
	for k, v := range environ {
		names[strings.ToUpper(k)] = v
	}
	profile := strings.TrimSpace(names[profileVar])
	userHome := func() (string, error) {
		if profile == "" {
			return "", ErrRootOutsideEnvironment
		}
		return filepath.Clean(profile), nil
	}
	native, err := platformDefaultFrom(func(k string) string { return names[k] }, userHome)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(names[EnvHome])
	if raw == "" {
		return native, nil
	}
	undefined := false
	expanded := foreignReference.ReplaceAllStringFunc(raw, func(m string) string {
		sub := foreignReference.FindStringSubmatch(m)
		v, ok := names[strings.ToUpper(sub[1]+sub[2]+sub[3])]
		undefined = undefined || !ok
		return v
	})
	if undefined {
		return "", ErrRootOutsideEnvironment
	}
	if expanded == "~" || strings.HasPrefix(expanded, "~/") || strings.HasPrefix(expanded, `~\`) {
		h, err := userHome()
		if err != nil {
			return "", err
		}
		expanded = h + expanded[1:]
	}
	if !filepath.IsAbs(expanded) {
		return "", ErrRootOutsideEnvironment
	}
	return rootOf(native, filepath.Clean(expanded))
}

// Form of home shown to users, a tilde path below the account home and the path itself elsewhere
func Display(home string) (string, error) {
	h, err := UserHome()
	if err != nil {
		return "", err
	}
	if rel, ok := within(home, h); ok {
		return "~/" + filepath.ToSlash(rel), nil
	}
	return filepath.Clean(home), nil
}

// Stable key of a home directory, its resolved path, remembered once the directory exists
func (r *Resolver) Key(path string) (string, error) {
	raw := filepath.Clean(path)
	r.keyMu.Lock()
	defer r.keyMu.Unlock()
	if k, ok := r.keys[raw]; ok {
		return k, nil
	}
	expanded := ExpandUser(raw)
	if real, err := realPath(expanded); err == nil {
		k := normCase(real)
		r.keys[raw] = k
		return k, nil
	}
	lax, err := resolvePath(expanded)
	if err != nil {
		return "", err
	}
	return normCase(lax), nil
}

// Registry write scope key, empty for the process global layer and the home key otherwise
func (r *Resolver) NormalizeScope(scope string) (string, error) {
	if scope == "" {
		return "", nil
	}
	return r.Key(scope)
}
