package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEnvLineDefinesKeyMatchesWhatLoadEnvAccepts(t *testing.T) {
	const pat = "ghp_oldtoken0000000000000000000000000000"
	cases := []struct {
		line string
		want bool
	}{
		{"GITHUB_TOKEN=" + pat, true},
		{"export GITHUB_TOKEN=" + pat, true},
		{"GITHUB_TOKEN = " + pat, true},
		{"GITHUB_TOKEN\t=\t" + pat, true},
		{"export GITHUB_TOKEN = " + pat, true},
		{"# GITHUB_TOKEN=" + pat, false},
		{"GITHUB_TOKEN_EXTRA=" + pat, false},
		{"MY_GITHUB_TOKEN=" + pat, false},
	}
	for _, c := range cases {
		if got := EnvLineDefinesKey(c.line, "GITHUB_TOKEN"); got != c.want {
			t.Errorf("EnvLineDefinesKey(%q) = %v, want %v", c.line, got, c.want)
		}
		if _, loaded := ParseEnv(c.line + "\n")["GITHUB_TOKEN"]; loaded != c.want {
			t.Errorf("writer and loader disagree on %q", c.line)
		}
	}
	for _, line := range []string{"\x1cexport GITHUB_TOKEN=1", "GITHUB_TOKEN\x1f=1", "export \x0bGITHUB_TOKEN=1\x1e"} {
		if !EnvLineDefinesKey(line, "GITHUB_TOKEN") {
			t.Errorf("str.strip whitespace around %q must still define the key", line)
		}
	}
	if EnvLineDefinesKey("GITHUB_TOKEN", "GITHUB_TOKEN") {
		t.Error("a line without = assigns nothing")
	}
}

func TestEnvLineDefinesKeyFoldsCaseUnderTheWindowsPolicy(t *testing.T) {
	restore := envPolicyWindows
	t.Cleanup(func() { envPolicyWindows = restore })
	for _, prefix := range []string{"", "export "} {
		line := prefix + "Path=C:\\Windows\\System32\n"
		envPolicyWindows = true
		if !EnvLineDefinesKey(line, "PATH") {
			t.Errorf("windows policy must match %q", line)
		}
		envPolicyWindows = false
		if EnvLineDefinesKey(line, "PATH") {
			t.Errorf("posix policy must not match %q", line)
		}
	}
}

func TestReadEnvLinesNormalizesLikeTheUpstreamReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	raw := "\xef\xbb\xbf  A=1  \r\n  # c  \r\n\r\nB=\xff2\rC=3"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnvLines(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=1\n", "  # c  \n", "\n", "B=\ufffd2\n", "C=3\n"}
	if !slices.Equal(got, want) {
		t.Fatalf("ReadEnvLines = %q, want %q", got, want)
	}
	empty := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadEnvLines(empty); err != nil || len(got) != 0 {
		t.Fatalf("empty file = %q, %v", got, err)
	}
	if _, err := ReadEnvLines(filepath.Join(dir, "missing.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file error = %v, want FileNotFoundError", err)
	}
}

func TestWriteEnvLinesKeepsModeAndSymlinkTarget(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_MANAGED", "")
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("OLD=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvLines(context.Background(), path, []string{"  X=1", "Y=2\n"}, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "  X=1Y=2\n" {
		t.Fatalf("lines must be written verbatim, got %q", b)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("preserved mode = %o, want 640", info.Mode().Perm())
	}
	if err := WriteEnvLines(context.Background(), path, []string{"Z=3\n"}, false); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); !containerOrChmodSkipped() && ManagedSystem(context.Background(), "") == "" && info.Mode().Perm() != 0o600 {
		t.Fatalf("secured mode = %o, want 600", info.Mode().Perm())
	}

	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "target.env")
	if err := os.WriteFile(target, []byte("A=1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvLines(context.Background(), link, []string{"B=2\n"}, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink must survive the write, got %v %v", info, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "B=2\n" {
		t.Fatalf("symlink target content = %q", b)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Fatalf("symlink target mode = %o, want 640", info.Mode().Perm())
	}
	for _, d := range []string{dir, realDir} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Fatalf("staged temp %s left in %s", e.Name(), d)
			}
		}
	}

	if err := WriteEnvLines(context.Background(), filepath.Join(dir, "absent", ".env"), []string{"A=1\n"}, true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent error = %v, want FileNotFoundError", err)
	}
}
