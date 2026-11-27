package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Launch record of a Chromium: who owns it, which process it is, the profile it runs on and the PID namespace it was written in
type record struct {
	OwnerPID   int     `json:"owner_pid"`
	OwnerStart *int64  `json:"owner_start_time"`
	PID        int     `json:"pid"`
	Start      *int64  `json:"start_time"`
	Dir        string  `json:"user_data_dir"`
	Profile    string  `json:"home_key"`
	Ephemeral  bool    `json:"ephemeral"`
	NS         *string `json:"ns"`
	Devtools   *string `json:"devtools"`
}

// A process's hold on a shared Chromium, beside the record of it
type hold struct {
	HolderPID   int     `json:"holder_pid"`
	HolderStart *int64  `json:"holder_start_time"`
	PID         int     `json:"pid"`
	Start       *int64  `json:"start_time"`
	Dir         string  `json:"user_data_dir"`
	NS          *string `json:"ns"`
}

const holdersLock = ".holders.lock"

// Inode of this process's PID namespace with the kernel's boot id, false where they can't be read
var namespace = sync.OnceValues(func() (string, bool) {
	ino, ok := pidNamespace()
	if !ok {
		return "", false
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%s", ino, strings.TrimSpace(string(boot))), true
})

func nsValue() *string {
	if identity, ok := namespace(); ok {
		return &identity
	}
	return nil
}

// Name suffix of this namespace's records, holds and throwaway folders, empty without an identity
func nsTag() string {
	identity, ok := namespace()
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return "-n" + hex.EncodeToString(sum[:])[:12]
}

// Whether a record or hold was written in this PID namespace and boot
func sameNamespace(ns *string) bool {
	identity, ok := namespace()
	if !ok {
		return ns == nil
	}
	return ns != nil && *ns == identity
}

func startOf(pid int) *int64 {
	if v, ok := terminal.StartTime(pid); ok {
		return &v
	}
	return nil
}

func sameStart(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// Whether the pid still is the process that wrote a record or hold: alive, with the start time recorded
func processAlive(pid int, start *int64) bool {
	if pid <= 0 || !terminal.Running(pid) {
		return false
	}
	return start == nil || sameStart(startOf(pid), start)
}

// Reads the JSON of a record or hold into v, removing a file that can't be read as one
func loadState(path string, v any) bool {
	raw, err := os.ReadFile(path)
	if err == nil && json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), v) == nil {
		return true
	}
	os.Remove(path)
	return false
}

// Writes JSON through a temp file and a rename
func writeState(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsx.Write(path, data, 0o600)
}

// Exclusive lock of a launcher state folder across processes; held is true whenever blocking
func recordsLock(records string, blocking bool) (held bool, unlock func(), err error) {
	path := filepath.Join(records, holdersLock)
	if blocking {
		release, err := fsx.Lock(context.Background(), path, -1)
		return err == nil, release, err
	}
	release, ok, err := fsx.TryLock(path)
	if err != nil {
		return false, nil, err
	}
	if !ok {
		release = func() {}
	}
	return ok, release, nil
}

func stem(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func holdPath(record string, holder int) string {
	return filepath.Join(filepath.Dir(record), fmt.Sprintf("%s@%d%s.lease", stem(record), holder, nsTag()))
}

// Notes that this process holds the browser, the records lock held
func writeHold(record string, b *launched) error {
	me := os.Getpid()
	return writeState(holdPath(record, me), hold{HolderPID: me, HolderStart: startOf(me), PID: b.pid, Start: b.start, Dir: b.dir, NS: nsValue()})
}

func dropHold(record string) { os.Remove(holdPath(record, os.Getpid())) }

// Whether the hold's process is proven gone: written in this namespace and boot by a process that no longer runs as it did
func holderGone(h hold) bool {
	return sameNamespace(h.NS) && !processAlive(h.HolderPID, h.HolderStart)
}

// Holds of other live processes on the browser a record names, dead or stale ones removed
func otherHolders(record string, browserStart *int64) []hold {
	prefix := stem(record) + "@"
	entries, _ := os.ReadDir(filepath.Dir(record))
	var paths []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".lease") {
			paths = append(paths, filepath.Join(filepath.Dir(record), e.Name()))
		}
	}
	slices.Sort(paths)
	var holders []hold
	for _, path := range paths {
		var h hold
		if !loadState(path, &h) || h.HolderPID == os.Getpid() && sameNamespace(h.NS) {
			continue
		}
		otherLaunch := !sameStart(h.Start, browserStart) && (sameNamespace(h.NS) || h.Start != nil)
		if otherLaunch || holderGone(h) {
			os.Remove(path)
			continue
		}
		holders = append(holders, h)
	}
	return holders
}

// Removes the holds whose holder is gone, the records lock held
func dropDeadHolds(records string) {
	entries, _ := os.ReadDir(records)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lease") {
			continue
		}
		path := filepath.Join(records, e.Name())
		var h hold
		if loadState(path, &h) && holderGone(h) {
			os.Remove(path)
		}
	}
}

// The live holder of this namespace a record can be handed to
func successor(holders []hold) *hold {
	for i := range holders {
		if sameNamespace(holders[i].NS) {
			return &holders[i]
		}
	}
	return nil
}

// Makes the holder the record's owner, so the last holder out stops the browser
func handOver(path string, rec record, heir *hold) error {
	rec.OwnerPID, rec.OwnerStart = heir.HolderPID, heir.HolderStart
	if err := writeState(path, rec); err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("browser: Chromium pid %d is still used by Icebath pid %d; handed it over instead of stopping it", rec.PID, heir.HolderPID))
	return nil
}
