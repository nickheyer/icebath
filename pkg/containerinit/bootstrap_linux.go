package containerinit

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Data volume entries handed back to the service account when a chown is needed
var (
	ownedSubdirs = []string{"cron", "sessions", "logs", "hooks", "memories", "skills", "skins", "plans", "workspace",
		"home", "profiles", "pairing", "platforms/pairing"}
	ownedFiles = []string{"auth.json", "auth.lock", ".env", "state.db", "state.db-shm", "state.db-wal", "icebath_state.db",
		"response_store.db", "response_store.db-shm", "response_store.db-wal", "gateway.pid", "gateway.lock",
		"gateway_state.json", "processes.json", "active_profile"}
	seededDirs = []string{"backups", "cron", "sessions", "logs", "logs/gateways", "hooks", "memories", "skills", "skins",
		"plans", "workspace", "home", "pairing", "platforms/pairing"}
	routingOverrides = []string{"ICEBATH_PORTAL_BASE_URL", "NOUS_PORTAL_BASE_URL", "NOUS_INFERENCE_BASE_URL"}
	apiKeyLine       = regexp.MustCompile(`(?m)^API_SERVER_KEY=.+`)
)

const routingMark = "# stage2-managed"

// Boot time preparation of the data volume the init runs before any service starts
type bootstrap struct {
	home string
	root bool
	exe  string
	acct account
}

func say(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Numeric id in the range the remap accepts
func validID(v string) bool {
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(v)
	return err == nil && n >= 1 && n <= 65534
}

// True when any component of path up to root is a symlink
func pathHasSymlinkComponent(path, root string) bool {
	for path != "" && path != "/" {
		if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return true
		}
		if path == root {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return false
}

// Refuses an action through a symlinked path with a warning, true when refused
func (b *bootstrap) refuse(action, target string) bool {
	if pathHasSymlinkComponent(target, b.home) {
		say("[stage2] Warning: refusing %s through symlinked path %s — continuing", action, target)
		return true
	}
	return false
}

func (b *bootstrap) chown(p string) error { return os.Chown(p, b.acct.uid, b.acct.gid) }

// Recursive chown that never follows a symlink, warning when the volume refuses it
func (b *bootstrap) chownTree(target string) {
	if b.refuse("recursive chown", target) {
		return
	}
	failed := false
	filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			failed = true
			return nil
		}
		if os.Lchown(p, b.acct.uid, b.acct.gid) != nil {
			failed = true
		}
		return nil
	})
	if failed {
		say("[stage2] Warning: chown %s failed (rootless container?) — continuing", target)
	}
}

// True when any entry of the tree has another owner or group than the service account
func (b *bootstrap) treeHasNonOwner(target string) bool {
	found := false
	filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != b.acct.uid || int(st.Gid) != b.acct.gid) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// Runs argv as the service account when the init is root
func (b *bootstrap) asIcebath(argv ...string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if b.root {
		cmd.SysProcAttr = b.acct.credential()
	}
	return cmd.Run()
}

// Creates an empty private file owned by the service account
func (b *bootstrap) touchPrivate(p string) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if b.root {
		return b.chown(p)
	}
	return nil
}

// Remaps the account to ICEBATH_UID and ICEBATH_GID, or the PUID and PGID aliases
func (b *bootstrap) remap() error {
	uid := os.Getenv("ICEBATH_UID")
	if uid == "" {
		uid = os.Getenv("PUID")
	}
	gid := os.Getenv("ICEBATH_GID")
	if gid == "" {
		gid = os.Getenv("PGID")
	}
	if uid != "" && validID(uid) && uid != strconv.Itoa(b.acct.uid) {
		say("[stage2] Changing icebath UID to %s", uid)
		if err := shadowTool("usermod", "-u", uid, accountName); err != nil {
			return err
		}
	}
	if gid != "" && validID(gid) && gid != strconv.Itoa(b.acct.gid) {
		say("[stage2] Changing icebath GID to %s", gid)
		shadowToolQuiet("groupmod", "-o", "-g", gid, accountName)
	}
	return nil
}

// Adds the account to the group owning a mounted Docker socket, creating hostdocker when the gid has no name
func (b *bootstrap) dockerSocketGroup() {
	for _, sock := range dockerSockets {
		info, err := os.Stat(sock)
		if err != nil || info.Mode()&fs.ModeSocket == 0 {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		gid := int(st.Gid)
		for _, g := range b.acct.groups {
			if g == gid {
				say("[stage2] icebath already in group %d for %s", gid, sock)
				return
			}
		}
		name, found := groupName(gid)
		if !found {
			name = "hostdocker"
			if shadowToolQuiet("groupadd", "-g", strconv.Itoa(gid), name) != nil {
				say("[stage2] Warning: groupadd -g %d %s failed; skipping docker socket group setup", gid, name)
				return
			}
			say("[stage2] Created group %s (GID %d) for Docker socket", name, gid)
		}
		if shadowToolQuiet("usermod", "-aG", name, accountName) == nil {
			say("[stage2] Added icebath to group %s (GID %d) for %s", name, gid, sock)
		} else {
			say("[stage2] Warning: usermod -aG %s icebath failed; docker backend may fail with EACCES", name)
		}
		return
	}
}

// Hands the data volume and its runtime files back to the account without following symlinks
func (b *bootstrap) fixOwnership() {
	h := b.home
	if info, err := os.Stat(h); err != nil || int(info.Sys().(*syscall.Stat_t).Uid) != b.acct.uid {
		say("[stage2] Fixing ownership of %s (targeted) to icebath (%d)", h, b.acct.uid)
		if !b.refuse("chown", h) && b.chown(h) != nil {
			say("[stage2] Warning: chown %s failed (rootless container?) — continuing", h)
		}
		for _, sub := range ownedSubdirs {
			p := filepath.Join(h, sub)
			if exists(p) && b.treeHasNonOwner(p) {
				b.chownTree(p)
			}
		}
	}
	for _, sub := range []string{"profiles", "cron"} {
		if p := filepath.Join(h, sub); isDir(p) && b.treeHasNonOwner(p) {
			b.chownTree(p)
		}
	}
	if p := filepath.Join(h, "logs", "gateways"); isDir(p) && !b.refuse("chown", p) {
		b.chown(p)
	}
	for _, sub := range []string{"platforms/pairing", "pairing"} {
		if p := filepath.Join(h, sub); isDir(p) && b.treeHasNonOwner(p) {
			b.chownTree(p)
		}
	}
	for _, f := range ownedFiles {
		if p := filepath.Join(h, f); exists(p) && !b.refuse("chown", p) {
			b.chown(p)
		}
	}
	if p := filepath.Join(h, "config.yaml"); isFile(p) && !b.refuse("chown/chmod", p) {
		b.chown(p)
		os.Chmod(p, 0o640)
	}
}

// Creates the container scoped runtime dir private to the account, refusing one another uid owns
func (b *bootstrap) runtimeDir() {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return
	}
	owner := ""
	if exists(dir) {
		owner = "unknown"
		if info, err := os.Stat(dir); err == nil {
			owner = strconv.Itoa(int(info.Sys().(*syscall.Stat_t).Uid))
		}
	}
	switch {
	case b.refuse("create", dir):
	case owner != "" && owner != "0" && owner != strconv.Itoa(b.acct.uid):
		say("[stage2] Warning: %s is owned by uid %s (not root or icebath) — refusing to adopt it", dir, owner)
	default:
		if os.MkdirAll(dir, 0o755) != nil {
			say("[stage2] Warning: could not create XDG_RUNTIME_DIR %s (continuing)", dir)
		}
		if isDir(dir) {
			if b.chown(dir) != nil {
				say("[stage2] Warning: could not chown XDG_RUNTIME_DIR %s (rootless?)", dir)
			}
			os.Chmod(dir, 0o700)
		}
	}
}

// Copies an image default into the volume on first boot
func (b *bootstrap) seed(dest, src string) error {
	d, s := filepath.Join(b.home, dest), filepath.Join(installDir, src)
	if isFile(d) || !isFile(s) || b.refuse("seed", d) {
		return nil
	}
	return b.asIcebath("cp", s, d)
}

// Lines of a file that do not match re, joined and without trailing newlines, the grep -v the image used
func linesWithout(path string, re *regexp.Regexp) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var kept []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if !re.MatchString(sc.Text()) {
			kept = append(kept, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n"), nil
}

// Drops the lines matching re from path and appends line when given, warning on read or write failure
func rewriteEnvVar(path, name string, re *regexp.Regexp, line string) bool {
	rest, err := linesWithout(path, re)
	if err != nil {
		say("[stage2] Warning: could not read %s — leaving %s untouched", path, name)
		return false
	}
	if line != "" {
		if rest != "" {
			rest += "\n"
		}
		rest += line
	}
	if rest != "" {
		rest += "\n"
	}
	if err := os.WriteFile(path, []byte(rest), 0o600); err != nil {
		say("[stage2] Warning: could not write %s to %s (read-only volume?) — routed turns will fall back to the production Portal", name, path)
		return false
	}
	return true
}

// True when any line of path matches re
func fileMatches(path string, re *regexp.Regexp) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// Mirrors the Nous routing overrides of the container environment into one env file as managed lines
func (b *bootstrap) syncRoutingOverrides(path string) {
	if b.refuse("sync", path) {
		return
	}
	for _, name := range routingOverrides {
		value := os.Getenv(name)
		managed := regexp.MustCompile("^" + name + "=.* " + regexp.QuoteMeta(routingMark) + "$")
		if value == "" {
			if fileMatches(path, managed) && rewriteEnvVar(path, name, managed, "") {
				say("[stage2] Removed %s from %s (no longer set in the container environment)", name, path)
			}
			continue
		}
		line := name + "=" + value + " " + routingMark
		if raw, err := os.ReadFile(path); err == nil && containsLine(string(raw), line) {
			continue
		}
		if !isFile(path) && b.touchPrivate(path) != nil {
			say("[stage2] Warning: could not create %s — the Nous routing overrides will not reach this profile's secret scope", path)
			return
		}
		if rewriteEnvVar(path, name, regexp.MustCompile("^"+name+"="), line) {
			say("[stage2] Synced %s from the container environment into %s", name, path)
		}
	}
}

func containsLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

// Keeps a loopback api_server key in the volume env unless the container environment supplies one
func (b *bootstrap) apiServerKey() {
	envFile := filepath.Join(b.home, ".env")
	emptyKey := regexp.MustCompile(`^API_SERVER_KEY=$`)
	raw, _ := os.ReadFile(envFile)
	if key := os.Getenv("API_SERVER_KEY"); key != "" {
		if isFile(envFile) && apiKeyLine.Match(raw) {
			say("[stage2] Warning: API_SERVER_KEY is set in both the container environment and %s — the .env value wins at runtime (loaded with override=True)", envFile)
			return
		}
		if len(key) < 16 {
			say("[stage2] Warning: container-provided API_SERVER_KEY is shorter than 16 characters — the gateway api_server will refuse to start (cron fires unavailable). Generate a strong secret, e.g. `openssl rand -hex 32`.")
		}
		if isFile(envFile) && !b.refuse("clean", envFile) {
			if rest, err := linesWithout(envFile, emptyKey); err == nil {
				writeKeepingMode(envFile, rest)
			}
		}
		say("[stage2] API_SERVER_KEY provided via container environment — skipping generation")
		return
	}
	if apiKeyLine.Match(raw) || b.refuse("append", envFile) {
		return
	}
	if !exists(envFile) {
		b.touchPrivate(envFile)
	}
	if !isFile(envFile) {
		say("[stage2] Warning: could not create %s — gateway api_server (cron fires) will be unavailable", envFile)
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return
	}
	if rest, err := linesWithout(envFile, emptyKey); err == nil {
		writeKeepingMode(envFile, rest)
	}
	f, err := os.OpenFile(envFile, os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = f.WriteString("API_SERVER_KEY=" + hex.EncodeToString(buf) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		say("[stage2] Warning: could not write API_SERVER_KEY to %s (read-only volume?) — gateway api_server (cron fires) will be unavailable", envFile)
		return
	}
	say("[stage2] Generated API_SERVER_KEY for the loopback gateway api_server")
}

// Rewrites a file in place with rest and a trailing newline when rest is not empty
func writeKeepingMode(path, rest string) {
	if rest != "" {
		rest += "\n"
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	f.WriteString(rest)
	f.Close()
}

// Runs one delegated step as the account, warning with text when it fails
func (b *bootstrap) step(warning string, args ...string) {
	if err := b.asIcebath(append([]string{b.exe, "container-stage"}, args...)...); err != nil && warning != "" {
		say("%s", warning)
	}
}

// Prepares the data volume, the Go form of the image's stage2 hook, stopping at the first fatal step
func runBootstrap(exe string) error {
	b := &bootstrap{home: DataHome(), root: os.Getuid() == 0, exe: exe}
	if err := os.MkdirAll(b.home, 0o755); err != nil {
		return err
	}
	acct, err := lookupAccount(accountName)
	if err != nil {
		return err
	}
	b.acct = acct
	if err := b.remap(); err != nil {
		return err
	}
	if b.acct, err = lookupAccount(accountName); err != nil {
		return err
	}
	b.dockerSocketGroup()
	if b.acct, err = lookupAccount(accountName); err != nil {
		return err
	}
	b.fixOwnership()
	dirs := make([]string, 0, len(seededDirs))
	for _, d := range seededDirs {
		dirs = append(dirs, filepath.Join(b.home, d))
	}
	if err := b.asIcebath(append([]string{"mkdir", "-p"}, dirs...)...); err != nil {
		return err
	}
	b.runtimeDir()
	if raw, err := os.ReadFile(filepath.Join(b.home, ".install_method")); err == nil && strings.Join(strings.Fields(string(raw)), "") == "docker" {
		os.Remove(filepath.Join(b.home, ".install_method"))
	}
	for _, pair := range [][2]string{{".env", ".env.example"}, {"config.yaml", "cli-config.yaml.example"}, {"SOUL.md", "docker/SOUL.md"}} {
		if err := b.seed(pair[0], pair[1]); err != nil {
			return err
		}
	}
	b.apiServerKey()
	b.syncRoutingOverrides(filepath.Join(b.home, ".env"))
	profiles, _ := filepath.Glob(filepath.Join(b.home, "profiles", "*"))
	sort.Strings(profiles)
	for _, p := range profiles {
		if isDir(p) {
			b.syncRoutingOverrides(filepath.Join(p, ".env"))
		}
	}
	if envFile := filepath.Join(b.home, ".env"); isFile(envFile) && !b.refuse("chown/chmod", envFile) {
		b.chown(envFile)
		os.Chmod(envFile, 0o600)
	}
	if info, err := os.Stat(flyAPISocket); err == nil && info.Mode()&fs.ModeSocket != 0 && !b.refuse("chgrp/chmod", flyAPISocket) {
		if os.Chown(flyAPISocket, -1, b.acct.gid) == nil && os.Chmod(flyAPISocket, info.Mode().Perm()|0o020) == nil {
			say("[stage2] Granted icebath group access to the Fly Machines API socket")
		} else {
			say("[stage2] Warning: could not grant group access to /.fly/api — scale-to-zero self-suspend will fail EACCES (fail-awake)")
		}
	}
	b.step("", "config-migrate")
	b.step("[stage2] Warning: dependency refresh failed; continuing", "dependencies")
	if err := b.authBootstrap(); err != nil {
		return err
	}
	b.step("[stage2] Warning: skills sync failed; continuing", "skills-sync")
	if raw, err := os.ReadFile(chromiumPin); err == nil {
		bin := string(raw)
		if info, err := os.Stat(bin); bin != "" && err == nil && info.Mode()&0o111 != 0 {
			say("[stage2] Using pinned Chromium binary: %s", bin)
		} else {
			shown := bin
			if shown == "" {
				shown = "<empty>"
			}
			say("[stage2] Warning: baked Chromium binary is missing (%s); browser_exec cannot launch the pinned browser", shown)
		}
	}
	say("[stage2] Setup complete; starting user services")
	return nil
}

// Seeds auth.json and the gateway state from the bootstrap variables and re-seeds a terminal Nous session
func (b *bootstrap) authBootstrap() error {
	auth := filepath.Join(b.home, "auth.json")
	if seed := os.Getenv("ICEBATH_AUTH_JSON_BOOTSTRAP"); !exists(auth) && seed != "" && !b.refuse("seed", auth) {
		if err := os.WriteFile(auth, []byte(seed), 0o644); err != nil {
			return err
		}
		b.chown(auth)
		if err := os.Chmod(auth, 0o600); err != nil {
			return err
		}
	}
	if isFile(auth) && os.Getenv(RebootstrapEnv) != "" && !b.refuse("reseed", auth) {
		b.step("[stage2] Warning: Nous session re-bootstrap failed; continuing", "rebootstrap", auth)
	}
	state := filepath.Join(b.home, "gateway_state.json")
	if !exists(state) && os.Getenv("ICEBATH_GATEWAY_BOOTSTRAP_STATE") == "running" && !b.refuse("seed", state) {
		if err := os.WriteFile(state, []byte("{\"gateway_state\":\"running\"}\n"), 0o644); err != nil {
			return err
		}
		b.chown(state)
		if err := os.Chmod(state, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Error text of the unsupported arbitrary uid start, printed by the bootstrap and by the main program launcher
func arbitraryUIDErrors(uid int) (string, string) {
	stage := fmt.Sprintf(`[stage2] ERROR: container started with --user %d (an arbitrary, non-icebath UID).
This is not supported under the supervised image. The container bootstrap
(UID remap, data-volume ownership, config seeding) needs to start as root,
and the baked /opt/icebath install tree is intentionally root-owned and
non-writable, so a pinned --user UID cannot repair startup state — startup
will fail.

To make container-written files match your HOST user, DON'T use --user.
Start the container as root (the default) and pass your host UID/GID instead:

    docker run -e ICEBATH_UID=$(id -u) -e ICEBATH_GID=$(id -g) ...

NAS users (Synology / unRAID / UGOS) can use the PUID/PGID aliases:

    docker run -e PUID=$(id -u) -e PGID=$(id -g) ...

The image remaps the icebath user to that UID/GID at boot and chowns the data
volume accordingly, so files land owned by your host user — the same outcome
--user was being used for, without breaking the supervision tree.
`, uid)
	wrapper := fmt.Sprintf(`[icebath] ERROR: container started with --user %d (an arbitrary, non-icebath UID) — not supported.
To make container-written files match your HOST user, don't use --user.
Start as root (the default) and pass your host UID/GID instead:
    docker run -e ICEBATH_UID=$(id -u) -e ICEBATH_GID=$(id -g) ...
NAS users (Synology / unRAID / UGOS) can use the PUID/PGID aliases:
    docker run -e PUID=$(id -u) -e PGID=$(id -g) ...
The image remaps the icebath user to that UID/GID at boot and chowns the data
volume, so files land owned by your host user — the same outcome --user gave,
without breaking the supervision tree.
`, uid)
	return stage, wrapper
}
