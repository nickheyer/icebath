package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// File beside a container's sandbox folders listing what was handed to the host user, given back to root on the next start
	handbackRecord = ".icebath-owner-handback"
	// Longest an ownership walk and change may take
	chownTimeout = 10 * time.Second
)

// A host folder bound into a container and where the container sees it
type bind struct {
	host, sandbox string
}

// Environment of the throwaway containers that walk and change ownership, untouched by the image's own
var safeEnv = []string{"-e", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "-e", "LD_PRELOAD=", "-e", "LD_LIBRARY_PATH=", "-w", "/"}

// Confinement of a throwaway container: only the capabilities changing ownership takes
var throwawayConfinement = []string{"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER",
	"--security-opt", "no-new-privileges", "--pids-limit", "64", "--label", "icebath-handback=1"}

// Whether the daemon maps container users to others of the host, rootless or with user namespaces, so ownership needs no handing back
func (r *registrant) remapsUsers(ctx context.Context, docker string) bool {
	r.mu.Lock()
	known, ok := r.remaps[docker]
	r.mu.Unlock()
	if ok {
		return known
	}
	options := ""
	if captured, err := terminal.Capture(ctx, []string{docker, "info", "--format", "{{.SecurityOptions}}"}, 10*time.Second, nil); err != nil {
		slog.Debug("docker info for ownership handback failed", "error", err)
	} else if captured.Code == 0 {
		options = strings.ToLower(captured.Stdout)
	}
	remaps := strings.TrimSpace(options) == "" || strings.Contains(options, "rootless") || strings.Contains(options, "userns")
	r.mu.Lock()
	r.remaps[docker] = remaps
	r.mu.Unlock()
	return remaps
}

// Uid and gid files go back to, false where nothing is handed back: off Linux, as root, or with a daemon that remaps users
func (r *registrant) handbackIDs(ctx context.Context, docker string) (int, int, bool) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 || r.remapsUsers(ctx, docker) {
		return 0, 0, false
	}
	return os.Geteuid(), os.Getegid(), true
}

// File of the handed back paths, beside the first sandbox folder
func recordPath(binds []bind) string {
	if len(binds) == 0 {
		return ""
	}
	return filepath.Join(filepath.Dir(binds[0].host), handbackRecord)
}

// What docker inspect says of a container
type inspected struct {
	Image  string
	Config struct {
		User string
	}
	HostConfig struct {
		Privileged bool
		CapAdd     []string
	}
	Mounts []struct {
		Type        string
		Source      string
		Destination string
	}
}

func inspect(ctx context.Context, docker, id string) *inspected {
	captured, err := terminal.Capture(ctx, []string{docker, "inspect", id}, 10*time.Second, nil)
	if err != nil || captured.Code != 0 {
		slog.Debug("docker inspect failed", "id", short(id))
		return nil
	}
	var list []inspected
	if err := json.Unmarshal([]byte(captured.Stdout), &list); err != nil || len(list) == 0 {
		return nil
	}
	return &list[0]
}

func (i *inspected) root() bool {
	user, _, _ := strings.Cut(i.Config.User, ":")
	return user == "" || user == "root" || user == "0"
}

// Whether the container may set file capabilities, which a change of owner clears
func (i *inspected) fileCaps() bool {
	if i.HostConfig.Privileged {
		return true
	}
	for _, capability := range i.HostConfig.CapAdd {
		switch strings.TrimPrefix(strings.ToUpper(capability), "CAP_") {
		case "SETFCAP", "ALL":
			return true
		}
	}
	return false
}

func (i *inspected) mountPoints() []string {
	var out []string
	for _, m := range i.Mounts {
		out = append(out, m.Destination)
	}
	return out
}

// Path through links, the fallback when it can't be resolved
func realOr(p, fallback string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return fallback
}

// Sandbox folders of icebath bound at /root or /workspace of the container: those under a docker folder of a profile's sandboxes
func sandboxBinds(cfg *config.Config, i *inspected) []bind {
	roots := map[string]bool{}
	add := func(p string) { roots[realOr(p, p)] = true }
	add(filepath.Join(cfg.Dir(), "sandboxes", "docker"))
	if found, err := filepath.Glob(filepath.Join(cfg.Dir(), "profiles", "*", "sandboxes", "docker")); err == nil {
		for _, p := range found {
			add(p)
		}
	}
	add(filepath.Join(cfg.ProfileDir(cfg.Profile()), "sandboxes", "docker"))
	if dir, err := terminal.SandboxDir(cfg); err == nil {
		add(filepath.Join(dir, "docker"))
	}
	var binds []bind
	for _, m := range i.Mounts {
		if m.Type != "bind" || (m.Destination != "/root" && m.Destination != "/workspace") {
			continue
		}
		source := m.Source
		if source == "" {
			source = "/nonexistent"
		}
		source = realOr(source, source)
		if st, err := os.Stat(source); err == nil && st.IsDir() && roots[filepath.Dir(filepath.Dir(source))] {
			binds = append(binds, bind{source, m.Destination})
		}
	}
	return binds
}

// Characters find patterns treat as special
var findSpecial = regexp.MustCompile(`([\[\]*?\\])`)

// find over the sandbox folders on their own filesystem, mount points nested in them pruned, followed by the tail
func walkArgv(binds []bind, mountPoints []string, tail []string) []string {
	var targets []string
	for _, b := range binds {
		targets = append(targets, b.sandbox)
	}
	var points []string
	for _, p := range mountPoints {
		if p = strings.TrimRight(p, "/"); !slices.Contains(points, p) {
			points = append(points, p)
		}
	}
	slices.Sort(points)
	var prune []string
	for _, point := range points {
		if !slices.ContainsFunc(targets, func(t string) bool { return strings.HasPrefix(point, strings.TrimRight(t, "/")+"/") }) {
			continue
		}
		if len(prune) > 0 {
			prune = append(prune, "-o")
		}
		prune = append(prune, "-path", findSpecial.ReplaceAllString(point, `\$1`))
	}
	argv := append([]string{"/usr/bin/find", "-P"}, targets...)
	argv = append(argv, "-xdev")
	if len(prune) > 0 {
		argv = append(argv, "(")
		argv = append(argv, prune...)
		argv = append(argv, ")", "-prune", "-o")
	}
	return append(argv, tail...)
}

// Runs docker with the target and argv until the deadline, the input on stdin; a throwaway container named by the target is removed
// when it runs out its time
func dockerIn(ctx context.Context, docker string, target, argv []string, deadline time.Time, input []byte) (*terminal.Captured, error) {
	ctx, cancel := context.WithTimeout(ctx, max(time.Until(deadline), time.Second))
	defer cancel()
	full := append(append([]string{docker}, target...), argv...)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		for i, word := range target {
			if word == "--name" && i+1 < len(target) {
				terminal.Capture(context.Background(), []string{docker, "rm", "-f", target[i+1]}, 10*time.Second, nil)
			}
		}
		return nil, terminal.ErrTimedOut
	}
	captured := &terminal.Captured{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return nil, err
		}
		captured.Code = exit.ExitCode()
	}
	return captured, nil
}

// Paths under the sandbox folders owned by the uid
func ownedBy(ctx context.Context, docker string, target []string, binds []bind, i *inspected, owner int, deadline time.Time) ([]string, bool) {
	tail := []string{"(", "-type", "f", "-o", "-type", "d", "-o", "-type", "l", ")", "-user", strconv.Itoa(owner), "-print0"}
	captured, err := dockerIn(ctx, docker, target, walkArgv(binds, i.mountPoints(), tail), deadline, nil)
	if err != nil || captured.Code != 0 {
		if captured != nil {
			slog.Debug("ownership walk exited nonzero", "code", captured.Code, "stderr", strs.Head(strings.TrimSpace(captured.Stderr), 500))
		}
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(captured.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

// Gives the paths to the owner, links themselves rather than what they point at
func chown(ctx context.Context, docker string, target, paths []string, owner string, deadline time.Time) bool {
	var input bytes.Buffer
	for _, p := range paths {
		input.WriteString(p)
		input.WriteByte(0)
	}
	captured, err := dockerIn(ctx, docker, target, []string{"/usr/bin/xargs", "-0", "-r", "/bin/chown", "-h", owner, "--"}, deadline, input.Bytes())
	if err != nil || captured.Code != 0 {
		slog.Debug("ownership chown failed", "owner", owner)
		return false
	}
	return true
}

// A root throwaway container of the container's image, with no network and only the capabilities changing ownership takes
func throwaway(id string, i *inspected, volumes []string) []string {
	target := []string{"run", "--rm", "-i", "--pull", "never", "--network", "none", "--user", "0:0"}
	target = append(target, throwawayConfinement...)
	target = append(target, safeEnv...)
	target = append(target, "--name", fmt.Sprintf("icebath-handback-%s-%s", short(id), strs.Random(4)))
	target = append(target, volumes...)
	return append(target, "--entrypoint", "", i.Image)
}

// Script giving each file back to its own owner, which clears setuid and setgid bits and file capabilities a container could set
const sameOwnerChown = `uids=$("$@" -type f -exec stat -c %u -- {} +) || exit; for u in $(printf "%s\n" "$uids" | sort -u); do "$@" -type f -user "$u" -exec chown -h "$u" {} + || exit; done`

// Strips setuid and setgid bits and file capabilities from what a root container left in its sandbox folders, so nothing there runs with
// raised rights on the host
func (r *registrant) stripPrivileges(ctx context.Context, cfg *config.Config, docker, id string, i *inspected) {
	if i == nil {
		if _, _, ok := r.handbackIDs(ctx, docker); ok {
			i = inspect(ctx, docker, id)
		}
	}
	if i == nil || !i.root() {
		return
	}
	binds := sandboxBinds(cfg, i)
	if len(binds) == 0 {
		return
	}
	var volumes []string
	for _, b := range binds {
		volumes = append(volumes, "--mount", "type=bind,src="+b.host+",dst="+b.sandbox)
	}
	target := throwaway(id, i, volumes)
	walk := append([]string{"find"}, walkArgv(binds, i.mountPoints(), nil)[1:]...)
	var argv []string
	if i.fileCaps() {
		argv = append([]string{"/bin/sh", "-c", sameOwnerChown, "sh"}, walk...)
	} else {
		argv = append(walk, "-type", "f", "-perm", "/6000", "-exec", "chmod", "ug-s", "{}", "+")
	}
	failure := ""
	captured, err := dockerIn(ctx, docker, target, argv, time.Now().Add(chownTimeout), nil)
	switch {
	case err != nil:
		failure = err.Error()
	case captured.Code != 0:
		failure = fmt.Sprintf("exit %d: %s", captured.Code, strs.Head(captured.Stderr, 300))
	}
	if failure == "" {
		return
	}
	where := filepath.Dir(binds[0].host)
	r.mu.Lock()
	warned := r.warned[where]
	r.warned[where] = true
	r.mu.Unlock()
	text := "Could not strip setuid/setgid bits and file capabilities from Docker sandbox; files there may still run with elevated rights on the host"
	if warned {
		slog.Debug(text, "sandbox", where, "error", strings.TrimSpace(failure))
	} else {
		slog.Warn(text, "sandbox", where, "error", strings.TrimSpace(failure))
	}
}

// Gives what a stopped root container left in its sandbox folders to the host user, recording which paths, so the user can read and
// remove them
func (r *registrant) handBack(ctx context.Context, cfg *config.Config, docker, id string, binds []bind) {
	record := recordPath(binds)
	if record == "" {
		return
	}
	uid, gid, ok := r.handbackIDs(ctx, docker)
	if !ok {
		return
	}
	i := inspect(ctx, docker, id)
	if i == nil || !i.root() {
		return
	}
	target := throwaway(id, i, []string{"--volumes-from", id})
	deadline := time.Now().Add(chownTimeout)
	if paths, walked := ownedBy(ctx, docker, target, binds, i, 0, deadline); walked && len(paths) > 0 {
		known := map[string]bool{}
		if data, err := os.ReadFile(record); err == nil {
			for _, p := range strings.Split(string(data), "\x00") {
				known[p] = true
			}
		}
		if f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			for _, p := range paths {
				if !known[p] {
					f.WriteString(p + "\x00")
				}
			}
			f.Close()
		}
		chown(ctx, docker, target, paths, fmt.Sprintf("%d:%d", uid, gid), deadline)
	}
	r.stripPrivileges(ctx, cfg, docker, id, i)
}

// Gives the paths handed to the host user back to root inside the root container starting on them again, then drops the record
func reclaim(ctx context.Context, docker, id string, binds []bind) {
	record := recordPath(binds)
	if record == "" || !fsx.IsFile(record) {
		return
	}
	var i *inspected
	if runtime.GOOS == "linux" {
		if i = inspect(ctx, docker, id); i == nil {
			return
		}
	}
	if i == nil || !i.root() {
		os.Remove(record)
		return
	}
	target := append([]string{"exec", "-i", "-u", "0:0"}, safeEnv...)
	target = append(target, id)
	deadline := time.Now().Add(chownTimeout)
	data, err := os.ReadFile(record)
	if err != nil {
		return
	}
	recorded := map[string]bool{}
	for _, p := range strings.Split(string(data), "\x00") {
		recorded[p] = true
	}
	owned, walked := ownedBy(ctx, docker, target, binds, i, os.Geteuid(), deadline)
	if !walked {
		return
	}
	var paths []string
	for _, p := range owned {
		if recorded[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 && !chown(ctx, docker, target, paths, "0:0", deadline) {
		return
	}
	os.Remove(record)
}
