package terminal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// File in the settings folder recording the icebath processes of this machine and the long lived children they own, shared by every
// profile and install
const ledgerName = "spawn-ledger.json"

// Variable a spawner stamps on each long lived child it owns, naming its install, the child's purpose and itself, which an icebath
// child records as its spawner
const SpawnEnv = "ICEBATH_SPAWN"

const spawnTagVersion = "v1"

// Past this a stalled writer of the ledger no longer holds up every spawn of the machine
const ledgerLockWait = 2 * time.Second

// One process the ledger records, with what proves a live process of its pid is still the one recorded
type LedgerEntry struct {
	Pid int `json:"pid"`
	// Start time of the process as StartTime reads it, and the boot it started in, empty where the machine names none
	Start int64  `json:"start_time,omitempty"`
	Boot  string `json:"boot_id,omitempty"`
	// What it is, such as cli, server or terminal-background
	Purpose string `json:"purpose"`
	// Install of icebath that recorded it, by the folder of its program
	Install string `json:"install"`
	// Process that started it and when, none for one that recorded itself without a spawner
	SpawnerPid   int       `json:"spawner_pid,omitempty"`
	SpawnerStart int64     `json:"spawner_start_time,omitempty"`
	Registered   time.Time `json:"registered_at"`
	// First words of its command line
	Argv string `json:"argv"`
	// Folder of the profile it serves
	Profile string `json:"profile"`
}

// Serializes the writes of this process, the lock file those of every other
var ledger struct {
	mu     sync.Mutex
	warned bool
}

func ledgerPath(cfg *config.Config) string {
	return filepath.Join(cfg.Dir(), ledgerName)
}

// Identifier of an install of icebath: twelve hex digits of the digest of the folder its program is in
func Install() string {
	dir := ""
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir = filepath.Dir(exe)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(strings.ToLower(dir)))
	return hex.EncodeToString(sum[:])[:12]
}

// Identifier of the running boot of this machine, empty where the machine names none
func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Tag a long lived child of this process is stamped with, under SpawnEnv
func SpawnTag(purpose string) string {
	start, _ := StartTime(os.Getpid())
	return strings.Join([]string{spawnTagVersion, Install(), purpose, strconv.Itoa(os.Getpid()), strconv.FormatInt(start, 10)}, ":")
}

// Spawner a tag names, its pid and start time; false for a tag that is malformed
func parseSpawnTag(tag string) (int, int64, bool) {
	parts := strings.Split(tag, ":")
	if len(parts) != 5 || parts[0] != spawnTagVersion || parts[1] == "" || parts[2] == "" {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(parts[3])
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pid, start, true
}

// Records this process in the ledger for the purpose, serving the profile, with the spawner its SpawnEnv tag names
func RegisterSelf(cfg *config.Config, purpose string) {
	entry := newEntry(os.Getpid(), purpose, cfg)
	if pid, start, ok := parseSpawnTag(os.Getenv(SpawnEnv)); ok {
		entry.SpawnerPid, entry.SpawnerStart = pid, start
	}
	argv := os.Args[:min(len(os.Args), 10)]
	entry.Argv = strings.Join(argv, " ")
	appendEntry(cfg, entry)
}

// Records a child this process just started for the purpose, serving the profile, with this process as its spawner
func RegisterChild(cfg *config.Config, pid int, purpose string) {
	if pid <= 0 {
		return
	}
	entry := newEntry(pid, purpose, cfg)
	if entry.Start == 0 {
		return
	}
	entry.SpawnerPid = os.Getpid()
	entry.SpawnerStart, _ = StartTime(entry.SpawnerPid)
	if argv := cmdline(pid); len(argv) > 0 {
		entry.Argv = strings.Join(argv[:min(len(argv), 10)], " ")
	}
	appendEntry(cfg, entry)
}

func newEntry(pid int, purpose string, cfg *config.Config) LedgerEntry {
	start, _ := StartTime(pid)
	return LedgerEntry{Pid: pid, Start: start, Boot: bootID(), Purpose: purpose, Install: Install(), Registered: time.Now(),
		Profile: cfg.ProfileDir(cfg.Profile())}
}

// Command line of a process of this machine, from /proc or else ps; nil when it can't be read
func cmdline(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil
		}
		return strings.Fields(string(out))
	}
	var argv []string
	for _, word := range bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0}) {
		argv = append(argv, string(word))
	}
	return argv
}

// Whether the entry's process still runs as the process recorded, strictly only when its start time proves it
func (e LedgerEntry) running(strict bool) bool {
	if e.Pid <= 0 || !alive(e.Pid) {
		return false
	}
	if e.Boot != "" && e.Boot != bootID() {
		return false
	}
	if e.Start == 0 {
		return !strict
	}
	current, ok := StartTime(e.Pid)
	if !ok {
		return !strict
	}
	if strict {
		return current == e.Start
	}
	return SameStart(e.Start, current)
}

// Entries of the ledger, ok false when the file can't be parsed; a missing or blank one holds none
func readLedger(path string) ([]LedgerEntry, bool) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, true
	}
	var entries []LedgerEntry
	if json.Unmarshal(data, &entries) != nil {
		return nil, false
	}
	return entries, true
}

// Entries of the ledger, one that can't be parsed moved aside so the next write starts it over; the caller holds the ledger
func readOrQuarantine(path string) []LedgerEntry {
	entries, ok := readLedger(path)
	if !ok {
		if os.Rename(path, path+".corrupt") == nil {
			slog.Warn(fmt.Sprintf("spawn ledger was unreadable; moved to %s", path+".corrupt"))
		}
	}
	return entries
}

// Writes the entry with the entries still running, a row of the same pid replaced: the only way the ledger is written
func appendEntry(cfg *config.Config, entry LedgerEntry) {
	path := ledgerPath(cfg)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Debug("spawn ledger write failed", "error", err)
		return
	}
	if unlock, err := fsx.Lock(context.Background(), path+".lock", ledgerLockWait); err != nil {
		if !ledger.warned {
			ledger.warned = true
			slog.Warn("spawn ledger lock unavailable (a stalled holder, or no file locks here); writing without it, so concurrent icebath " +
				"processes may drop each other's rows")
		}
		slog.Debug("spawn ledger lock unavailable", "error", err)
	} else {
		defer unlock()
	}
	var kept []LedgerEntry
	for _, e := range readOrQuarantine(path) {
		if e.Pid != entry.Pid && e.running(false) {
			kept = append(kept, e)
		}
	}
	data, err := json.MarshalIndent(append(kept, entry), "", "  ")
	if err == nil {
		err = fsx.Write(path, data, 0o600)
	}
	if err != nil {
		slog.Debug("spawn ledger write failed", "error", err)
	}
}

// Entries of every install whose process is proven still running by its start time
func live(cfg *config.Config) []LedgerEntry {
	ledger.mu.Lock()
	entries := readOrQuarantine(ledgerPath(cfg))
	ledger.mu.Unlock()
	var out []LedgerEntry
	for _, e := range entries {
		if e.running(true) {
			out = append(out, e)
		}
	}
	return out
}

// Warning when another install of icebath, proven running, serves the profile in use, empty otherwise
func SharedProfile(cfg *config.Config) string {
	own, profile := Install(), cfg.ProfileDir(cfg.Profile())
	for _, e := range live(cfg) {
		if e.Install != "" && e.Install != own && e.Profile == profile {
			return "Another icebath installation is using this profile, and the two share its settings and data, so changes made in one can " +
				"undo changes made in the other. Close the other installation before you change anything here."
		}
	}
	return ""
}
