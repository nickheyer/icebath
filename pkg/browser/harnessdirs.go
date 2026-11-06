package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	macosSocketDirBudget = 103 - len("/bu-.sock") - 64
	linuxSocketDirBudget = 107 - len("/bu-.sock") - 64
)

// Env keys that together with BU_NAME identify one harness daemon
var harnessDirKeys = []string{"BH_RUNTIME_DIR", "BH_RUNTIME_DIR_SHARED", "BH_TMP_DIR", "BH_TMP_DIR_SHARED"}

func envOr(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return fallback
}

func pathJoin(base string, parts ...string) string {
	return pypath.PosixPath(pypath.PosixJoin(base, parts...))
}

func expandUserPath(p string) string {
	if out, err := pypath.PathExpanduser(p); err == nil {
		return out
	}
	return p
}

func resolvePath(p string) string {
	if out, err := pypath.Resolve(p); err == nil {
		return out
	}
	return p
}

// The harness dir values plus BU_NAME of the daemon a call with env drives
func daemonIdentity(env map[string]string) []string {
	out := make([]string, 0, len(harnessDirKeys)+1)
	for _, k := range harnessDirKeys {
		out = append(out, env[k])
	}
	return append(out, envOr(env, "BU_NAME", "default"))
}

// Env repointed at the dirs and BU_NAME the identity names
func daemonEnv(env map[string]string, identity []string) map[string]string {
	target := make(map[string]string, len(env)+1)
	for k, v := range env {
		target[k] = v
	}
	for i, k := range harnessDirKeys {
		delete(target, k)
		if identity[i] != "" {
			target[k] = identity[i]
		}
	}
	target["BU_NAME"] = identity[len(identity)-1]
	return target
}

// Runtime dir byte budget where the harness binds AF_UNIX sockets, false on Windows
func socketDirBudget(platform string) (int, bool) {
	if platform == "darwin" {
		return macosSocketDirBudget, true
	}
	if strings.HasPrefix(platform, "linux") {
		return linuxSocketDirBudget, true
	}
	return 0, false
}

// Whether a 64 character session socket under the resolved dir fits the budget, no budget always fits
func fitsSocketBudget(resolvedDir string, budget int, hasBudget bool) bool {
	return !hasBudget || len(resolvedDir) <= budget
}

// Where the harness publishes the pid of the daemon a call with env drives
func harnessPidFile(env map[string]string) string {
	isolated := env["BH_RUNTIME_DIR"]
	if isolated == "" {
		isolated = env["BH_TMP_DIR"]
	}
	runtimeDir := pathJoin(harnessHome(env), "runtime")
	if isolated != "" {
		runtimeDir = pypath.PosixPath(expandUserPath(isolated))
	}
	if isolated == "" || env["BH_RUNTIME_DIR_SHARED"] == "1" {
		return pathJoin(runtimeDir, "bu-"+envOr(env, "BU_NAME", "default")+".pid")
	}
	return pathJoin(runtimeDir, "bu.pid")
}

func shortDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// Gives a served profile tag its own harness runtime and tmp dirs, an empty tag leaves env unchanged
func namespaceHarnessDirs(env map[string]string, tag string) {
	if tag == "" {
		return
	}
	root := harnessHome(env)
	leaf := "p-" + shortDigest(tag)
	base := env["BH_RUNTIME_DIR"]
	if base == "" {
		base = env["BH_TMP_DIR"]
	}
	if base == "" {
		base = pathJoin(root, "runtime")
	}
	env["BH_RUNTIME_DIR"] = socketFitRuntimeDir(pathJoin(base, leaf), base, tag)
	env["BH_RUNTIME_DIR_SHARED"] = "1"
	tmp := env["BH_TMP_DIR"]
	if tmp == "" {
		tmp = pathJoin(root, "tmp")
	}
	env["BH_TMP_DIR"] = pathJoin(tmp, leaf)
	env["BH_TMP_DIR_SHARED"] = "1"
}

// The browser harness home dir resolved against the child env
func harnessHome(env map[string]string) string {
	root := env["BH_HOME"]
	if root == "" {
		root = env["BROWSER_HARNESS_HOME"]
	}
	if root == "" {
		if xdg := env["XDG_CONFIG_HOME"]; xdg != "" {
			root = pathJoin(xdg, "browser-harness")
		} else {
			userHome := env["HOME"]
			if userHome == "" {
				userHome, _ = pypath.PathExpanduser("~")
			}
			root = pathJoin(userHome, ".config", "browser-harness")
		}
	}
	return pypath.PosixPath(expandUserPath(root))
}

// Moves the default runtime dir to a private short dir when the call's own socket would overflow sun_path
func fitDefaultSocketDir(env map[string]string) {
	if env["BH_RUNTIME_DIR"] != "" || env["BH_TMP_DIR"] != "" {
		return
	}
	budget, ok := socketDirBudget(runtime.GOOS)
	if !ok {
		return
	}
	root := harnessHome(env)
	name := env["BU_NAME"]
	if name == "" {
		name = "default"
	}
	stemBudget := budget + 64 - len(name)
	if fitsSocketBudget(resolvePath(pathJoin(root, "runtime")), stemBudget, true) {
		return
	}
	if short := claimShortDir(root+"\x00", stemBudget, true); short != "" {
		env["BH_RUNTIME_DIR"] = short
		env["BH_RUNTIME_DIR_SHARED"] = "1"
	}
}

// The runtime dir unless its resolved socket path overflows, then a private short dir when one can be claimed
func socketFitRuntimeDir(runtimeDir, base, tag string) string {
	budget, ok := socketDirBudget(runtime.GOOS)
	if fitsSocketBudget(resolvePath(expandUserPath(runtimeDir)), budget, ok) {
		return runtimeDir
	}
	if short := claimShortDir(base+"\x00"+tag, budget, ok); short != "" {
		return short
	}
	return runtimeDir
}

// A private hbh dir keyed by uid and key that fits the budget, under XDG_RUNTIME_DIR first then the socket temp root
func claimShortDir(key string, budget int, hasBudget bool) string {
	digest := shortDigest(strconv.Itoa(os.Getuid()) + "\x00" + key)
	var candidates []string
	if xdg := privateUserRuntimeDir(); xdg != "" {
		candidates = append(candidates, xdg)
	}
	if tmp, err := home.SocketSafeTmpdir(); err == nil {
		candidates = append(candidates, tmp)
	}
	candidates = append(candidates, "/tmp")
	seen := map[string]bool{}
	for _, raw := range candidates {
		root, err := pypath.Realpath(raw, pypath.Lax)
		if err != nil {
			root = raw
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		short := filepath.Join(root, "hbh-"+digest)
		if fitsSocketBudget(short, budget, hasBudget) && claimPrivateDir(short) {
			return short
		}
	}
	return ""
}

// XDG_RUNTIME_DIR or /run/user/uid when it is an absolute real dir owned by this uid and closed to others
func privateUserRuntimeDir() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	raw := os.Getenv("XDG_RUNTIME_DIR")
	if raw == "" {
		raw = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	if !filepath.IsAbs(raw) {
		return ""
	}
	info, err := os.Lstat(raw)
	if err != nil || !ownedPrivateDir(info) {
		return ""
	}
	return raw
}

// Claim seam over the real private dir creation
var claimPrivateDir = realClaim

// Creates the dir 0700 or accepts an existing real dir owned by this uid and closed to group and other
func realClaim(path string) bool {
	err := syscall.Mkdir(path, 0o700)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrExist) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && ownedPrivateDir(info)
}
