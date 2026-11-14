package home

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestElevatedUpdateStepMakesAStrandedHomeUsable(t *testing.T) {
	parent := reportersParent(t)
	nested := filepath.Join(parent, "sessions", "2026")
	if err := os.MkdirAll(nested, 0o777); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(parent, "gateway-locks")
	if err := os.Mkdir(old, 0o777); err != nil {
		t.Fatal(err)
	}
	writeACLFile(t, filepath.Join(old, "telegram-bot-token-x.lock"), "{}")
	requireFaithfulProbe(t, parent, nested)
	t.Setenv("ICEBATH_HOME", parent)
	if err := RepairWindowsHomeACL(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	after := aclChild(t, "usable", "nested", nested, "locks", old, "home", parent)
	if !reflect.DeepEqual(after, map[string]any{"nested": true, "locks": true, "home": true}) {
		t.Fatal(after)
	}
}

// Creates link as a directory junction to target like the winapi CreateJunction call
func createJunction(t *testing.T, target, link string) {
	t.Helper()
	full, err := windows.FullPath(target)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := windows.UTF16FromString(`\??\` + full)
	prt, _ := windows.UTF16FromString(full)
	subLen, prtLen := (len(sub)-1)*2, (len(prt)-1)*2
	pathBytes := len(sub)*2 + len(prt)*2
	buf := make([]byte, 16+pathBytes)
	binary.LittleEndian.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:], uint16(8+pathBytes))
	binary.LittleEndian.PutUint16(buf[10:], uint16(subLen))
	binary.LittleEndian.PutUint16(buf[12:], uint16(subLen+2))
	binary.LittleEndian.PutUint16(buf[14:], uint16(prtLen))
	off := 16
	for _, u := range append(sub, prt...) {
		binary.LittleEndian.PutUint16(buf[off:], u)
		off += 2
	}
	linkPtr, _ := windows.UTF16PtrFromString(link)
	if err := windows.CreateDirectory(linkPtr, nil); err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(linkPtr, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var returned uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, (*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf)), nil, 0, &returned, nil); err != nil {
		t.Fatal(err)
	}
}

// Home holding a junction to a directory outside it, standing in for a planted link to Program Files
func homeWithJunctionOut(t *testing.T) (string, string, string) {
	t.Helper()
	if !ProcessIsElevated() {
		t.Skip("needs an elevated token: only an elevated run grants")
	}
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath")
	if err := os.MkdirAll(filepath.Join(h, "cache"), 0o777); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "Program Files", "App")
	if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	writeACLFile(t, filepath.Join(outside, "bin", "app.exe"), "x")
	icacls(t, true, append([]string{outside}, reportersDACL...)...)
	junction := filepath.Join(h, "cache", "images")
	createJunction(t, outside, junction)
	t.Setenv("ICEBATH_HOME", h)
	t.Cleanup(func() {
		os.Remove(junction)
		icacls(t, false, outside, "/reset", "/t", "/c", "/q")
	})
	return h, outside, junction
}

// icacls stdout of each path with the path itself normalized for comparison
func dacls(t *testing.T, paths []string) []string {
	t.Helper()
	out := make([]string, len(paths))
	for i, p := range paths {
		text, err := exec.Command("icacls", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		out[i] = strings.ReplaceAll(string(text), p, "<p>")
	}
	return out
}

func TestElevatedGrantsNeverReachAJunctionTargetOutsideTheHome(t *testing.T) {
	h, outside, junction := homeWithJunctionOut(t)
	if !IsReparsePoint(junction) {
		t.Fatal("junction is not a reparse point")
	}
	watched := []string{outside}
	var below []string
	filepath.WalkDir(outside, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != outside {
			below = append(below, p)
		}
		return nil
	})
	slices.Sort(below)
	watched = append(watched, below...)
	before := dacls(t, watched)
	resetGranted()
	MakeUserDir(context.Background(), junction, 0o777, true, true)
	MakeUserDir(context.Background(), filepath.Join(junction, "thumbs"), 0o777, true, true)
	if GrantUserAccess(context.Background(), junction, ProcessIsElevated(), false) {
		t.Fatal("a grant on the junction itself was attempted")
	}
	granted, skipped, failed, err := GrantTreeUserAccess(h, false, ProcessIsElevated())
	if err != nil {
		t.Fatal(err)
	}
	if err := RepairWindowsHomeACL(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(skipped, junction) || len(failed) != 0 || granted < 2 {
		t.Fatalf("granted %d skipped %v failed %v", granted, skipped, failed)
	}
	if after := dacls(t, watched); !reflect.DeepEqual(after, before) {
		t.Fatal("an elevated grant followed the junction out of the home")
	}
	who, err := exec.Command("whoami", "/user", "/fo", "csv", "/nh").Output()
	if err != nil {
		t.Fatal(err)
	}
	sid := strings.Trim(strings.TrimSpace(string(who)[strings.LastIndex(string(who), ",")+1:]), `"`)
	homeACL, err := exec.Command("icacls", filepath.Join(h, "cache")).Output()
	if err != nil {
		t.Fatal(err)
	}
	user, err := exec.Command("whoami").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(homeACL), sid) && !strings.Contains(strings.ToLower(string(homeACL)), strings.ToLower(strings.TrimSpace(string(user)))) {
		t.Fatal(string(homeACL))
	}
}
