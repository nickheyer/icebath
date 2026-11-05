package state

import (
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"strings"
)

const lockOwnersPendingByte = 0x40000000

var (
	shmLockNames = map[int64]string{
		120: "WAL write",
		121: "WAL checkpoint",
		122: "WAL recover",
		128: "shm dead-man switch",
	}
	mainLockNames = map[int64]string{
		lockOwnersPendingByte:     "PENDING",
		lockOwnersPendingByte + 1: "RESERVED",
	}
)

// Device and inode pair naming one file, the os.stat st_dev and st_ino key
type DevIno struct {
	Dev uint64
	Ino uint64
}

// One write lock from /proc/locks with its owner pid, -1 for an OFD lock, its kind and the sidecar suffix
type ProcLock struct {
	PID     *big.Int
	Kind    string
	Sidecar string
}

// Small int64 value of a parsed integer, false when it is out of int64 range
func bigSmall(n *big.Int) (int64, bool) {
	if n.IsInt64() {
		return n.Int64(), true
	}
	return 0, false
}

// Operator name of a locked byte range on the main file or one of its sidecars
func describeRange(sidecar string, start, end *big.Int) string {
	s, sOK := bigSmall(start)
	same := start.Cmp(end) == 0
	if sidecar == "-shm" {
		if name, ok := shmLockNames[s]; sOK && ok && same {
			return name
		}
		if sOK && s >= 123 && s <= 127 && same {
			return fmt.Sprintf("WAL read slot %d", s-123)
		}
		return fmt.Sprintf("shm bytes %s-%s", start, end)
	}
	if name, ok := mainLockNames[s]; sOK && ok && same {
		return name
	}
	if sOK && s == lockOwnersPendingByte+2 {
		return "SHARED range"
	}
	return fmt.Sprintf("db bytes %s-%s", start, end)
}

// Every WRITE lock of /proc/locks text on the inodes, waiters and read locks dropped, makedev errors raised
func ParseProcLocks(text string, inodes map[DevIno]string) ([]ProcLock, error) {
	var found []ProcLock
	for _, line := range pySplitlines(text) {
		fields := pySplitWhitespace(line)
		if len(fields) < 8 || fields[1] == "->" {
			continue
		}
		access, pidS, ident, startS, endS := fields[3], fields[4], fields[5], fields[6], fields[7]
		if access != "WRITE" {
			continue
		}
		parts := strings.Split(ident, ":")
		if len(parts) != 3 {
			continue
		}
		makedev, err := osMakedev()
		if err != nil {
			return nil, err
		}
		major, ok := pyIntBase16(parts[0])
		if !ok {
			continue
		}
		minor, ok := pyIntBase16(parts[1])
		if !ok {
			continue
		}
		dev, err := makedev(major, minor)
		if err != nil {
			return nil, err
		}
		ino, err := pyIntOf(parts[2])
		if err != nil {
			continue
		}
		pid, err := pyIntOf(pidS)
		if err != nil {
			continue
		}
		start, err := pyIntOf(startS)
		if err != nil {
			continue
		}
		end := start
		if endS != "EOF" {
			if end, err = pyIntOf(endS); err != nil {
				continue
			}
		}
		if !ino.IsUint64() {
			continue
		}
		sidecar, ok := inodes[DevIno{Dev: dev, Ino: ino.Uint64()}]
		if !ok {
			continue
		}
		found = append(found, ProcLock{PID: pid, Kind: describeRange(sidecar, start, end), Sidecar: sidecar})
	}
	return found, nil
}

// Converts one os.makedev argument the way the dev_t converter does, -1 standing for NODEV
func makedevArg(n *big.Int) (uint64, error) {
	if n.Sign() < 0 {
		if n.IsInt64() && n.Int64() == -1 {
			return math.MaxUint64, nil
		}
		return 0, &pyConvError{"OverflowError", "can't convert negative int to unsigned"}
	}
	if !n.IsUint64() {
		return 0, &pyConvError{"OverflowError", "int too big to convert"}
	}
	return n.Uint64(), nil
}

// Major and minor numbers as the C unsigned int makedev takes, NODEV truncated to all ones
func makedevParts(major, minor *big.Int) (uint32, uint32, error) {
	maj, err := makedevArg(major)
	if err != nil {
		return 0, 0, err
	}
	mnr, err := makedevArg(minor)
	if err != nil {
		return 0, 0, err
	}
	for _, v := range []uint64{maj, mnr} {
		if v > math.MaxUint32 && v != math.MaxUint64 {
			return 0, 0, &pyConvError{"OverflowError", "Python int too large to convert to C unsigned int"}
		}
	}
	return uint32(maj), uint32(mnr), nil
}

// Lines naming every process that holds a write class lock on the database or its sidecars, empty off Linux
func StateDBWriteLockHolders(dbPath string) ([]string, error) {
	return writeLockHolders(dbPath)
}

// Union of three /proc/locks passes since lock churn elsewhere can shift the file mid read and skip the holder
func procLocksHeld(inodes map[DevIno]string) ([]ProcLock, bool, error) {
	var held []ProcLock
	for range 3 {
		text, err := readProcLocks()
		if err != nil {
			return nil, false, nil
		}
		entries, err := ParseProcLocks(text, inodes)
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			if !containsProcLock(held, e) {
				held = append(held, e)
			}
		}
	}
	return held, true, nil
}

func containsProcLock(held []ProcLock, e ProcLock) bool {
	for _, h := range held {
		if h.PID.Cmp(e.PID) == 0 && h.Kind == e.Kind && h.Sidecar == e.Sidecar {
			return true
		}
	}
	return false
}

// Holder description of a lock owner, the OFD text for pids the kernel does not export
func describeLockOwner(pid *big.Int) string {
	if pid.Sign() <= 0 {
		return "OFD lock, owner pid not exported by the kernel"
	}
	if p, ok := bigSmall(pid); ok && int64(int(p)) == p {
		return DescribeHolderPID(int(p))
	}
	return fmt.Sprintf("PID %s (command line unavailable)", pid)
}

// Reads /proc/locks decoded as ASCII with every other byte replaced by U+FFFD
var readProcLocks = func() (string, error) {
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range raw {
		if c < 0x80 {
			b.WriteByte(c)
		} else {
			b.WriteRune('�')
		}
	}
	return b.String(), nil
}

// Logs one warning naming the write lock holders at the moment a writer gave up waiting
func LogWriteLockHolders(dbPath string, patienceS float64) error {
	holders, err := StateDBWriteLockHolders(dbPath)
	if err != nil {
		return err
	}
	detail := "no write-class lock held at the deadline (the holder released just before it)"
	if len(holders) > 0 {
		detail = strings.Join(holders, " | ")
	}
	slog.Warn(fmt.Sprintf("state.db write lock unavailable for %ss (%s): %s", pyPercentF0(patienceS), pathlibName(dbPath), detail))
	return nil
}

// Python percent .0f formatting with the nan and inf spellings
func pyPercentF0(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	return fmt.Sprintf("%.0f", v)
}
