//go:build !unix

package userutil

import (
	"errors"
	"fmt"
	"os/user"
	"runtime"
	"strings"
)

// Account and group databases with numeric ids exist only on Unix
var errNoDatabase = fmt.Errorf("account database lookups on %s: %w", runtime.GOOS, errors.ErrUnsupported)

// Account named name, unsupported off Unix
func Lookup(name string) (Account, error) { return Account{}, errNoDatabase }

// Account with user id uid, unsupported off Unix
func LookupID(uid int) (Account, error) { return Account{}, errNoDatabase }

// Group with group id gid, unsupported off Unix
func LookupGroupID(gid int) (Group, error) { return Group{}, errNoDatabase }

// Every account, unsupported off Unix
func All() ([]Account, error) { return nil, errNoDatabase }

// Name of the account the process token belongs to, without its domain
func processLoginName() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("no login name in the environment: %w", err)
	}
	name := u.Username
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}
