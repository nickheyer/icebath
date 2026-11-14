package pm

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMuslUserlandResolvesTheNativeMuslTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	fakeSh := filepath.Join(t.TempDir(), "sh")
	writeFile(t, fakeSh, append(append([]byte("\x7fELF"), bytes.Repeat([]byte{0}, 60)...), []byte("/lib/ld-musl-x86_64.so.1\x00")...), 0o666)
	savedMusl, savedMachine, savedBionic := nativeLinuxUsesMusl, nativeMachine, isBionicLibc
	nativeLinuxUsesMusl = func() (bool, bool) { return elfLoaderIsMusl(fakeSh) }
	nativeMachine = func() string { return "x86_64" }
	isBionicLibc = func() bool { return false }
	t.Cleanup(func() { nativeLinuxUsesMusl, nativeMachine, isBionicLibc = savedMusl, savedMachine, savedBionic })
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target != "linux-x64-musl" {
		t.Fatalf("target = %q", target)
	}
}
