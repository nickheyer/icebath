package pypath

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"testing"
)

func TestPathHomeAndExpanduserMatchPathlib(t *testing.T) {
	if windows {
		t.Setenv("USERPROFILE", `C:\Users\ice\`)
		t.Setenv("HOME", "")
		cases := map[string]string{"~": `C:\Users\ice`, `~\a\b`: `C:\Users\ice\a\b`, "~/a": `C:\Users\ice\a`, `C:\~x`: `C:\~x`, `a\~`: `a\~`}
		for in, want := range cases {
			if got, err := PathExpanduser(in); err != nil || got != want {
				t.Fatalf("PathExpanduser(%q) = %q, %v want %q", in, got, err, want)
			}
		}
		if got, err := PathHome(); err != nil || got != `C:\Users\ice` {
			t.Fatalf("PathHome() = %q, %v", got, err)
		}
		return
	}
	t.Setenv("HOME", "/tmp/ice//home/")
	cases := map[string]string{"~": "/tmp/ice/home", "~/a/./b/": "/tmp/ice/home/a/b", "/x/~y": "/x/~y", "a/~": "a/~", "": "."}
	for in, want := range cases {
		if got, err := PathExpanduser(in); err != nil || got != want {
			t.Fatalf("PathExpanduser(%q) = %q, %v want %q", in, got, err, want)
		}
	}
	if got, err := PathHome(); err != nil || got != "/tmp/ice/home" {
		t.Fatalf("PathHome() = %q, %v", got, err)
	}
	var rt *RuntimeError
	if _, err := PathExpanduser("~icebath-no-such-user-zq/a"); !errors.As(err, &rt) || rt.Error() != "Could not determine home directory." || rt.PyTypeName() != "RuntimeError" {
		t.Fatalf("unknown user error = %v", err)
	}
	t.Setenv("HOME", "")
	if got, err := PathHome(); err != nil || got != "/" {
		t.Fatalf("empty HOME PathHome() = %q, %v", got, err)
	}
	os.Unsetenv("HOME")
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		if _, err := PathHome(); !errors.As(err, &rt) {
			t.Fatalf("no HOME and no passwd entry gave %v", err)
		}
		return
	}
	if got, err := PathHome(); err != nil || got != PathStr(u.HomeDir) {
		t.Fatalf("unset HOME PathHome() = %q, %v want %q", got, err, u.HomeDir)
	}
}
