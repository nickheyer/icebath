//go:build !unix

package app

// Terminal background query needs termios, absent here
func queryOSC11Background() (string, bool) { return "", false }

// Cooked mode healing needs termios, absent here
func healCookedModeDrift(int) bool { return false }
