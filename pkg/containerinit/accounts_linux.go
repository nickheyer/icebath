package containerinit

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Account databases the lookups read, vars so tests can point them at fixtures
var (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// Passwd entry of one account with its supplementary groups
type account struct {
	name     string
	uid, gid int
	home     string
	groups   []int
}

// Colon separated rows of an account database
func readDB(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, strings.Split(line, ":"))
	}
	return rows, sc.Err()
}

// Account by name or numeric uid, with the gids of every group listing it as a member
func lookupAccount(spec string) (account, error) {
	rows, err := readDB(passwdPath)
	if err != nil {
		return account{}, err
	}
	for _, r := range rows {
		if len(r) < 6 || (r[0] != spec && r[2] != spec) {
			continue
		}
		uid, err1 := strconv.Atoi(r[2])
		gid, err2 := strconv.Atoi(r[3])
		if err1 != nil || err2 != nil {
			return account{}, fmt.Errorf("invalid passwd entry for %s", r[0])
		}
		a := account{name: r[0], uid: uid, gid: gid, home: r[5]}
		groups, err := readDB(groupPath)
		if err != nil {
			return account{}, err
		}
		seen := map[int]bool{gid: true}
		a.groups = []int{gid}
		for _, g := range groups {
			if len(g) < 4 {
				continue
			}
			n, err := strconv.Atoi(g[2])
			if err != nil || seen[n] {
				continue
			}
			for _, m := range strings.Split(g[3], ",") {
				if m == a.name {
					seen[n] = true
					a.groups = append(a.groups, n)
					break
				}
			}
		}
		return a, nil
	}
	return account{}, fmt.Errorf("unknown account %s", spec)
}

// Name of the group holding gid, false when none does
func groupName(gid int) (string, bool) {
	rows, err := readDB(groupPath)
	if err != nil {
		return "", false
	}
	for _, g := range rows {
		if len(g) >= 3 && g[2] == strconv.Itoa(gid) {
			return g[0], true
		}
	}
	return "", false
}

// Process credential that runs a child as the account
func (a account) credential() *syscall.SysProcAttr {
	groups := make([]uint32, 0, len(a.groups))
	for _, g := range a.groups {
		groups = append(groups, uint32(g))
	}
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid), Groups: groups}}
}

// Switches the calling thread to the account, the credentials an immediately following exec hands the new image
func (a account) apply() error {
	runtime.LockOSThread()
	if err := unix.Setgroups(a.groups); err != nil {
		return err
	}
	if _, _, e := unix.RawSyscall(sysSetgid, uintptr(a.gid), 0, 0); e != 0 {
		return e
	}
	if _, _, e := unix.RawSyscall(sysSetuid, uintptr(a.uid), 0, 0); e != 0 {
		return e
	}
	return nil
}

// Runs a shadow utility of the image with its output on the init's streams
func shadowTool(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// Runs a shadow utility with every stream discarded, the 2>/dev/null of the image scripts
func shadowToolQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
