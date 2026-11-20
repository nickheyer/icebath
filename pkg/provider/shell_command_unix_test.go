//go:build !windows

package provider

// Shell line that prints the token through printf under /bin/sh
func printTokenCmd(token string) string { return "printf " + token }
