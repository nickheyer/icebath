package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Host folder sandboxes keep their files in, the sandbox folder setting over the profile's sandboxes folder
func SandboxDir(cfg *config.Config) (string, error) {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "sandboxes")
	if custom := strings.TrimSpace(cfg.GetString(terminalconfig.SandboxDir)); custom != "" {
		dir = ExpandPath(custom)
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// Characters a sandbox folder name may not hold
var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Name for a sandbox folder or container from a key: the key itself when it is safe, else a cleaned stem and a digest of it
func SafeName(key string) string {
	if key == "" {
		return "default"
	}
	cleaned := unsafeName.ReplaceAllString(key, "_")
	if cleaned == key && len(key) <= 128 && key != "." && key != ".." && !strings.HasSuffix(key, ".") {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	stem := strings.Trim(cleaned[:min(len(cleaned), 128-12-1)], "._")
	if stem == "" {
		stem = "task"
	}
	return stem + "-" + hex.EncodeToString(sum[:])[:12]
}

// Host folder shapes that can't exist in a container sandbox
var windowsDrive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Whether a path is a host user folder or a Windows drive path
func hostPath(p string) bool {
	return strings.HasPrefix(p, "/Users/") || strings.HasPrefix(p, "/home/") || windowsDrive.MatchString(p)
}

// Comparison key of a host path, slash style and drive letter case ignored
func HostKey(p string) string {
	text := strings.ReplaceAll(p, `\`, "/")
	if len(text) >= 2 && text[1] == ':' {
		text = strings.ToLower(text[:1]) + text[1:]
	}
	if len(text) > 3 {
		text = strings.TrimRight(text, "/")
	}
	return text
}

// Sandbox path of a host path under the mounted host folder, empty when it is not under it
func TranslateMounted(p, host, mount string) string {
	if p == "" || host == "" || mount == "" {
		return ""
	}
	key, root := HostKey(p), HostKey(host)
	if key == "" || root == "" {
		return ""
	}
	mount = strings.TrimRight(mount, "/")
	if mount == "" {
		mount = "/"
	}
	if key == root {
		return mount
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return mount + "/" + key[len(prefix):]
}

// Whether cwd names the mounted host folder itself
func mountedHost(cwd, host string) bool {
	if cwd == "" || host == "" {
		return false
	}
	left, err1 := filepath.Abs(ExpandUser(cwd))
	right, err2 := filepath.Abs(ExpandUser(host))
	return err1 == nil && err2 == nil && filepath.Clean(left) == filepath.Clean(right)
}

// Whether cwd is a host or relative path that can't be a container's folder
func unusableInContainer(cwd, host string) bool {
	if cwd == "" {
		return false
	}
	return mountedHost(cwd, host) || hostPath(cwd) || !path.IsAbs(cwd)
}

// Whether cwd is at or under the sandbox's home
func inSandboxHome(cwd string, e Environment) bool {
	home := e.SandboxHome()
	if home == "" || home == "/" || !path.IsAbs(home) || cwd == "" || !path.IsAbs(cwd) {
		return false
	}
	home, cwd = path.Clean(home), path.Clean(cwd)
	return cwd == home || strings.HasPrefix(cwd, home+"/")
}

// Whether a working folder value names no folder of its own
func Placeholder(cwd string) bool {
	for _, p := range terminalconfig.Placeholders {
		if strings.TrimSpace(cwd) == p {
			return true
		}
	}
	return false
}
