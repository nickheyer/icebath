package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/strs"
)

// Process holding the database or one of its logs open, a pid of -1 when the scan could not prove there is none
type holder struct {
	pid    int
	target string
}

// Other processes holding the database, its write-ahead log or its shared memory open. Fails closed: whatever can't be inspected counts
func foreignHolders(path string) []holder {
	watched := map[[2]uint64]string{}
	family := []string{path, path + "-wal", path + "-shm"}
	for _, target := range family {
		info, err := os.Stat(target)
		switch {
		case err == nil:
			dev, ino := identity(info)
			watched[[2]uint64{dev, ino}] = filepath.Base(target)
		case !os.IsNotExist(err):
			return []holder{{pid: -1, target: "watched-file stat failed: " + err.Error()}}
		}
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return []holder{{pid: -1, target: "open-file scan unavailable"}}
	}
	self := os.Getpid()
	var out []holder
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == self {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", proc.Name(), "fd"))
		if err != nil {
			if !os.IsNotExist(err) && icebath(pid) {
				out = append(out, holder{pid: pid, target: "uninspectable holder: " + commandLine(pid)})
			}
			continue
		}
		var held []string
		for _, fd := range fds {
			link := filepath.Join("/proc", proc.Name(), "fd", fd.Name())
			info, err := os.Stat(link)
			if err != nil {
				target, linkErr := os.Readlink(link)
				if linkErr == nil && strings.HasSuffix(target, " (deleted)") && slices.Contains(family, strings.TrimSuffix(target, " (deleted)")) {
					held = append(held, filepath.Base(strings.TrimSuffix(target, " (deleted)"))+" (deleted)")
				}
				continue
			}
			dev, ino := identity(info)
			if name, ok := watched[[2]uint64{dev, ino}]; ok && !slices.Contains(held, name) {
				held = append(held, name)
			}
		}
		if len(held) > 0 {
			out = append(out, holder{pid: pid, target: strings.Join(held, ", ")})
		}
	}
	return out
}

// Whether the process is an icebath binary
func icebath(pid int) bool {
	args := strings.Split(commandLine(pid), " ")
	return len(args) > 0 && strings.HasPrefix(filepath.Base(args[0]), "icebath")
}

// Command line of the process, cut to 80 characters
func commandLine(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "an unknown command"
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
	if len(text) > 80 {
		text = text[:80]
	}
	return text
}

// Processes holding write locks on the database or its shared memory, named by what each lock is, for the log when a write gives up
func lockHolders(path string) []string {
	ranges := map[[2]uint64]string{}
	for _, target := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(target); err == nil {
			dev, ino := identity(info)
			ranges[[2]uint64{dev, ino}] = filepath.Base(target)
		}
	}
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[3] != "WRITE" {
			continue
		}
		parts := strings.Split(fields[5], ":")
		if len(parts) != 3 {
			continue
		}
		major, _ := strconv.ParseUint(parts[0], 16, 32)
		minor, _ := strconv.ParseUint(parts[1], 16, 32)
		ino, _ := strconv.ParseUint(parts[2], 10, 64)
		file, ok := ranges[[2]uint64{major<<8 | minor, ino}]
		if !ok {
			file, ok = ranges[[2]uint64{mkdev(major, minor), ino}]
		}
		if !ok {
			continue
		}
		start, _ := strconv.ParseInt(fields[6], 10, 64)
		end := fields[7]
		owner := "OFD lock, owner pid not exported by the kernel"
		if pid, err := strconv.Atoi(fields[4]); err == nil && pid > 0 {
			owner = fmt.Sprintf("PID %d (%s)", pid, commandLine(pid))
		}
		out = append(out, fmt.Sprintf("%s holds %s lock on %s", owner, lockKind(file, start, end), file))
	}
	return out
}

// Device number as the kernel packs it in stat
func mkdev(major, minor uint64) uint64 {
	return (major&0xfff)<<8 | (major&^0xfff)<<32 | minor&0xff | (minor&^0xff)<<12
}

// What a byte range lock on the database or its shared memory stands for
func lockKind(file string, start int64, end string) string {
	if strings.HasSuffix(file, "-shm") {
		switch {
		case start == 120:
			return "WAL write"
		case start == 121:
			return "WAL checkpoint"
		case start == 122:
			return "WAL recover"
		case start >= 123 && start <= 127:
			return fmt.Sprintf("WAL read slot %d", start-123)
		case start == 128:
			return "shm dead-man switch"
		}
		return fmt.Sprintf("shm bytes %d-%s", start, end)
	}
	switch start {
	case 0x40000000:
		return "PENDING"
	case 0x40000001:
		return "RESERVED"
	case 0x40000002:
		return "SHARED range"
	}
	return fmt.Sprintf("db bytes %d-%s", start, end)
}

func describeHolders(lines []string) string {
	if len(lines) == 0 {
		return "no write-class lock held at the deadline (the holder released just before it)"
	}
	return strings.Join(lines, " | ")
}

// Refusal of a task that rewrites the store while other processes hold it, empty when none do. Holders the scan could not inspect count
// unless forced
func refusal(task, path string, holders []holder, force bool) string {
	if force {
		holders = slices.DeleteFunc(holders, func(h holder) bool { return h.pid <= 0 })
	}
	if len(holders) == 0 {
		return ""
	}
	lines := []string{fmt.Sprintf("icebath sessions %s did not run, because another process is using %s.", task, path)}
	for _, h := range holders {
		if h.pid <= 0 {
			lines = append(lines, "  icebath can't tell whether the database is in use, because it could not check every process: "+
				strings.TrimSuffix(strs.Head(h.target, 120), ".")+".")
			continue
		}
		lines = append(lines, fmt.Sprintf("  Process %d, %s, holds %s.", h.pid, commandLine(h.pid), h.target))
	}
	return strings.Join(append(lines,
		"Changing the database while another process writes to it can stop every agent from replying. Nothing was lost.",
		"Stop them first by quitting the other icebath windows and servers, then run the command again.",
		"To run it anyway at that risk, add --force.",
		"For how to rescue a damaged database, run icebath sessions recover --help."), "\n")
}
