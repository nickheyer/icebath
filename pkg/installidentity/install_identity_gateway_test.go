package installidentity_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/installidentity"
)

func TestIndependentFirstCallersReturnTheSingleCommittedIdentity(t *testing.T) {
	root := t.TempDir()
	held := filepath.Join(t.TempDir(), "writer")
	winner, winnerOut, err := installidentity.StartRace(root, strings.Repeat("a", 32), "", held)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(held + ".entered"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never entered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	loser, loserOut, err := installidentity.StartRace(root, strings.Repeat("b", 32), "", "")
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- loser.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("loser finished while the writer held the lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := os.WriteFile(held+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := winner.Wait(); err != nil {
		t.Fatalf("winner exit %v", err)
	}
	if err := <-exited; err != nil {
		t.Fatalf("loser exit %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "install_id"))
	if err != nil {
		t.Fatal(err)
	}
	persisted := strings.TrimSpace(string(raw))
	if winnerOut.String() != persisted || loserOut.String() != persisted {
		t.Fatalf("returned %q %q persisted %q", winnerOut, loserOut, persisted)
	}
	t.Setenv("ICEBATH_HOME", root)
	saved := *installidentity.ProcessCache
	*installidentity.ProcessCache = installidentity.Cache{}
	t.Cleanup(func() { *installidentity.ProcessCache = saved })
	got, err := gateway.LocalAuthorityGatewayID(context.Background())
	if err != nil || got != "install:"+persisted {
		t.Fatalf("authority %q %v", got, err)
	}
}
