package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procCreateRestrictedToken   = advapi32.NewProc("CreateRestrictedToken")
	procImpersonateLoggedOnUser = advapi32.NewProc("ImpersonateLoggedOnUser")
)

var reportersDACL = []string{"/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F"}

func init() { platformTestModes["acl-probe"] = aclProbeChild }

// Removes SeBackup and SeRestore from the process token so no open bypasses an ACE
func dropPrivileges() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), 0x0020|0x0008, &token); err != nil {
		return err
	}
	defer token.Close()
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
			return err
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: 0x00000004}
		if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Impersonates a UAC filtered token on this thread, Administrators deny only and every privilege but one removed
func impersonateFilteredToken() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), 0x0002|0x0008|0x0001|0x0004, &token); err != nil {
		return err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	disable := windows.SIDAndAttributes{Sid: admins}
	var filtered windows.Token
	if r, _, e := procCreateRestrictedToken.Call(uintptr(token), 0x1, 1, uintptr(unsafe.Pointer(&disable)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&filtered))); r == 0 {
		return e
	}
	if r, _, e := procImpersonateLoggedOnUser.Call(uintptr(filtered)); r == 0 {
		return e
	}
	return nil
}

func probeUsable(directory string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(directory, "probe.lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

func probeWritable(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.WriteString("{}")
	return true, err
}

func runnerIdentity(directory string) string {
	who, _ := exec.Command("whoami", "/user", "/groups", "/priv").Output()
	acl, _ := exec.Command("icacls", directory).Output()
	return string(who) + "\n" + string(acl)
}

// Child role that probes the given paths under the filtered token and prints one JSON line
func aclProbeChild(args []string) int {
	runtime.LockOSThread()
	result, err := func() (map[string]any, error) {
		if err := dropPrivileges(); err != nil {
			return nil, err
		}
		if err := impersonateFilteredToken(); err != nil {
			return nil, err
		}
		out := map[string]any{}
		switch args[0] {
		case "usable":
			for i := 1; i+1 < len(args); i += 2 {
				ok, err := probeUsable(args[i+1])
				if err != nil {
					return nil, err
				}
				out[args[i]] = ok
			}
		case "precondition":
			denied, err := probeUsable(args[1])
			if err != nil {
				return nil, err
			}
			plain, err := probeUsable(args[2])
			if err != nil {
				return nil, err
			}
			identity := ""
			if plain {
				identity = runnerIdentity(args[2])
			}
			out["denied"], out["plain"], out["identity"] = denied, plain, identity
		case "writable":
			ok, err := probeWritable(args[1])
			if err != nil {
				return nil, err
			}
			out["old_lock_writable"] = ok
		}
		return out, nil
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	line, _ := json.Marshal(result)
	fmt.Println(string(line))
	return 0
}

func aclChild(t *testing.T, args ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), testModeEnv+"=acl-probe")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v %s", err, stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	var out map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func icacls(t *testing.T, check bool, args ...string) string {
	t.Helper()
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if check && err != nil {
		t.Fatalf("icacls %v: %v %s", args, err, out)
	}
	return string(out)
}

// Parent directory carrying the reporters' SYSTEM plus Administrators DACL, needing an elevated token
func reportersParent(t *testing.T) string {
	t.Helper()
	if !ProcessIsElevated() {
		t.Skip("needs an elevated token to stand in for the elevated install")
	}
	parent := filepath.Join(t.TempDir(), "icebath")
	if err := os.Mkdir(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	icacls(t, true, append([]string{parent}, reportersDACL...)...)
	t.Cleanup(func() { icacls(t, false, parent, "/reset", "/t", "/c", "/q") })
	return parent
}

// Skips when the filtered child bypasses ACLs or is still granted by the reporters' DACL
func requireFaithfulProbe(t *testing.T, parent, dir string) {
	t.Helper()
	denied := filepath.Join(filepath.Dir(parent), "everyone-denied")
	if err := os.Mkdir(denied, 0o777); err != nil {
		t.Fatal(err)
	}
	icacls(t, true, denied, "/deny", "*S-1-1-0:(F)")
	probe := func() map[string]any {
		defer icacls(t, false, denied, "/remove:d", "*S-1-1-0")
		return aclChild(t, "precondition", denied, dir)
	}()
	if probe["denied"] == true {
		t.Skip("precondition failed: the filtered child still opens a dir an Everyone:(DENY)(F) ACE " +
			"refuses, so it bypasses ACLs on this runner and cannot judge the fix")
	}
	if probe["plain"] == true {
		t.Skip("precondition failed: this runner's filtered token is still granted by the reporters' " +
			"SYSTEM + Administrators DACL, so a dir without the fix is not unusable here:\n" + fmt.Sprint(probe["identity"]))
	}
}

func TestDirMadeWhileElevatedStaysUsableByTheFilteredToken(t *testing.T) {
	parent := reportersParent(t)
	plain := filepath.Join(parent, "plain")
	if err := os.Mkdir(plain, 0o777); err != nil {
		t.Fatal(err)
	}
	requireFaithfulProbe(t, parent, plain)
	granted, err := MakeUserDir(filepath.Join(parent, "gateway-locks"), 0o700, false, false)
	if err != nil {
		t.Fatal(err)
	}
	result := aclChild(t, "usable", "plain", plain, "granted", granted)
	if result["plain"] != false {
		t.Fatal("the filtered token could use the reporters' DACL: the probe is not faithful")
	}
	if result["granted"] != true {
		t.Fatal(result)
	}
}

func TestElevatedRerunRepairsADirAnEarlierElevatedRunLeft(t *testing.T) {
	parent := reportersParent(t)
	left := filepath.Join(parent, "state")
	if err := os.Mkdir(left, 0o777); err != nil {
		t.Fatal(err)
	}
	requireFaithfulProbe(t, parent, left)
	for range 2 {
		if _, err := MakeUserDir(left, 0o777, true, true); err != nil {
			t.Fatal(err)
		}
	}
	after := aclChild(t, "usable", "usable", left)
	if after["usable"] != true {
		t.Fatal(icacls(t, true, left))
	}
}

func TestElevatedRepairAlsoReachesAnOldLockAlreadyInsideTheDir(t *testing.T) {
	parent := reportersParent(t)
	left := filepath.Join(parent, "gateway-locks")
	if err := os.Mkdir(left, 0o777); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(left, "telegram-bot-token-x.lock")
	if err := os.WriteFile(old, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	requireFaithfulProbe(t, parent, left)
	if _, err := MakeUserDir(left, 0o777, true, true); err != nil {
		t.Fatal(err)
	}
	after := aclChild(t, "writable", old)
	if after["old_lock_writable"] != true {
		t.Fatal(icacls(t, true, old))
	}
}

func TestProcessElevationOfOwnPidMatchesThePseudoHandle(t *testing.T) {
	own, ownKnown := ProcessElevation(nil)
	pid := windows.GetCurrentProcessId()
	byPid, pidKnown := ProcessElevation(&pid)
	if !ownKnown || !pidKnown || own != byPid || ProcessIsElevated() != own {
		t.Fatalf("%v %v %v %v", own, ownKnown, byPid, pidKnown)
	}
	missing := uint32(0xFFFFFFF0)
	if _, known := ProcessElevation(&missing); known {
		t.Fatal("a process that cannot be opened has an elevation")
	}
}

func TestNativeGrantWritesTheUserACEOnceOnAPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := osMkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := grantNativeACL(dir, true, "")
	if err != nil || !first {
		t.Fatalf("first grant %v %v", first, err)
	}
	if again, err := grantNativeACL(dir, true, ""); err != nil || again {
		t.Fatalf("second grant grew the DACL %v %v", again, err)
	}
	var refused *ReparsePointRefused
	if _, err := grantNativeACL(dir, true, dir+`\elsewhere`); !errors.As(err, &refused) {
		t.Fatalf("a handle resolving elsewhere was granted %v", err)
	}
}
