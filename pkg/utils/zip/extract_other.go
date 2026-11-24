//go:build !windows && !darwin

package ziputil

// Name element as it is, every element but . and .. being a legal name here
func legalElement(el string) string { return el }
