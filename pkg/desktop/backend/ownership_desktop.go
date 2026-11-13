package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const ownedBackendStopGrace = 1500 * time.Millisecond

func boolRef(v bool) *bool { return &v }

func missingProcess(err error) bool {
	return errors.Is(err, ErrNoProcess) || errors.Is(err, fs.ErrNotExist)
}

// Whether the process behind an entry is still the recorded one, nil when that cannot be verified
func ProcessIdentityMatches(entry OwnershipEntry) *bool {
	if IsPidOnlyStartMarker(entry.StartMarker) {
		if IsPidAlive(entry.Pid) {
			return boolRef(true)
		}
		return boolRef(false)
	}
	marker, err := ProcessStartMarker(entry.Pid)
	if err != nil {
		if missingProcess(err) {
			return boolRef(false)
		}
		return nil
	}
	return boolRef(marker == entry.StartMarker)
}

// Command line of pid from ps, or Win32_Process on Windows, empty when it cannot be read
func BackendCommandForPid(pid int) string {
	command, args := "ps", []string{"-p", strconv.Itoa(pid), "-o", "command="}
	if runtime.GOOS == "windows" {
		command, args = "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf("(Get-CimInstance Win32_Process -Filter 'ProcessId = %d').CommandLine", pid)}
	}
	out, err := ExecText(command, args, ReapProbeTimeout)
	if err != nil {
		return ""
	}
	return out
}

// Whether an entry is still the recorded process running an icebath backend command
func BackendIdentityMatches(entry OwnershipEntry) (*bool, error) {
	matches := ProcessIdentityMatches(entry)
	if matches == nil || !*matches {
		return matches, nil
	}
	command := BackendCommandForPid(entry.Pid)
	if command == "" {
		return nil, nil
	}
	return boolRef(BackendCommandMatches(command)), nil
}

// Whether the desktop that spawned an entry is still alive, nil when the entry names no parent
func BackendParentMatches(entry OwnershipEntry) (*bool, error) {
	if entry.ParentPid == 0 || entry.ParentStartMarker == "" {
		return nil, nil
	}
	marker, err := ProcessStartMarker(entry.ParentPid)
	if err != nil {
		if missingProcess(err) {
			return boolRef(false), nil
		}
		return nil, nil
	}
	return boolRef(marker == entry.ParentStartMarker), nil
}

// Stops a verified owned backend, the whole tree on Windows and its group elsewhere, failing when it survives
func StopOwnedBackend(entry OwnershipEntry, log func(string)) error {
	matches := ProcessIdentityMatches(entry)
	if matches != nil && !*matches {
		return nil
	}
	if matches == nil {
		return fmt.Errorf("Could not verify backend PID %d before stopping it.", entry.Pid)
	}
	if runtime.GOOS == "windows" {
		if err := TaskkillTree(entry.Pid); err != nil {
			if still := ProcessIdentityMatches(entry); still == nil || *still {
				return fmt.Errorf("taskkill failed for backend PID %d: %s", entry.Pid, err.Error())
			}
			log(fmt.Sprintf("taskkill reported failure for already-gone backend PID %d: %s", entry.Pid, err.Error()))
		}
	} else {
		if KillGroup(-entry.Pid, "SIGTERM") != nil {
			if p, err := os.FindProcess(entry.Pid); err != nil || p.Signal(syscall.SIGTERM) != nil {
				return nil
			}
		}
		deadline := time.Now().Add(ownedBackendStopGrace)
		for time.Now().Before(deadline) {
			if m := ProcessIdentityMatches(entry); m == nil || !*m {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		if m := ProcessIdentityMatches(entry); m != nil && *m {
			if KillGroup(-entry.Pid, "SIGKILL") != nil {
				p, err := os.FindProcess(entry.Pid)
				if err == nil {
					err = p.Kill()
				}
				if err != nil {
					return err
				}
			}
		}
	}
	time.Sleep(50 * time.Millisecond)
	if remaining := ProcessIdentityMatches(entry); remaining == nil || *remaining {
		return fmt.Errorf("Backend PID %d did not stop cleanly.", entry.Pid)
	}
	return nil
}

// ownershipFile is backend-ownership.json in the desktop user data directory
type ownershipFile struct {
	path string
	log  func(string)
}

func (f ownershipFile) Read() (string, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return "", nil
	}
	return string(raw), nil
}

func (f ownershipFile) Write(contents string) error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o777); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.%d.tmp", f.path, os.Getpid())
	defer os.Remove(temp)
	if err := os.WriteFile(temp, []byte(contents), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, f.path)
}

func (f ownershipFile) Quarantine() error {
	parked := f.path + ".corrupt"
	if err := os.Rename(f.path, parked); err == nil {
		f.log("Backend ownership file was unreadable; moved to " + parked)
	}
	return nil
}

// Ownership over userData/backend-ownership.json with the real process probes
func NewDesktopOwnership(userData string, log func(string)) *Ownership {
	return NewOwnership(OwnershipDeps{
		MatchesIdentity: BackendIdentityMatches,
		MatchesParent:   BackendParentMatches,
		Stop:            func(e OwnershipEntry) error { return StopOwnedBackend(e, log) },
		Store:           ownershipFile{path: filepath.Join(userData, "backend-ownership.json"), log: log},
		ReapDeadline:    ReapOrphansDeadline,
		Now:             time.Now,
	})
}
