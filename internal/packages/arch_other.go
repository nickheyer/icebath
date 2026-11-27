//go:build !darwin && !windows

package packages

// Linux and the rest run no x64 emulation this process could be under
func emulatedOnArm64() bool { return false }
