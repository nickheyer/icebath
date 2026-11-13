package hostinfo

import (
	"context"
	"strings"
)

// Family of the operating system this binary was built for
const OSFamily = FamilyLinux

// Identity of the running Linux kernel
func probeOS() (OS, error) { return unameOS() }

// Linux runs this binary natively on its own architecture
func nativeArch() Arch { return processArch() }

// Kernel name, release, machine, processor and C library joined the way the dashboard shows them
func platformLabel(ctx context.Context, h *Host) (string, error) {
	o, err := h.OS()
	if err != nil {
		return "", err
	}
	processor, err := unameProcessor(ctx)
	if err != nil {
		return "", err
	}
	if processor == o.Machine {
		processor = ""
	}
	libc, err := h.LibC(ctx)
	if err != nil {
		return "", err
	}
	return joinPlatform(o.System, o.Release, o.Machine, processor, "with", string(libc.Flavor)+libc.Version), nil
}

// Processor name uname prints for its p flag, empty when uname is missing, fails or names none
func unameProcessor(ctx context.Context) (string, error) {
	out, _, err := runProgram(ctx, "uname", []string{"/usr/bin/uname", "/bin/uname"}, "-p")
	if err != nil {
		return "", ctx.Err()
	}
	name := strings.TrimSpace(string(out))
	if name == "unknown" {
		return "", nil
	}
	return name, nil
}
