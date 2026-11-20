//go:build windows

package provider

// Shell line that prints the token through echo under ComSpec
func printTokenCmd(token string) string { return "echo " + token }
