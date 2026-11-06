package pyfs

import "testing"

func TestFilemodeMatchesCPython(t *testing.T) {
	for mode, want := range map[uint32]string{
		0o100644: "-rw-r--r--",
		0o040700: "drwx------",
		0o140755: "srwxr-xr-x",
		0o120777: "lrwxrwxrwx",
		0o010600: "prw-------",
		0o104755: "-rwsr-xr-x",
		0o102644: "-rw-r-Sr--",
		0o041777: "drwxrwxrwt",
		0o041776: "drwxrwxrwT",
		0o000000: "?---------",
	} {
		if got := Filemode(mode); got != want {
			t.Errorf("%o: %q want %q", mode, got, want)
		}
	}
}
