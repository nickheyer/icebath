package connections

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopSSHPathOverrideReadsDesktopSshPathFromConfigYaml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	eq(t, DesktopSSHPathOverride(home), "")
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("model: x\ndesktop:\n  ssh_path: \"D:\\\\tools\\\\ssh.exe\"\n"), 0o600)
	eq(t, DesktopSSHPathOverride(home), `D:\tools\ssh.exe`)
}

// Acts as the ssh client when the test binary is spawned with ICEBATH_FAKE_SSH set
func fakeSSH(mode string, args []string) int {
	if len(args) != 3 || args[0] != "-G" || args[1] != "--" {
		return 9
	}
	if mode == "fail" {
		os.Stderr.WriteString("ssh: Could not resolve hostname " + args[2] + "\n")
		return 255
	}
	os.Stdout.WriteString("host " + args[2] + "\nhostname 10.0.0.5\nuser alice\nport 2222\n")
	return 0
}

func TestResolveSSHHostParsesSshGOutputAndReportsFailures(t *testing.T) {
	self, err := os.Executable()
	eq(t, err, nil)
	t.Setenv("ICEBATH_FAKE_SSH", "ok")
	got, err := ResolveSSHHost(self, " devbox ")
	eq(t, err, nil)
	eq(t, *got.Hostname, "10.0.0.5")
	eq(t, *got.User, "alice")
	eq(t, *got.Port, 2222)
	t.Setenv("ICEBATH_FAKE_SSH", "fail")
	_, err = ResolveSSHHost(self, "nope")
	eq(t, err.Error(), "ssh: Could not resolve hostname nope")
	_, err = ResolveSSHHost(self, "  ")
	eq(t, err.Error(), "SSH host is required.")
}
