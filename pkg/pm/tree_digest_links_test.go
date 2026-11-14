package pm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// Points a junction at target through cmd mklink, failing the test when cmd refuses
func tdlMklinkJunction(t *testing.T, junction, target string) {
	t.Helper()
	command := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	out, err := exec.Command(command, "/d", "/c", "mklink", "/J", junction, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink failed: %v %s", err, out)
	}
}

func TestDigestWithoutPathlibJunctionAPI(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	payload := filepath.Join(root, "lib", "payload")
	writeFile(t, payload, []byte("before"), 0o666)
	first := stDigest(t, root)
	if stDigest(t, root) != first {
		t.Fatal("digest is not deterministic")
	}
	writeFile(t, payload, []byte("after"), 0o666)
	if stDigest(t, root) == first {
		t.Fatal("nested payload change kept the digest")
	}
}

func TestDirectoryLinksBindOnlyTheirTargetText(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "tree")
	writeFile(t, filepath.Join(root, "payload"), []byte("unchanged"), 0o666)
	left, right := filepath.Join(tmp, "left"), filepath.Join(tmp, "right")
	for _, target := range []string{left, right} {
		writeFile(t, filepath.Join(target, "content"), []byte("same bytes"), 0o666)
	}
	plain := stDigest(t, root)
	link := filepath.Join(root, "directory")
	if err := os.Symlink(filepath.Join("..", "left"), link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
			t.Skip("directory symlinks require Windows Developer Mode or privilege")
		}
		t.Fatal(err)
	}
	first := stDigest(t, root)
	if first == plain {
		t.Fatal("link text did not enter the digest")
	}
	writeFile(t, filepath.Join(left, "content"), []byte("changed outside the tree"), 0o666)
	if stDigest(t, root) != first {
		t.Fatal("digest followed the directory link")
	}
	os.Remove(link)
	if err := os.Symlink(filepath.Join("..", "right"), link); err != nil {
		t.Fatal(err)
	}
	if stDigest(t, root) == first {
		t.Fatal("retargeted link kept the digest")
	}
	os.Remove(link)
	if err := os.Symlink(".", link); err != nil {
		t.Fatal(err)
	}
	cycle := stDigest(t, root)
	if stDigest(t, root) != cycle {
		t.Fatal("cyclic link digest is not deterministic")
	}
	if cycle == plain || cycle == first {
		t.Fatal("cyclic link digest collides")
	}
}

func TestJunctionsBindTargetTextWithoutWalkingOutside(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "tree")
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatal(err)
	}
	left, right := filepath.Join(tmp, "left"), filepath.Join(tmp, "right")
	for _, target := range []string{left, right} {
		writeFile(t, filepath.Join(target, "content"), []byte("same bytes"), 0o666)
	}
	junction := filepath.Join(root, "junction")
	tdlMklinkJunction(t, junction, left)
	func() {
		defer os.Remove(junction)
		first := stDigest(t, root)
		writeFile(t, filepath.Join(left, "content"), []byte("changed outside the tree"), 0o666)
		if stDigest(t, root) != first {
			t.Fatal("digest walked through the junction")
		}
		if err := os.Remove(junction); err != nil {
			t.Fatal(err)
		}
		tdlMklinkJunction(t, junction, right)
		if stDigest(t, root) == first {
			t.Fatal("retargeted junction kept the digest")
		}
	}()
	if string(readFile(t, filepath.Join(left, "content"))) != "changed outside the tree" {
		t.Fatal("left content changed")
	}
	if string(readFile(t, filepath.Join(right, "content"))) != "same bytes" {
		t.Fatal("right content changed")
	}
}
