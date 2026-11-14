package toolchain

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/dl"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/shell"
)

// Shared library official Linux builds may need from the host and the bound on one package manager run
const (
	libatomicName      = "libatomic.so.1"
	hostInstallTimeout = 300 * time.Second
	sudoProbeTimeout   = 10 * time.Second
)

// Outcome of one host package install attempt and the command a user can run instead
type hostRepair struct {
	ok     bool
	remedy string
}

// Package manager commands that install the libatomic runtime, by manager
var libatomicInstallers = map[string][]string{
	"apt-get": {"apt-get", "install", "-y", "libatomic1"},
	"dnf":     {"dnf", "install", "-y", "libatomic"},
	"yum":     {"yum", "install", "-y", "libatomic"},
	"zypper":  {"zypper", "--non-interactive", "install", "libatomic1"},
	"pacman":  {"pacman", "-S", "--needed", "--noconfirm", "gcc-libs"},
	"apk":     {"apk", "add", "libatomic"},
}

// Distribution families by the identifiers their os-release files carry
var (
	debianFamily = map[string]bool{"debian": true, "ubuntu": true, "linuxmint": true, "pop": true, "raspbian": true, "kali": true, "elementary": true}
	rpmFamily    = map[string]bool{"rhel": true, "fedora": true, "centos": true, "rocky": true, "almalinux": true, "ol": true, "oracle": true, "amzn": true}
	archFamily   = map[string]bool{"arch": true, "manjaro": true, "endeavouros": true, "garuda": true, "artix": true}
)

// Package managers to try on this distribution, the native one alone when it is recognised
func (s *Store) managerOrder() []string {
	var tokens []string
	if dist, err := s.host.Distribution(); err == nil {
		for _, word := range append([]string{dist.ID}, dist.IDLike...) {
			tokens = append(tokens, strings.Fields(strings.ReplaceAll(strings.ToLower(word), ",", " "))...)
		}
	}
	some := func(test func(string) bool) bool {
		for _, t := range tokens {
			if test(t) {
				return true
			}
		}
		return false
	}
	switch {
	case some(func(t string) bool { return debianFamily[t] }):
		return []string{"apt-get"}
	case some(func(t string) bool { return rpmFamily[t] }):
		return []string{"dnf", "yum"}
	case some(func(t string) bool { return strings.Contains(t, "suse") || strings.HasPrefix(t, "sles") }):
		return []string{"zypper"}
	case some(func(t string) bool { return archFamily[t] }):
		return []string{"pacman"}
	case some(func(t string) bool { return t == "alpine" }):
		return []string{"apk"}
	}
	return []string{"dnf", "yum", "apt-get", "zypper", "pacman", "apk"}
}

// Runs one host command without a terminal and reports whether it exited zero in time
func runHostCommand(ctx context.Context, argv []string) bool {
	bounded, cancel := context.WithTimeout(ctx, hostInstallTimeout)
	defer cancel()
	res, err := proc.Run(bounded, exec.Command(argv[0], argv[1:]...))
	return err == nil && res.Exit.Success()
}

type HostPackage struct {
	Library string
	Command []string
	Message string
}

func (s *Store) libatomicCommand() []string {
	for _, manager := range s.managerOrder() {
		if _, err := exec.LookPath(manager); err == nil {
			return libatomicInstallers[manager]
		}
	}
	return nil
}

func runAttached(ctx context.Context, argv []string) bool {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	exit, err := proc.RunAttached(ctx, cmd)
	return err == nil && exit.Success()
}

func (s *Store) installBeforeLock(ctx context.Context, confirm func(context.Context, HostPackage) bool) {
	if confirm == nil || hostinfo.OSFamily != hostinfo.FamilyLinux || os.Geteuid() == 0 {
		return
	}
	host, err := s.hostTarget(ctx)
	if err != nil || host.target.musl() || host.target.bionic() {
		return
	}
	if _, err := dl.Open(libatomicName, dl.Local); err == nil {
		return
	}
	command := s.libatomicCommand()
	if command == nil {
		return
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return
	}
	probe, cancel := context.WithTimeout(ctx, sudoProbeTimeout)
	res, err := proc.Run(probe, exec.Command(sudo, "-n", "true"))
	cancel()
	if err == nil && res.Exit.Success() {
		return
	}
	request := HostPackage{
		Library: libatomicName,
		Command: append([]string{"sudo"}, command...),
		Message: "a pinned program needs " + libatomicName + "; sudo may ask for your password to install it",
	}
	if !confirm(ctx, request) {
		return
	}
	install := append([]string{sudo}, command...)
	if runAttached(ctx, install) || command[0] != "apt-get" {
		return
	}
	if runAttached(ctx, []string{sudo, "apt-get", "update", "-qq"}) {
		runAttached(ctx, install)
	}
}

// Installs the libatomic package without prompting, refreshing empty apt package lists once
func (s *Store) installLibatomic(ctx context.Context) hostRepair {
	command := s.libatomicCommand()
	if command == nil {
		return hostRepair{remedy: "the pinned program needs " + libatomicName + "; install the distro package that provides it and rerun"}
	}
	var prefix []string
	manual := shell.Join(command)
	if os.Geteuid() != 0 {
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			return hostRepair{remedy: "install the missing runtime library with as root: " + manual + " and rerun"}
		}
		prefix = []string{sudo, "-n"}
		manual = shell.Join(append([]string{"sudo"}, command...))
	}
	remedy := "install the missing runtime library with " + manual + " and rerun"
	argv := append(append([]string(nil), prefix...), command...)
	slog.Info(fmt.Sprintf("a pinned program needs %s; trying non-interactive %s", libatomicName, shell.Join(argv)))
	if runHostCommand(ctx, argv) {
		return hostRepair{ok: true, remedy: remedy}
	}
	if command[0] != "apt-get" {
		return hostRepair{remedy: remedy}
	}
	update := append(append([]string(nil), prefix...), "apt-get", "update", "-qq")
	return hostRepair{ok: runHostCommand(ctx, update) && runHostCommand(ctx, argv), remedy: remedy}
}

// Tries the host package a failed smoke run names, once per store, and verifies the staged tree again
func (s *Store) repairStaged(ctx context.Context, r *recipe, tree string, host hostTarget, reason string) (string, string) {
	if hostinfo.OSFamily != hostinfo.FamilyLinux || !strings.Contains(reason, libatomicName) {
		return reason, ""
	}
	s.mu.Lock()
	fixed, tried := s.hostFixed[libatomicName]
	if !tried {
		fixed = s.installLibatomic(ctx)
		s.hostFixed[libatomicName] = fixed
	}
	s.mu.Unlock()
	if !fixed.ok {
		return reason, fixed.remedy
	}
	retried := s.verifyEntry(ctx, r, tree, host.target, host.runs())
	if strings.Contains(retried, libatomicName) {
		return retried, "the libatomic package installed, but the program still cannot load " + libatomicName + "; check the loader path"
	}
	return retried, ""
}
