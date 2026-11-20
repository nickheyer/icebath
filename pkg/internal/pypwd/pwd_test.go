package pypwd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDatabasesParseLikeNssFiles(t *testing.T) {
	dir := t.TempDir()
	passwdPath = filepath.Join(dir, "passwd")
	groupPath = filepath.Join(dir, "group")
	t.Cleanup(func() { passwdPath, groupPath = "/etc/passwd", "/etc/group" })
	os.WriteFile(passwdPath, []byte("# comment\nroot:x:0:0:root:/root:/bin/sh\nbad:x:z:0::/:/bin/sh\n+nis\nnick:x:1000:1000:Nick:/home/nick:/bin/zsh\nshort:x:1\n"), 0o644)
	os.WriteFile(groupPath, []byte("root:x:0:\nwheel:x:10:root,nick\n"), 0o644)
	all, err := Getpwall()
	if err != nil || len(all) != 2 || all[1].Name != "nick" || all[1].GID != 1000 || all[1].Shell != "/bin/zsh" {
		t.Fatalf("%+v %v", all, err)
	}
	if p, err := Getpwuid(0); err != nil || p.Name != "root" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Getpwuid(7); err == nil || err.Error() != "getpwuid(): uid not found: 7" {
		t.Fatal(err)
	}
	if p, err := Getpwnam("nick"); err != nil || p.UID != 1000 || p.Dir != "/home/nick" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Getpwnam("ghost"); err == nil || err.Error() != "getpwnam(): name not found: 'ghost'" {
		t.Fatal(err)
	}
	if g, err := Getgrgid(10); err != nil || g.Name != "wheel" || len(g.Mem) != 2 || g.Mem[1] != "nick" {
		t.Fatalf("%+v %v", g, err)
	}
	if g, err := Getgrgid(0); err != nil || len(g.Mem) != 0 {
		t.Fatalf("%+v %v", g, err)
	}
	if _, err := Getgrgid(5); err == nil || err.Error() != "getgrgid(): gid not found: 5" {
		t.Fatal(err)
	}
}
