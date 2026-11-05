//go:build unix

package state

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// Plain comparable form of parsed locks for table assertions
type procLockRow struct {
	PID     string
	Kind    string
	Sidecar string
}

func procLockRows(locks []ProcLock) []procLockRow {
	rows := []procLockRow{}
	for _, l := range locks {
		rows = append(rows, procLockRow{l.PID.String(), l.Kind, l.Sidecar})
	}
	return rows
}

func TestParseProcLocksKeepsOnlyWriteLocksOnOurInodesAndDecodesTheWALWriteByte(t *testing.T) {
	inodes := map[DevIno]string{{unix.Mkdev(0x103, 0x02), 4194737}: "-shm", {unix.Mkdev(0x103, 0x02), 4228330}: ""}
	text := "1: POSIX  ADVISORY  WRITE 594094 103:02:4194737 120 120\n" +
		"2: POSIX  ADVISORY  READ 594094 103:02:4194737 125 125\n" +
		"3: POSIX  ADVISORY  READ 99493 103:02:4194737 128 128\n" +
		"4: OFDLCK ADVISORY  WRITE -1 103:02:4228330 1073741825 1073741825\n" +
		"5: POSIX  ADVISORY  WRITE 4242 103:02:99999 120 120\n" +
		"6: -> POSIX  ADVISORY  WRITE 777 103:02:4194737 120 120\n"
	got, err := ParseProcLocks(text, inodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []procLockRow{{"594094", "WAL write", "-shm"}, {"-1", "RESERVED", ""}}
	if !reflect.DeepEqual(procLockRows(got), want) {
		t.Fatalf("got %v want %v", procLockRows(got), want)
	}
}

func TestParseProcLocksDecodesEveryRangeShape(t *testing.T) {
	dev := unix.Mkdev(0x103, 0x02)
	inodes := map[DevIno]string{{dev, 1}: "", {dev, 2}: "-wal", {dev, 3}: "-shm", {unix.Mkdev(0xffffffff, 0), 4}: ""}
	text := "1: POSIX ADVISORY WRITE 10 103:02:3 121 121\r\n" +
		"2: POSIX ADVISORY WRITE 10 103:02:3 122 122\r" +
		"3: POSIX ADVISORY WRITE 10 103:02:3 128 128\u0085" +
		"4: POSIX ADVISORY WRITE 10 103:02:3 123 123 " +
		"5: POSIX ADVISORY WRITE 10 103:02:3 127 127\v" +
		"6: POSIX ADVISORY WRITE 10 103:02:3 120 121\f" +
		"7: POSIX ADVISORY WRITE 10 103:02:3 125 EOF\n" +
		"8: POSIX ADVISORY WRITE 10 103:02:1 1073741824 1073741824\n" +
		"9: POSIX ADVISORY WRITE 10 103:02:1 1073741826 1073742335\n" +
		"10: POSIX ADVISORY WRITE 10 103:02:1 0 EOF\n" +
		"11: POSIX ADVISORY WRITE 10 0x103:0X0_2:2 120 120\n" +
		"12: POSIX ADVISORY WRITE 10 -1:0:4 1073741825 1073741825 extra fields\n" +
		"13: POSIX ADVISORY WRITE 10 103:02 120 120\n" +
		"14: POSIX ADVISORY WRITE 10 1g3:02:3 120 120\n" +
		"15: POSIX ADVISORY WRITE 10 103:0z:3 120 120\n" +
		"16: POSIX ADVISORY WRITE x 103:02:3 120 120\n" +
		"17: POSIX ADVISORY WRITE 10 103:02:x 120 120\n" +
		"18: POSIX ADVISORY WRITE 10 103:02:3 x 120\n" +
		"19: POSIX ADVISORY WRITE 10 103:02:3 120 x\n" +
		"20: POSIX ADVISORY WRITE 10 103:02:-3 120 120\n" +
		"21: POSIX ADVISORY WRITE 99999999999999999999 103:02:3 1_2_0 120\n" +
		"22: POSIX ADVISORY WRITE\n"
	got, err := ParseProcLocks(text, inodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []procLockRow{
		{"10", "WAL checkpoint", "-shm"},
		{"10", "WAL recover", "-shm"},
		{"10", "shm dead-man switch", "-shm"},
		{"10", "WAL read slot 0", "-shm"},
		{"10", "WAL read slot 4", "-shm"},
		{"10", "shm bytes 120-121", "-shm"},
		{"10", "WAL read slot 2", "-shm"},
		{"10", "PENDING", ""},
		{"10", "SHARED range", ""},
		{"10", "db bytes 0-0", ""},
		{"10", "db bytes 120-120", "-wal"},
		{"10", "RESERVED", ""},
		{"99999999999999999999", "WAL write", "-shm"},
	}
	if !reflect.DeepEqual(procLockRows(got), want) {
		t.Fatalf("got\n%v\nwant\n%v", procLockRows(got), want)
	}
}

func TestParseProcLocksRaisesTheMakedevOverflow(t *testing.T) {
	inodes := map[DevIno]string{}
	for ident, msg := range map[string]string{
		"-2:0:1":                "can't convert negative int to unsigned",
		"100000000:0:1":         "Python int too large to convert to C unsigned int",
		"0:-5:1":                "can't convert negative int to unsigned",
		"10000000000000000:0:1": "int too big to convert",
	} {
		_, err := ParseProcLocks("1: POSIX ADVISORY WRITE 1 "+ident+" 0 0\n", inodes)
		var conv *pyConvError
		if !errors.As(err, &conv) || conv.Class != "OverflowError" || conv.Msg != msg {
			t.Fatalf("%s raised %v", ident, err)
		}
	}
	if locks, err := ParseProcLocks("1: POSIX ADVISORY WRITE 1 -2:zz:1 0 0\n", inodes); err != nil || len(locks) != 0 {
		t.Fatalf("an unparsable minor skips the line before makedev runs, got %v %v", locks, err)
	}
}

func TestLockOwnerDescriptionForOFDAndUnrepresentablePIDs(t *testing.T) {
	if got := describeLockOwner(big.NewInt(-1)); got != "OFD lock, owner pid not exported by the kernel" {
		t.Fatal(got)
	}
	if got := describeLockOwner(big.NewInt(0)); got != "OFD lock, owner pid not exported by the kernel" {
		t.Fatal(got)
	}
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	if got := describeLockOwner(huge); got != "PID 99999999999999999999 (command line unavailable)" {
		t.Fatal(got)
	}
}
