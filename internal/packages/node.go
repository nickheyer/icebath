package packages

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// The one libatomic install this process tries, and what to tell the person when it could not
type attempt struct {
	tried  bool
	remedy string
}

var libatomicInstallers = map[string][]string{
	"apt-get": {"apt-get", "install", "-y", "libatomic1"},
	"dnf":     {"dnf", "install", "-y", "libatomic"},
	"yum":     {"yum", "install", "-y", "libatomic"},
	"zypper":  {"zypper", "--non-interactive", "install", "libatomic1"},
	"pacman":  {"pacman", "-S", "--needed", "--noconfirm", "gcc-libs"},
	"apk":     {"apk", "add", "libatomic"},
}

// Official Linux Node needs libatomic.so.1: installs the distribution's package once without asking for a password, then checks again
func repairLibatomic(ctx context.Context, h *Packages, r Reporter, entry, target, reason string) (string, string) {
	if target != CurrentTarget() || !strings.HasPrefix(target, "linux") || !strings.Contains(reason, "libatomic.so.1") {
		return reason, ""
	}
	h.mu.Lock()
	if h.libatomic == nil {
		tried, remedy := installLibatomic(ctx, reporter(r))
		h.libatomic = &attempt{tried: tried, remedy: remedy}
	}
	outcome := *h.libatomic
	h.mu.Unlock()
	if !outcome.tried {
		return reason, outcome.remedy
	}
	p, err := h.Package(Node)
	if err != nil {
		return reason, outcome.remedy
	}
	retried := h.verify(p, entry, target)
	if strings.Contains(retried, "libatomic.so.1") {
		return retried, "The libatomic package is installed, but Node still cannot load libatomic.so.1. Check where the loader looks for libraries."
	}
	return retried, ""
}

// Runs the distribution's libatomic install through sudo -n, or as root, returning whether it ran and the command for the person to run
func installLibatomic(ctx context.Context, r Reporter) (bool, string) {
	var install []string
	for _, manager := range packageManagers() {
		if Which(manager, os.Getenv("PATH")) != "" {
			install = libatomicInstallers[manager]
			break
		}
	}
	if install == nil {
		return false, "Node needs libatomic.so.1. Install the package of your Linux distribution that provides it, then run the command again."
	}
	argv, shown, remedy := slices.Clone(install), shellJoin(install), shellJoin(install)
	if os.Geteuid() != 0 {
		sudo := Which("sudo", os.Getenv("PATH"))
		if sudo == "" {
			return false, fmt.Sprintf("Install the missing library as root with `%s`, then run the command again.", shellJoin(install))
		}
		argv = append([]string{sudo, "-n"}, install...)
		shown, remedy = shellJoin(append([]string{"sudo", "-n"}, install...)), shellJoin(append([]string{"sudo"}, install...))
	}
	remedy = fmt.Sprintf("Install the missing library with `%s`, then run the command again.", remedy)
	r.Line(fmt.Sprintf("Node needs libatomic.so.1, so icebath runs `%s` without asking for a password…", shown))
	attempt := func(args []string) bool {
		out, err := run(ctx, command{argv: args, timeout: 300 * time.Second})
		return err == nil && out.code == 0
	}
	if attempt(argv) {
		return true, remedy
	}
	if install[0] != "apt-get" {
		return false, remedy
	}
	prefix := argv[:len(argv)-len(install)]
	return attempt(append(slices.Clone(prefix), "apt-get", "update", "-qq")) && attempt(argv), remedy
}

// Package managers to try for this distribution, all of them when it is not known
func packageManagers() []string {
	tokens := map[string]bool{}
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if ok && (key == "ID" || key == "ID_LIKE") {
				for _, t := range strings.Fields(strings.ReplaceAll(strings.ToLower(strings.Trim(value, `"'`)), ",", " ")) {
					tokens[t] = true
				}
			}
		}
		break
	}
	has := func(names ...string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return tokens[n] })
	}
	switch {
	case has("debian", "ubuntu", "linuxmint", "pop", "raspbian", "kali", "elementary"):
		return []string{"apt-get"}
	case has("rhel", "fedora", "centos", "rocky", "almalinux", "ol", "oracle", "amzn"):
		return []string{"dnf", "yum"}
	}
	for t := range tokens {
		if strings.Contains(t, "suse") || strings.HasPrefix(t, "sles") {
			return []string{"zypper"}
		}
	}
	switch {
	case has("arch", "manjaro", "endeavouros", "garuda", "artix"):
		return []string{"pacman"}
	case tokens["alpine"]:
		return []string{"apk"}
	}
	return []string{"dnf", "yum", "apt-get", "zypper", "pacman", "apk"}
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// Command line quoted for a POSIX shell
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a == "":
			parts[i] = "''"
		case shellSafe.MatchString(a):
			parts[i] = a
		default:
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

// Installs the Node dependencies of a folder holding a package.json with the package manager's npm, recorded so pm update --npm refreshes
// them; empty when it worked, else what went wrong
func (h *Packages) NodeModules(ctx context.Context, cfg *config.Config, dir string, explicit bool) string {
	if !fsx.IsFile(filepath.Join(dir, "package.json")) {
		return ""
	}
	if !explicit && !h.LazyAllowed(cfg) {
		return "Downloads when needed are off, so the Node packages of this folder were not installed. Turn on Download when needed in /settings under Downloads."
	}
	verb := "install"
	if fsx.IsFile(filepath.Join(dir, "package-lock.json")) {
		verb = "ci"
	}
	runner, err := h.Ensure(ctx, cfg, NPM, Ensuring{Explicit: explicit})
	if err != nil {
		return fmt.Sprintf("`npm %s` could not run: %s", verb, err)
	}
	npm := Which("npm", runner.Env[pathKey(runner.Env)])
	if npm == "" {
		return "npm is missing from the programs the package manager installed. Run `icebath pm install npm`."
	}
	if message := npmRun(ctx, runner, npm, dir, verb, "--no-audit", "--no-fund"); message != "" {
		return message
	}
	if err := h.RecordTree(dir); err != nil {
		return fmt.Sprintf("`npm %s` finished, but its folder could not be recorded: %s", verb, err)
	}
	return ""
}

// Runs npm in a folder for at most fifteen minutes, empty when it worked, else its exit and the tail of what it said
func npmRun(ctx context.Context, runner *Runner, npm, dir, verb string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 900*time.Second)
	defer cancel()
	cmd := runner.Command(ctx, npm, append([]string{verb}, args...)...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("`%s` did not finish within 15 minutes.", strings.Join(cmd.Args, " "))
	}
	exit, ok := err.(*exec.ExitError)
	if err != nil && !ok {
		return fmt.Sprintf("`npm %s` could not run: %s", verb, err)
	}
	if err == nil {
		return ""
	}
	tail := strings.TrimSpace(strings.ToValidUTF8(cmp.Or(stderr.String(), stdout.String()), "�"))
	return fmt.Sprintf("`npm %s` ended with exit code %d: %s", verb, exit.ExitCode(), strs.Tail(tail, 300))
}

// Folders whose Node dependencies the package manager installed
func (h *Packages) trees() []string {
	data, err := os.ReadFile(filepath.Join(h.stateDir(), "node-trees.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Trees []string `json:"trees"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	return doc.Trees
}

// Records a folder holding a package.json whose Node dependencies a feature installed with the package manager's npm, so pm update --npm
// refreshes them
func (h *Packages) RecordTree(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	trees := map[string]bool{abs: true}
	for _, tree := range h.trees() {
		if fsx.IsFile(filepath.Join(tree, "package.json")) {
			trees[tree] = true
		}
	}
	return writeJSON(filepath.Join(h.stateDir(), "node-trees.json"), map[string]any{"trees": slices.Sorted(maps.Keys(trees))})
}
