package pypwd

import (
	"errors"
	"os"
)

// Login name from LOGNAME, USER, LNAME or USERNAME, else the password database entry of the real uid
func Getuser() (string, error) {
	for _, name := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if user := os.Getenv(name); user != "" {
			return user, nil
		}
	}
	pw, err := Getpwuid(os.Getuid())
	if err != nil {
		return "", errors.New("No username set in the environment")
	}
	return pw.Name, nil
}
