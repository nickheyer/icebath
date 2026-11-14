package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Runtime dir byte budgets, sun_path minus the bu-NAME.sock stem of a 64 character session
const (
	macosSocketDirBudget = 103 - len("/bu-.sock") - 64
	linuxSocketDirBudget = 107 - len("/bu-.sock") - 64
)

// Env keys that with BU_NAME identify one harness daemon
var HarnessDirKeys = [4]string{"BH_RUNTIME_DIR", "BH_RUNTIME_DIR_SHARED", "BH_TMP_DIR", "BH_TMP_DIR_SHARED"}

// Harness dir values then BU_NAME of the daemon a call with env drives
func DaemonIdentity(env map[string]string) [5]string {
	var identity [5]string
	for i, key := range HarnessDirKeys {
		identity[i] = env[key]
	}
	name, ok := env["BU_NAME"]
	if !ok {
		name = "default"
	}
	identity[4] = name
	return identity
}

// Copy of env pointed at the dirs and BU_NAME the identity names
func DaemonEnv(env map[string]string, identity [5]string) map[string]string {
	target := make(map[string]string, len(env)+1)
	for k, v := range env {
		target[k] = v
	}
	for i, key := range HarnessDirKeys {
		delete(target, key)
		if identity[i] != "" {
			target[key] = identity[i]
		}
	}
	target["BU_NAME"] = identity[4]
	return target
}

// Runtime dir byte budget of a sys.platform value where the harness binds AF_UNIX sockets, nil elsewhere
func SocketDirBudget(platform string) *int {
	budget := 0
	switch {
	case platform == "darwin":
		budget = macosSocketDirBudget
	case strings.HasPrefix(platform, "linux"):
		budget = linuxSocketDirBudget
	default:
		return nil
	}
	return &budget
}

// True when a 64 character session socket under the resolved dir fits the budget, always without one
func FitsSocketBudget(resolvedDir string, budget *int) bool {
	return budget == nil || len(resolvedDir) <= *budget
}

// Browser harness home dir resolved against the child env
func harnessHome(env map[string]string) (string, error) {
	root := env["BH_HOME"]
	if root == "" {
		root = env["BROWSER_HARNESS_HOME"]
	}
	if root == "" {
		if xdg := env["XDG_CONFIG_HOME"]; xdg != "" {
			root = pypath.PathJoin(xdg, "browser-harness")
		} else {
			base := env["HOME"]
			if base == "" {
				h, err := pypath.PathHome()
				if err != nil {
					return "", err
				}
				base = h
			}
			root = pypath.PathJoin(base, ".config", "browser-harness")
		}
	}
	return pypath.PathExpanduser(root)
}

// Pid file of the daemon a call with env drives, bu.pid inside an isolated dir no other name shares
func HarnessPidFile(env map[string]string) (string, error) {
	isolated := env["BH_RUNTIME_DIR"]
	if isolated == "" {
		isolated = env["BH_TMP_DIR"]
	}
	var runtimeDir string
	if isolated != "" {
		expanded, err := pypath.PathExpanduser(isolated)
		if err != nil {
			return "", err
		}
		runtimeDir = expanded
	} else {
		root, err := harnessHome(env)
		if err != nil {
			return "", err
		}
		runtimeDir = pypath.PathJoin(root, "runtime")
	}
	if isolated != "" && env["BH_RUNTIME_DIR_SHARED"] != "1" {
		return pypath.PathJoin(runtimeDir, "bu.pid"), nil
	}
	name, ok := env["BU_NAME"]
	if !ok {
		name = "default"
	}
	return pypath.PathJoin(runtimeDir, "bu-"+name+".pid"), nil
}

// First twelve hex digits of the SHA-256 of text
func sha12(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

// Gives a served profile tag its own harness runtime and tmp dirs in the child env, an empty tag changing nothing
func NamespaceHarnessDirs(env map[string]string, tag string) error {
	if tag == "" {
		return nil
	}
	root, err := harnessHome(env)
	if err != nil {
		return err
	}
	leaf := "p-" + sha12(tag)
	base := env["BH_RUNTIME_DIR"]
	if base == "" {
		base = env["BH_TMP_DIR"]
	}
	if base == "" {
		base = pypath.PathJoin(root, "runtime")
	}
	runtimeDir, err := SocketFitRuntimeDir(pypath.PathJoin(base, leaf), base, tag)
	if err != nil {
		return err
	}
	env["BH_RUNTIME_DIR"] = runtimeDir
	env["BH_RUNTIME_DIR_SHARED"] = "1"
	tmp := env["BH_TMP_DIR"]
	if tmp == "" {
		tmp = pypath.PathJoin(root, "tmp")
	}
	env["BH_TMP_DIR"] = pypath.PathJoin(tmp, leaf)
	env["BH_TMP_DIR_SHARED"] = "1"
	return nil
}

// Moves a default runtime dir whose socket for the call's BU_NAME overflows sun_path to a private short dir
func FitDefaultSocketDir(env map[string]string) error {
	if env["BH_RUNTIME_DIR"] != "" || env["BH_TMP_DIR"] != "" {
		return nil
	}
	budget := SocketDirBudget(sysPlatform())
	if budget == nil {
		return nil
	}
	root, err := harnessHome(env)
	if err != nil {
		return err
	}
	name := env["BU_NAME"]
	if name == "" {
		name = "default"
	}
	stemBudget := *budget + 64 - utf8.RuneCountInString(name)
	resolved, err := pypath.Resolve(pypath.PathJoin(root, "runtime"))
	if err != nil {
		return err
	}
	if FitsSocketBudget(resolved, &stemBudget) {
		return nil
	}
	short, err := claimShortDir(root+"\x00", &stemBudget)
	if err != nil {
		return err
	}
	if short != "" {
		env["BH_RUNTIME_DIR"] = short
		env["BH_RUNTIME_DIR_SHARED"] = "1"
	}
	return nil
}

// The runtime dir as given when its resolved socket path fits, else a private short dir keyed by base and tag
func SocketFitRuntimeDir(runtimeDir, base, tag string) (string, error) {
	budget := SocketDirBudget(sysPlatform())
	expanded, err := pypath.PathExpanduser(runtimeDir)
	if err != nil {
		return "", err
	}
	resolved, err := pypath.Resolve(expanded)
	if err != nil {
		return "", err
	}
	if FitsSocketBudget(resolved, budget) {
		return runtimeDir, nil
	}
	short, err := claimShortDir(base+"\x00"+tag, budget)
	if err != nil {
		return "", err
	}
	if short != "" {
		return short, nil
	}
	return runtimeDir, nil
}

// Private hbh dir keyed by uid and key that fits the budget, under the user runtime dir first then the temp roots
func claimShortDir(key string, budget *int) (string, error) {
	digest := sha12(strconv.Itoa(harnessUID()) + "\x00" + key)
	var candidates []string
	if xdg := PrivateUserRuntimeDir(); xdg != "" {
		candidates = append(candidates, xdg)
	}
	tmp, err := home.SocketSafeTmpdir()
	if err != nil {
		return "", err
	}
	candidates = append(candidates, tmp, "/tmp")
	seen := map[string]bool{}
	for _, candidate := range candidates {
		root, err := pypath.Realpath(candidate, pypath.Lax)
		if err != nil {
			return "", err
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		short := pypath.Join(root, "hbh-"+digest)
		if FitsSocketBudget(short, budget) && ClaimPrivateDir(short) {
			return short, nil
		}
	}
	return "", nil
}

// The user runtime dir when it is an absolute real directory of this uid closed to group and other
func PrivateUserRuntimeDir() string {
	if !harnessHasUID {
		return ""
	}
	raw := os.Getenv("XDG_RUNTIME_DIR")
	if raw == "" {
		raw = "/run/user/" + strconv.Itoa(harnessUID())
	}
	if !pypath.IsAbs(raw) {
		return ""
	}
	info, err := os.Lstat(raw)
	if err != nil || !ownedPrivateDir(info) {
		return ""
	}
	return raw
}

// Creates the dir 0700, or accepts an existing real directory of this uid closed to group and other
func ClaimPrivateDir(path string) bool {
	err := os.Mkdir(path, 0o700)
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrExist) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && ownedPrivateDir(info)
}

// sys.platform value of the host
func sysPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}
