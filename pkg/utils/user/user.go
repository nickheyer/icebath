package userutil

import (
	"errors"
	"os"
)

// Account record of the user database
type Account struct {
	Name  string
	UID   int
	GID   int
	Gecos string
	Home  string
	Shell string
}

// Group record of the group database
type Group struct {
	Name    string
	GID     int
	Members []string
}

// Lookup miss, matched with errors.Is on every not-found error of this package
var ErrNotFound = errors.New("not found in the account database")

// Login name of this user: LOGNAME, USER, LNAME or USERNAME from the environment, else the name of the account the process runs as
func LoginName() (string, error) {
	for _, key := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if name := os.Getenv(key); name != "" {
			return name, nil
		}
	}
	return processLoginName()
}
