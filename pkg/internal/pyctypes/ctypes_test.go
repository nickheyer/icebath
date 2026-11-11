package pyctypes

import (
	"os"
	"testing"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

func TestStringAndPointerHelpers(t *testing.T) {
	c := CString("héllo")
	if len(c) != len("héllo")+1 || c[len(c)-1] != 0 {
		t.Fatalf("C string %v", c)
	}
	if got := GoString(uintptr(unsafe.Pointer(&c[0]))); got != "héllo" {
		t.Fatalf("round trip %q", got)
	}
	if GoString(0) != "" {
		t.Fatal("null pointer must read as empty")
	}
	empty := CString("")
	if got := GoString(uintptr(unsafe.Pointer(&empty[0]))); got != "" {
		t.Fatalf("empty string read as %q", got)
	}
	second := CString("two")
	array := []uintptr{uintptr(unsafe.Pointer(&c[0])), uintptr(unsafe.Pointer(&second[0]))}
	base := uintptr(unsafe.Pointer(&array[0]))
	if GoString(PointerAt(base, 0)) != "héllo" || GoString(PointerAt(base, 1)) != "two" {
		t.Fatal("pointer array read back wrong")
	}
	closeDir, err := AddDLLDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	closeDir()
}
