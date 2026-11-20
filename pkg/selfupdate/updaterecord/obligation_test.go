package updaterecord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var obSHA = strings.Repeat("a", 40)

func obLockDir(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "gateway-locks")
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", dir)
	return dir
}

func obInstall(t *testing.T, root string) {
	saved := obligationInstall
	obligationInstall = func() string { return root }
	t.Cleanup(func() { obligationInstall = saved })
}

func obTwoProfiles(t *testing.T) (string, map[string]string) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(tmp, "gateway-locks"))
	homes := map[string]string{}
	for _, name := range []string{"coder", "writer"} {
		homes[name] = filepath.Join(tmp, "profiles", name)
		if err := os.MkdirAll(homes[name], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return tmp, homes
}

func obSkipRoot(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission bits are not enforced for this user")
	}
}

func TestObligationOneInstallsCompletedRestartNeverErasesAnotherInstallsDebt(t *testing.T) {
	obLockDir(t)
	installA, installB := filepath.Join(t.TempDir(), "install-a"), filepath.Join(t.TempDir(), "install-b")
	bSHA := strings.Repeat("b", 40)
	obInstall(t, installA)
	if !WriteHostObligation(context.Background(), obSHA, nil, "", "run-a") {
		t.Fatal("install a could not arm")
	}
	obligationInstall = func() string { return installB }
	if !WriteHostObligation(context.Background(), bSHA, nil, "", "run-b") {
		t.Fatal("install b could not arm")
	}
	MarkHostRestartCompleted(context.Background(), bSHA)
	ClearHostObligation()
	obligationInstall = func() string { return installA }
	if got := mapGet(ReadHostObligation(), "expected_sha"); got != obSHA {
		t.Fatalf("install a debt = %v", got)
	}
	obligationInstall = func() string { return installB }
	if HostObligationPresent() {
		t.Fatal("install b still owes after its own completion")
	}
}

func TestObligationCompletionNeverErasesTheUnkeyedDebtAnOlderInstallArmed(t *testing.T) {
	for _, aSHA := range []string{obSHA, strings.Repeat("b", 40)} {
		t.Run(aSHA[:1], func(t *testing.T) {
			lockDir := obLockDir(t)
			legacy := filepath.Join(lockDir, HostObligationName)
			if !WriteHostObligation(context.Background(), strings.Repeat("b", 40), nil, "", "run-b") {
				t.Fatal("arm failed")
			}
			body := `{"version": 1, "pid": 1, "expected_sha": "` + aSHA + `"}`
			if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			MarkHostRestartCompleted(context.Background(), strings.Repeat("b", 40))
			ClearHostObligation()
			raw, err := os.ReadFile(legacy)
			if err != nil || !strings.Contains(string(raw), aSHA) {
				t.Fatalf("unkeyed debt erased: %v %s", err, raw)
			}
			if got := mapGet(ReadHostObligation(), "expected_sha"); got != aSHA {
				t.Fatalf("reader honours %v", got)
			}
			ClearHostObligation()
			if HostObligationPresent() {
				t.Fatal("the judged unkeyed record survived its discharge")
			}
		})
	}
}

func TestObligationEveryHostRecordWriteMakesItsRenameDurable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows opens no directory handle and NTFS journals the rename")
	}
	obLockDir(t)
	var events []string
	savedSync, savedReplace := obligationFsync, obligationReplace
	t.Cleanup(func() { obligationFsync, obligationReplace = savedSync, savedReplace })
	obligationFsync = func(f *os.File) error {
		st, err := f.Stat()
		if err == nil && st.IsDir() {
			events = append(events, "dir")
		} else {
			events = append(events, "file")
		}
		return f.Sync()
	}
	obligationReplace = func(src, dst string) error {
		events = append(events, "rename")
		return os.Rename(src, dst)
	}
	if !WriteHostObligation(context.Background(), obSHA, nil, "", "") {
		t.Fatal("arm failed")
	}
	if err := ReplaceBytes(HostObligationPath(), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "rename", "dir", "file", "rename", "dir"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v", events)
	}
}

func TestObligationUnreadableProfileRosterNeverAdmitsAPrivateRestartMarker(t *testing.T) {
	obSkipRoot(t)
	for _, unreadable := range []string{"roster", "profile"} {
		t.Run(unreadable, func(t *testing.T) {
			tmp, homes := obTwoProfiles(t)
			if err := os.WriteFile(filepath.Join(homes["writer"], "config.yaml"), []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", tmp)
			lockDir := filepath.Join(tmp, "gateway-locks")
			if err := os.MkdirAll(lockDir, 0o755); err != nil {
				t.Fatal(err)
			}
			blind, mode := filepath.Join(tmp, "profiles"), os.FileMode(0o300)
			if unreadable == "profile" {
				blind, mode = homes["writer"], 0
			}
			_ = os.Chmod(lockDir, 0o500)
			_ = os.Chmod(blind, mode)
			marker := filepath.Join(tmp, ProfileMarkerName)
			armed, err := ArmHostObligation(context.Background(), marker, obSHA, nil, "", "")
			_ = os.Chmod(blind, 0o700)
			_ = os.Chmod(lockDir, 0o700)
			if err != nil || armed {
				t.Fatalf("armed %v err %v", armed, err)
			}
			if st, err := os.Stat(marker); err != nil || !st.Mode().IsRegular() {
				t.Fatalf("the arming profile lost its marker: %v", err)
			}
		})
	}
}

func TestObligationUnwritableHostStateDirNeverHidesTheDebtFromAnotherProfile(t *testing.T) {
	obSkipRoot(t)
	tmp, homes := obTwoProfiles(t)
	for _, h := range homes {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", homes["coder"])
	lockDir := filepath.Join(tmp, "gateway-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(lockDir, 0o500)
	defer os.Chmod(lockDir, 0o700)
	runtimes := []any{pyjson.ObjectOf("kind", "gateway", "profile", "coder")}
	armed, err := ArmHostObligation(context.Background(), filepath.Join(homes["coder"], ProfileMarkerName), obSHA, runtimes, "coder", "")
	if err != nil || armed {
		t.Fatalf("a debt the writer profile cannot see was reported armed: %v %v", armed, err)
	}
}

func TestObligationAFailingMainPidProbeKeepsItsOwnRestart(t *testing.T) {
	boom := func(string) (int, error) { return 0, errors.New("systemctl exploded") }
	restart, covered := CollapseUnitsToHostProcesses([]string{"a.service", "b.service"}, boom)
	if !reflect.DeepEqual(restart, []string{"a.service", "b.service"}) || len(covered) != 0 {
		t.Fatalf("restart %v covered %v", restart, covered)
	}
}
