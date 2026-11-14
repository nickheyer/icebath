package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fedoraBashrc = `# .bashrc

# Source global definitions
if [ -f /etc/bashrc ]; then
    . /etc/bashrc
fi

# User specific environment
if ! [[ "$PATH" =~ "$HOME/.local/bin:$HOME/bin:" ]]; then
    PATH="$HOME/.local/bin:$HOME/bin:$PATH"
fi
export PATH
`

const fedoraBashProfile = `# .bash_profile

# Get the aliases and functions
if [ -f ~/.bashrc ]; then
    . ~/.bashrc
fi

# User specific environment and startup programs
`

const debianProfile = `# set PATH so it includes user's private bin if it exists
if [ -d "$HOME/.local/bin" ] ; then
    PATH="$HOME/.local/bin:$PATH"
fi
`

func shellHome(t *testing.T, files map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("posix shells")
	}
	h := t.TempDir()
	os.MkdirAll(filepath.Join(h, ".local", "bin"), 0o755)
	for name, text := range files {
		os.WriteFile(filepath.Join(h, name), []byte(text), 0o644)
	}
	return h
}

func wire(t *testing.T, h string, runs int) {
	t.Helper()
	for i := 0; i < runs; i++ {
		if err := WireShellPath(h, "/bin/bash", func(string) {}); err != nil {
			t.Fatal(err)
		}
	}
}

// PATH a login bash ends with after reading the first existing login startup file
func loginPath(t *testing.T, h string) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	cmd := exec.Command(bash, "--noprofile", "--norc", "-c", `for f in ~/.bash_profile ~/.bash_login ~/.profile; do if [ -r "$f" ]; then . "$f"; break; fi; done; printf %s "$PATH"`)
	cmd.Env = []string{"HOME=" + h, "PATH=/usr/local/bin:/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(out), ":")
}

func count(items []string, want string) int {
	n := 0
	for _, i := range items {
		if i == want {
			n++
		}
	}
	return n
}

func TestBareAssignmentCountsAsExistingSetup(t *testing.T) {
	for name, text := range map[string]string{".bashrc": fedoraBashrc, ".profile": debianProfile} {
		t.Run(name, func(t *testing.T) {
			h := shellHome(t, map[string]string{name: text})
			wire(t, h, 1)
			if data, _ := os.ReadFile(filepath.Join(h, name)); string(data) != text {
				t.Fatalf("%s rewritten:\n%s", name, data)
			}
		})
	}
}

func TestFedoraLoginShellHasLocalBinOnce(t *testing.T) {
	h := shellHome(t, map[string]string{".bashrc": fedoraBashrc, ".bash_profile": fedoraBashProfile})
	wire(t, h, 2)
	data, _ := os.ReadFile(filepath.Join(h, ".bash_profile"))
	if strings.Count(string(data), ShellPathMarker) != 1 {
		t.Fatalf(".bash_profile:\n%s", data)
	}
	if n := count(loginPath(t, h), filepath.Join(h, ".local", "bin")); n != 1 {
		t.Fatalf("user bin on PATH %d times", n)
	}
}

func TestBashLoginOnlyUserGetsLocalBin(t *testing.T) {
	h := shellHome(t, map[string]string{".bash_login": "# mine\n"})
	wire(t, h, 1)
	if count(loginPath(t, h), filepath.Join(h, ".local", "bin")) == 0 {
		t.Fatal("a .bash_login user must get the user bin")
	}
}

func TestUnwireShellPathRemovesOnlyTheInstallersLines(t *testing.T) {
	h := shellHome(t, map[string]string{".bashrc": "# mine\n"})
	wire(t, h, 1)
	changed, err := UnwireShellPath(h)
	if err != nil || len(changed) == 0 {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if data, _ := os.ReadFile(filepath.Join(h, ".bashrc")); string(data) != "# mine\n" {
		t.Fatalf(".bashrc after unwire %q", data)
	}
}
