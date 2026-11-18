//go:build !unix

package app

// Terminal background query needs termios, absent here
func queryOSC11Background() (string, bool, error) { return "", false, nil }
