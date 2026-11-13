//go:build !windows

package vault

// Line ending a secret written to a manager CLI ends with, the platform text mode one
const stdinNewline = "\n"
