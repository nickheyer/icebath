package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Target of a procfs link with trailing garbage after a NUL cut and a false deleted suffix dropped
func readLink(pid int, name string) (string, error) {
	target, err := os.Readlink(procPath(pid, name))
	if err != nil {
		return "", procError(pid, err)
	}
	target, _, _ = strings.Cut(target, "\x00")
	if trimmed, ok := strings.CutSuffix(target, " (deleted)"); ok {
		if _, err := os.Stat(target); err != nil {
			target = trimmed
		}
	}
	return target, nil
}

// Whether pid is an exited child its parent has not collected yet
func isZombie(pid int) bool {
	st, err := StatusOf(pid)
	return err == nil && st == Zombie
}

// Kernel name of pid between the first open and last close parenthesis of its stat line
func comm(pid int) (string, error) {
	data, err := readProc(pid, "stat")
	if err != nil {
		return "", err
	}
	start, end := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')')
	if start < 0 || end < start {
		return "", fmt.Errorf("pid %d: malformed stat line", pid)
	}
	return string(data[start+1 : end]), nil
}

// Process name, the fifteen byte kernel name widened to the argv zero basename it truncates
func Name(pid int) (string, error) {
	short, err := comm(pid)
	if err != nil {
		return "", err
	}
	return widenName(pid, short)
}

// Executable path of pid, empty for a kernel thread that has none
func Exe(pid int) (string, error) {
	target, err := readLink(pid, "exe")
	if err == nil {
		return target, nil
	}
	if errors.Is(err, ErrNotFound) && !isZombie(pid) {
		if _, lerr := os.Lstat(procPath(pid, "")); lerr == nil {
			return "", nil
		}
	}
	return "", err
}

// Argument vector of pid, split on spaces when the process rewrote its title into one string
func Cmdline(pid int) ([]string, error) {
	data, err := readProc(pid, "cmdline")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		if isZombie(pid) {
			return nil, fmt.Errorf("pid %d exited: %w", pid, ErrNotFound)
		}
		return []string{}, nil
	}
	text := string(data)
	sep := " "
	if strings.HasSuffix(text, "\x00") {
		sep = "\x00"
	}
	text = strings.TrimSuffix(text, sep)
	parts := strings.Split(text, sep)
	if sep == "\x00" && len(parts) == 1 && strings.Contains(text, " ") {
		parts = strings.Split(text, " ")
	}
	return parts, nil
}

// Working directory of pid
func Cwd(pid int) (string, error) { return readLink(pid, "cwd") }

// Environment pid started with or last rewrote
func Environ(pid int) (map[string]string, error) {
	data, err := readProc(pid, "environ")
	if err != nil {
		return nil, err
	}
	return parseEnviron(string(data), false), nil
}

// Real uid of pid
func RealUID(pid int) (int, error) {
	data, err := readProc(pid, "status")
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				return strconv.Atoi(fields[0])
			}
		}
	}
	return 0, fmt.Errorf("pid %d: status has no Uid line", pid)
}

// Memory, thread, CPU and descriptor use of pid from its stat line and descriptor table
func StatsOf(pid int) (Stats, error) {
	fields, err := statFields(pid)
	if err != nil {
		return Stats{}, err
	}
	nums := make([]uint64, len(fields))
	for _, i := range []int{11, 12, 17, 20, 21} {
		if nums[i], err = strconv.ParseUint(fields[i], 10, 64); err != nil {
			return Stats{}, fmt.Errorf("pid %d: stat field %d: %w", pid, i+3, err)
		}
	}
	fds, err := os.ReadDir(procPath(pid, "fd"))
	if err != nil {
		return Stats{}, procError(pid, err)
	}
	tick := float64(time.Second) / clockTicks()
	return Stats{
		RSS:         nums[21] * uint64(os.Getpagesize()),
		VMS:         nums[20],
		Threads:     int(nums[17]),
		User:        time.Duration(float64(nums[11]) * tick),
		System:      time.Duration(float64(nums[12]) * tick),
		Descriptors: len(fds),
	}, nil
}

// Regular files pid holds open, the descriptor links resolved and anything not a regular file skipped
func OpenFiles(pid int) ([]OpenFile, error) {
	dir := procPath(pid, "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, procError(pid, err)
	}
	out := []OpenFile{}
	vanished := false
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		target, err := readLink(pid, "fd/"+e.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				vanished = true
				continue
			}
			if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENAMETOOLONG) {
				continue
			}
			return nil, err
		}
		if !strings.HasPrefix(target, "/") {
			continue
		}
		regular, err := regularFile(target)
		if err != nil {
			return nil, denied(pid, err)
		}
		if regular {
			out = append(out, OpenFile{Path: target, FD: fd})
		}
	}
	if vanished && !Exists(pid) {
		return nil, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	}
	return out, nil
}
