package pyshlex

import (
	"reflect"
	"testing"
)

func TestSplitCommandLineKeepsWindowsBackslashes(t *testing.T) {
	got, err := splitCommandLineFor(`"C:\Program Files\x.exe" C:\Users\me\out.txt 'a b'`, true)
	if err != nil || !reflect.DeepEqual(got, []string{`C:\Program Files\x.exe`, `C:\Users\me\out.txt`, "a b"}) {
		t.Fatalf("windows split = %q, %v", got, err)
	}
	got, err = splitCommandLineFor(`bash "a b" c\ d`, false)
	if err != nil || !reflect.DeepEqual(got, []string{"bash", "a b", "c d"}) {
		t.Fatalf("posix split = %q, %v", got, err)
	}
	if _, err := splitCommandLineFor(`bash "unterminated`, false); err == nil {
		t.Fatal("unbalanced quotes split without an error")
	}
}
