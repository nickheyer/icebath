package pm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNodeLibatomicAutoRepairNeverReadsATTY(t *testing.T) {
	isolateHome(t)
	savedAttempt, savedRoot, savedCommand, savedWhich, savedRun := libatomicAttempt, isRoot, hostInstallCommand, libatomicWhich, libatomicRun
	t.Cleanup(func() {
		libatomicAttempt, isRoot, hostInstallCommand, libatomicWhich, libatomicRun = savedAttempt, savedRoot, savedCommand, savedWhich, savedRun
	})
	libatomicAttempt = nil
	isRoot = func() bool { return false }
	hostInstallCommand = func() []string { return []string{"dnf", "install", "-y", "libatomic"} }
	libatomicWhich = func(name string) string {
		if name == "sudo" {
			return "/usr/bin/sudo"
		}
		return ""
	}
	var captured *exec.Cmd
	libatomicRun = func(cmd *exec.Cmd) error {
		captured = cmd
		return errors.New("exit status 1")
	}
	attempted, _ := TryInstallLibatomic(context.Background())
	if attempted {
		t.Fatal("a failed install reported success")
	}
	if captured == nil || len(captured.Args) < 2 || !reflect.DeepEqual(captured.Args[:2], []string{"/usr/bin/sudo", "-n"}) {
		t.Fatalf("argv %v", captured)
	}
	if captured.Stdin != nil {
		t.Fatalf("stdin %v, want the null device", captured.Stdin)
	}
}

// The os-release file parses like platform freedesktop_os_release, a directory candidate skipped like an unreadable file
func TestNodeLibatomicOSReleaseParsesLikePython(t *testing.T) {
	tmp := isolateHome(t)
	dir := filepath.Join(tmp, "dir-candidate")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tmp, "os-release")
	writeFile(t, file, []byte("NAME=\"Fedora Linux\"\rID=fedora\nID_LIKE=\"rhel centos\"\r\nVERSION_ID=40\nPRETTY_NAME='Fedora \\\"x\\\"'\nBAD LINE\nQ=\"unterminated\nE=a\\$b\\`c\n"), 0o666)
	saved := osReleaseFiles
	t.Cleanup(func() { osReleaseFiles = saved })
	osReleaseFiles = []string{dir, file}
	info, err := freedesktopOSRelease()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"NAME": "Fedora Linux", "ID": "fedora", "PRETTY_NAME": `Fedora "x"`, "ID_LIKE": "rhel centos",
		"VERSION_ID": "40", "Q": `"unterminated`, "E": "a$b`c"}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("os-release %v", info)
	}
	if order := managerOrder(releaseTokens()); !reflect.DeepEqual(order, []string{"dnf", "yum"}) {
		t.Fatalf("manager order %v", order)
	}
	osReleaseFiles = []string{dir, filepath.Join(tmp, "missing")}
	if _, err := freedesktopOSRelease(); err == nil {
		t.Fatal("no readable candidate parsed")
	}
	if order := managerOrder(releaseTokens()); !reflect.DeepEqual(order, []string{"dnf", "yum", "apt-get", "zypper", "pacman", "apk"}) {
		t.Fatalf("unknown family order %v", order)
	}
}
