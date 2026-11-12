//go:build linux || darwin

package main

import (
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func osRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

func darwinMajor() int {
	if electronPlatform() != "darwin" {
		return 0
	}
	n, _ := strconv.Atoi(strings.SplitN(osRelease(), ".", 2)[0])
	return n
}

func runQuiet(name string, args ...string) error { return exec.Command(name, args...).Run() }
