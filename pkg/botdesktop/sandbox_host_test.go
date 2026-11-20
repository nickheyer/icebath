package botdesktop

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type cachedUserEnv struct{ tools.Environment }

func TestUserForReadsTheCachedDesktopUser(t *testing.T) {
	e := &cachedUserEnv{}
	user := DesktopUser
	factsOf(e).user = &user
	if got, err := UserFor(e); err != nil || got != DesktopUser {
		t.Fatalf("got %q %v", got, err)
	}
}
