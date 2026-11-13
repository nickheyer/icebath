//go:build !linux

package state

// Only Linux exposes a mount table with filesystem types, so no other host is ever flagged cross-VM
func mountFSType(string) string { return "" }
