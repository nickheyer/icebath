//go:build !windows

package pypwd

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/homeguard"
)

func TestSystemEntryOfThisProcessCarriesThePrivateTestHome(t *testing.T) {
	p, err := Getpwuid(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != homeguard.Home() || p.Dir == "" {
		t.Fatalf("passwd home %q, private test home %q", p.Dir, homeguard.Home())
	}
}
