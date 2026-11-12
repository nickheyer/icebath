package files

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	relPathMax = 1024
	segmentMax = 255
)

var (
	pluginIDRE     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,126}[A-Za-z0-9_-])?$`)
	wslShareRE     = regexp.MustCompile(`(?i)^\\\\(?:wsl\.localhost|wsl\$)\\[^\\]+`)
	driveLetterRE  = regexp.MustCompile(`^[a-zA-Z]:`)
	segmentSplitRE = regexp.MustCompile(`[\\/]+`)
	winTrailingRE  = regexp.MustCompile(`[. ]$`)
)

var systemRootsPosix = []string{"/proc", "/sys", "/dev"}

var wslSystemDirs = map[string]bool{"dev": true, "mnt": true, "proc": true, "run": true, "sys": true}

// Path of child relative to parent, rel inside or the folder itself
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// pathTools holds the containment rules of one platform
type pathTools struct {
	platform string
	caseless bool
}

func newPathTools(platform string) pathTools {
	return pathTools{platform: platform, caseless: platform == "win32" || platform == "darwin"}
}

func (p pathTools) inside(child, parent string) bool { return within(child, parent) }

// Containment that also matches case insensitively where volumes usually are
func (p pathTools) insideLoose(child, parent string) bool {
	return within(child, parent) || (p.caseless && within(jsToLower(child), jsToLower(parent)))
}

func (p pathTools) same(a, b string) bool { return resolvePath(a) == resolvePath(b) }

// Plain segments of a plugin relative path, refusing anything that names a place outside
func (p pathTools) segments(raw any) ([]string, error) {
	s, ok := raw.(string)
	if !ok || len(utf16Units(s)) > relPathMax || strings.ContainsRune(s, 0) {
		return nil, refuse("invalid-path", "Path must be a relative path inside the folder.")
	}
	if strings.HasPrefix(s, "/") || win32IsAbsolute(s) || driveLetterRE.MatchString(s) {
		return nil, refuse("invalid-path", "Path must be relative to the folder, not absolute.")
	}
	var parts []string
	for _, part := range segmentSplitRE.Split(s, -1) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	for _, part := range parts {
		if part == ".." || len(utf16Units(part)) > segmentMax || jsToLower(part) == "..namedfork" {
			return nil, refuse("invalid-path", "Path may not leave the folder.")
		}
		if p.platform == "win32" && (strings.Contains(part, ":") || winTrailingRE.MatchString(part) || winReserved(part)) {
			return nil, refuse("invalid-path", "Path segment is not valid on Windows.")
		}
	}
	return parts, nil
}

// Node path.win32.isAbsolute
func win32IsAbsolute(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '/' || s[0] == '\\' {
		return true
	}
	return len(s) > 2 && driveLetterRE.MatchString(s) && (s[2] == '/' || s[2] == '\\')
}

// JS toLowerCase, the dotted capital I lowered to i with a combining dot
func jsToLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307"))
}

func utf16Units(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r >= 0x10000 {
			out = append(out, 0, 0)
		} else {
			out = append(out, 0)
		}
	}
	return out
}

// Plugin id as a data folder key, refused unless it is a plain slug no platform aliases
func pluginKey(pluginID any) (string, error) {
	s, ok := pluginID.(string)
	if ok && pluginIDRE.MatchString(s) && !winReserved(s) {
		return s, nil
	}
	return "", refuse("invalid-plugin", "Plugin id is not a plain slug.")
}

// Data folder name of a plugin, ids with capitals suffixed so case insensitive volumes keep them apart
func dataDirName(pluginID string) string {
	if pluginID == strings.ToLower(pluginID) {
		return pluginID
	}
	sum := sha256.Sum256([]byte(pluginID))
	return strings.ToLower(pluginID) + "~" + hex.EncodeToString(sum[:])[:8]
}

func systemRoots(platform string) []string {
	if platform == "win32" {
		return nil
	}
	return systemRootsPosix
}

// Realpaths of the roots, a missing root kept resolved, duplicates dropped
func realRoots(roots []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		real, err := realpathOrEmpty(root)
		if err != nil {
			return nil, err
		}
		if real == "" {
			real = resolvePath(root)
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, real)
		}
	}
	return out, nil
}

// A WSL share path that holds Icebath data or is not the distro's own files
func WslProtectedPath(candidate string) bool {
	share := wslShareRE.FindString(candidate)
	if share == "" {
		return false
	}
	var rest []string
	for _, part := range strings.Split(candidate[len(share):], `\`) {
		if part != "" {
			rest = append(rest, jsToLower(part))
		}
	}
	holdsHome := len(rest) == 0 || (len(rest) == 1 && (rest[0] == "home" || rest[0] == "root")) ||
		(len(rest) == 2 && rest[0] == "home")
	if holdsHome {
		return true
	}
	if wslSystemDirs[rest[0]] {
		return true
	}
	for _, part := range rest {
		if part == ".icebath" {
			return true
		}
	}
	return false
}

var loopbackHosts = map[string]bool{"localhost": true, "::1": true, "[::1]": true, "0--1.ipv6-literal.net": true}

var (
	uncPrefixRE  = regexp.MustCompile(`(?i)^\\\\\?\\UNC\\`)
	devicePathRE = regexp.MustCompile(`^\\\\[?.]\\`)
	uncHostRE    = regexp.MustCompile(`^\\\\([^\\]+)`)
	loopbackV4RE = regexp.MustCompile(`^127\.`)
)

// A Windows path that reaches this machine's own drives a second way, a device path or a share it serves
func LoopbackShare(candidate string) bool {
	unc := candidate
	if uncPrefixRE.MatchString(candidate) {
		unc = `\\` + candidate[8:]
	}
	if devicePathRE.MatchString(unc) {
		return true
	}
	m := uncHostRE.FindStringSubmatch(unc)
	if m == nil {
		return false
	}
	host := jsToLower(strings.SplitN(m[1], "@", 2)[0])
	if host == "" {
		return false
	}
	own := map[string]bool{}
	if name, err := os.Hostname(); err == nil {
		own[jsToLower(name)] = true
	}
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					own[jsToLower(ipnet.IP.String())] = true
				}
			}
		}
	}
	return loopbackHosts[host] || loopbackV4RE.MatchString(host) || own[host] || own[strings.SplitN(host, ".", 2)[0]]
}

// Identity of a path as dev and ino, empty when it cannot be stat'd
func identity(target string) string {
	st, err := pfs.stat(target)
	if err != nil {
		return ""
	}
	dev, ino, ok := fileIdentity(st, target)
	if !ok {
		return ""
	}
	return identityKey(dev, ino)
}

func identityKey(dev, ino uint64) string {
	return jsNumber(float64(dev)) + ":" + jsNumber(float64(ino))
}

// Identities of a path and every folder above it
func identityChain(target string) []string {
	var ids []string
	for current := target; ; current = filepath.Dir(current) {
		if id := identity(current); id != "" {
			ids = append(ids, id)
		}
		if filepath.Dir(current) == current {
			return ids
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Overlap by file system identity, catching bind mounts, firmlinks and case insensitive spellings
func OverlapsByIdentity(candidate string, roots []string) bool {
	self := identity(candidate)
	chain := identityChain(candidate)
	for _, root := range roots {
		rootID := identity(root)
		if rootID != "" && (containsString(chain, rootID) || (self != "" && containsString(identityChain(root), self))) {
			return true
		}
	}
	return false
}

// True when target or a folder between it and root is one of the protected roots by identity
func protectedIdentityWithin(target, root string, roots []string) bool {
	ids := map[string]bool{}
	for _, r := range roots {
		if id := identity(r); id != "" {
			ids[id] = true
		}
	}
	for current := target; ; current = filepath.Dir(current) {
		if id := identity(current); id != "" && ids[id] {
			return true
		}
		if current == root || filepath.Dir(current) == current {
			return false
		}
	}
}
