//go:build unix

package pyplatform

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestPlatformEndsWithTheLibcOfTheProcess(t *testing.T) {
	label := Platform()
	if runtime.GOOS != "linux" {
		if strings.Contains(label, "-with") {
			t.Fatalf("label %q names a libc outside Linux", label)
		}
		return
	}
	lib, ver, ok := confstrLibcVer()
	if !ok {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		lib, ver = libcVer(executable)
	}
	if want := joinPlatform("with", lib+ver); !strings.HasSuffix(label, "-"+want) {
		t.Fatalf("label %q does not end with %q", label, want)
	}
}
