package selfupdate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Starts a sleeping process whose command line reads as a gateway run on a home, with its argv
func wgSpawnLookalike(t *testing.T, entry, served string) *exec.Cmd {
	t.Helper()
	child := exec.Command("/bin/sh", entry, "gateway", "run")
	child.Env = append(os.Environ(), "ICEBATH_HOME="+served)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if argv, err := winCmdline(child.Process.Pid); err == nil && len(argv) == 4 {
			return child
		}
		if time.Now().After(deadline) {
			t.Fatal("the spawned gateway never published its argv")
		}
		time.Sleep(time.Millisecond)
	}
}

func wgLookalikeEntry(t *testing.T) (string, []any) {
	t.Helper()
	entry := filepath.Join(t.TempDir(), "bin", "icebath")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("sleep 60\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return entry, []any{"/bin/sh", entry, "gateway", "run"}
}

func TestOneGatewayNeverRetiresTwoUnmappedDebts(t *testing.T) {
	served := t.TempDir()
	t.Setenv("ICEBATH_HOME", served)
	savedExists, savedAttest, savedBudget, savedFind := winPidExists, winWriteAttestation, relaunchVerifyTimeoutS, winFindGatewayPids
	t.Cleanup(func() {
		winPidExists, winWriteAttestation, relaunchVerifyTimeoutS, winFindGatewayPids = savedExists, savedAttest, savedBudget, savedFind
	})
	winPidExists = func(int) bool { return false }
	winWriteAttestation = func(context.Context, []int, string) {}
	relaunchVerifyTimeoutS = 3.0
	entry, argv := wgLookalikeEntry(t)
	debts := []any{map[string]any{"pid": 40011, "argv": argv}, map[string]any{"pid": 40012, "argv": argv}}
	child := wgSpawnLookalike(t, entry, served)
	winFindGatewayPids = func(context.Context) ([]int, error) { return []int{child.Process.Pid}, nil }
	token := map[string]any{"profiles": map[string]any{}, "unmapped": append([]any{}, debts...)}
	var out bytes.Buffer
	err := verifyRelaunchedGatewaysAlive(context.Background(), &out, token, map[string]any{}, debts)
	if err == nil || !strings.Contains(err.Error(), "pid 40012") {
		t.Fatalf("err %v output %q", err, out.String())
	}
	if !reflect.DeepEqual(token["unmapped"], []any{debts[1]}) {
		t.Fatalf("one relaunched gateway retired both debts: %v", token["unmapped"])
	}
}

func TestAnEqualArgvGatewayOfAnotherHomeNeverRetiresTheUnmappedDebt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	homeA, homeB := filepath.Join(root, "profiles", "a"), filepath.Join(root, "profiles", "b")
	for _, served := range []string{homeA, homeB} {
		if err := os.MkdirAll(served, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entry, argv := wgLookalikeEntry(t)
	old := wgSpawnLookalike(t, entry, homeA)
	debt := map[string]any{"pid": old.Process.Pid, "argv": argv, "home": homeA, "ct": gateway.GetProcessStartTime(old.Process.Pid)}
	_ = old.Process.Kill()
	_ = old.Wait()
	time.Sleep(50 * time.Millisecond)
	sibling := wgSpawnLookalike(t, entry, homeB)
	ctx := context.Background()
	got, err := unmappedReadyFilter(ctx, debt, map[int]bool{})([]int{sibling.Process.Pid})
	if err != nil || len(got) != 0 {
		t.Fatalf("home B's gateway with A's argv retired A's restart debt: %v %v", got, err)
	}
	replacement := wgSpawnLookalike(t, entry, homeA)
	got, err = unmappedReadyFilter(ctx, debt, map[int]bool{})([]int{sibling.Process.Pid, replacement.Process.Pid})
	if err != nil || !reflect.DeepEqual(got, []int{replacement.Process.Pid}) {
		t.Fatalf("got %v err %v", got, err)
	}
}
