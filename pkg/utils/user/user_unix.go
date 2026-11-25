//go:build unix

package userutil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/utils/homeguard"
)

// Local account and group databases, read before the system directory is asked
const (
	passwdFile = "/etc/passwd"
	groupFile  = "/etc/group"
)

// Longest wait for the system directory tool
const directoryTimeout = 10 * time.Second

// Directory lookup that cannot run because the tool asking the system directory is missing
var errNoDirectory = errors.New("no system directory tool")

// Account named name: the local passwd file first, then the system directory (NSS on Linux and the BSDs, Directory Services on macOS), then the C library lookup
func Lookup(name string) (Account, error) {
	return findAccount("name", name, func(a Account) bool { return a.Name == name },
		func() (*user.User, error) { return user.Lookup(name) }, "user "+strconv.Quote(name))
}

// First account with user id uid, searched like Lookup
func LookupID(uid int) (Account, error) {
	key := strconv.Itoa(uid)
	return findAccount("uid", key, func(a Account) bool { return a.UID == uid },
		func() (*user.User, error) { return user.LookupId(key) }, "user id "+key)
}

// First group with group id gid and its listed members: the local group file first, then the system directory, then the C library lookup without members
func LookupGroupID(gid int) (Group, error) {
	key := strconv.Itoa(gid)
	answered, firstErr := false, error(nil)
	var found Group
	hit := false
	err := scanFile(groupFile, 4, func(fields []string) bool {
		if g, ok := parseGroup(fields); ok && g.GID == gid {
			found, hit = g, true
			return false
		}
		return true
	})
	if hit {
		return found, nil
	}
	answered, firstErr = noteSource(err, answered, firstErr)
	groups, err := directoryGroups(key)
	for _, g := range groups {
		if g.GID == gid {
			return g, nil
		}
	}
	answered, firstErr = noteSource(err, answered, firstErr)
	g, err := user.LookupGroupId(key)
	if err == nil {
		if n, convErr := strconv.Atoi(g.Gid); convErr == nil && n == gid {
			return Group{Name: g.Name, GID: n}, nil
		}
	}
	var unknown user.UnknownGroupIdError
	if errors.As(err, &unknown) {
		answered = true
	} else {
		answered, firstErr = noteSource(err, answered, firstErr)
	}
	return Group{}, missing("group id "+key, answered, firstErr)
}

// Every account of the local passwd file in file order, then the accounts only the system directory lists
func All() ([]Account, error) {
	var out []Account
	seen := map[string]bool{}
	fileErr := scanFile(passwdFile, 7, func(fields []string) bool {
		if a, ok := parseAccount(fields); ok {
			seen[a.Name] = true
			out = append(out, guarded(a))
		}
		return true
	})
	listed, dirErr := directoryAccounts("", "")
	if fileErr != nil && dirErr != nil {
		return nil, fileErr
	}
	for _, a := range listed {
		if !seen[a.Name] {
			seen[a.Name] = true
			out = append(out, guarded(a))
		}
	}
	return out, nil
}

// Name of the account of the real user id
func processLoginName() (string, error) {
	uid := os.Getuid()
	a, err := LookupID(uid)
	if err != nil {
		return "", fmt.Errorf("no login name in the environment and no account for user id %d: %w", uid, err)
	}
	return a.Name, nil
}

// Account a key names, from the passwd file, then the directory under attr, then os/user
func findAccount(attr, key string, match func(Account) bool, lookup func() (*user.User, error), what string) (Account, error) {
	answered, firstErr := false, error(nil)
	var found Account
	hit := false
	err := scanFile(passwdFile, 7, func(fields []string) bool {
		if a, ok := parseAccount(fields); ok && match(a) {
			found, hit = a, true
			return false
		}
		return true
	})
	if hit {
		return guarded(found), nil
	}
	answered, firstErr = noteSource(err, answered, firstErr)
	if key == "" {
		return Account{}, missing(what, true, nil)
	}
	listed, err := directoryAccounts(attr, key)
	for _, a := range listed {
		if match(a) {
			return guarded(a), nil
		}
	}
	answered, firstErr = noteSource(err, answered, firstErr)
	u, err := lookup()
	if err == nil {
		if a, ok := fromUser(u); ok && match(a) {
			return guarded(a), nil
		}
		answered = true
	} else {
		var unknownName user.UnknownUserError
		var unknownID user.UnknownUserIdError
		if errors.As(err, &unknownName) || errors.As(err, &unknownID) {
			answered = true
		} else {
			answered, firstErr = noteSource(err, answered, firstErr)
		}
	}
	return Account{}, missing(what, answered, firstErr)
}

// Whether a source answered and the first real failure so far, after one more source ran with err
func noteSource(err error, answered bool, firstErr error) (bool, error) {
	switch {
	case err == nil:
		return true, firstErr
	case errors.Is(err, errNoDirectory):
		return answered, firstErr
	case firstErr == nil:
		return answered, err
	}
	return answered, firstErr
}

// Not-found error for what, or the failure that kept every source from answering
func missing(what string, answered bool, firstErr error) error {
	if !answered && firstErr != nil {
		return fmt.Errorf("%s: %w", what, firstErr)
	}
	return fmt.Errorf("%s: %w", what, ErrNotFound)
}

// Account with its home swapped for the private home when it is this process inside a test binary
func guarded(a Account) Account {
	a.Home = homeguard.AccountDir(a.UID, a.Home)
	return a
}

// Account of an os/user record, which carries no login shell
func fromUser(u *user.User) (Account, bool) {
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return Account{}, false
	}
	return Account{Name: u.Username, UID: uid, GID: gid, Gecos: u.Name, Home: u.HomeDir}, true
}

// Calls yield with the fields of each record of a colon separated database file holding exactly width fields, until yield returns false
func scanFile(path string, width int, yield func([]string) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if fields, ok := splitRecord(sc.Text(), width); ok && !yield(fields) {
			return nil
		}
	}
	return sc.Err()
}

// Fields of one database line, false for blank lines, comments, NIS +/- entries and lines of another width
func splitRecord(line string, width int) ([]string, bool) {
	line = strings.TrimLeft(line, " \t")
	if line == "" || line[0] == '#' || line[0] == '+' || line[0] == '-' {
		return nil, false
	}
	fields := strings.Split(strings.TrimRight(line, "\r"), ":")
	return fields, len(fields) == width
}

// Account of the seven passwd fields name:password:uid:gid:gecos:home:shell
func parseAccount(fields []string) (Account, bool) {
	uid, err1 := strconv.Atoi(fields[2])
	gid, err2 := strconv.Atoi(fields[3])
	if err1 != nil || err2 != nil {
		return Account{}, false
	}
	return Account{Name: fields[0], UID: uid, GID: gid, Gecos: fields[4], Home: fields[5], Shell: fields[6]}, true
}

// Group of the four group fields name:password:gid:members
func parseGroup(fields []string) (Group, bool) {
	gid, err := strconv.Atoi(fields[2])
	if err != nil {
		return Group{}, false
	}
	var members []string
	for member := range strings.SplitSeq(fields[3], ",") {
		if member = strings.TrimSpace(member); member != "" {
			members = append(members, member)
		}
	}
	return Group{Name: fields[0], GID: gid, Members: members}, true
}

// Longest wait for the directory tool's output once it was killed or exited, in case a process it started still holds the pipe
const directoryWaitDelay = time.Second

// Output of a directory tool run with args, killed at the directory timeout
func runDirectoryTool(tool string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), directoryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.WaitDelay = directoryWaitDelay
	return cmd.Output()
}

// Path of a system tool from PATH or the first of its usual locations that exists, empty when none does
func findTool(name string, usual ...string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, path := range usual {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			return path
		}
	}
	return ""
}
