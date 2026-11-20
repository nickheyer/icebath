package proc

import (
	"os"
	"os/user"
	"runtime"
	"strings"
	"testing"
)

func TestUsernameOfSelfMatchesCurrentUser(t *testing.T) {
	got, err := Username(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(got, u.Username) {
			t.Fatalf("got %q want %q", got, u.Username)
		}
		return
	}
	if got != u.Username {
		t.Fatalf("got %q want %q", got, u.Username)
	}
}
