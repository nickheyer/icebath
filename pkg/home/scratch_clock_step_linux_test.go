package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

func TestPruneKeepsARegisteredGatewaysShellAfterAClockStep(t *testing.T) {
	for _, step := range []float64{5, -5} {
		t.Run(strconv.FormatFloat(step, 'g', -1, 64), func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			scratch := testScratch(t, h)
			lane := filepath.Join(scratch, "gateway-lane")
			if err := os.Mkdir(lane, 0o755); err != nil {
				t.Fatal(err)
			}
			pidfile := filepath.Join(h, "shell.pid")
			gateway := testChild("gateway", lane, pidfile)
			if err := gateway.Start(); err != nil {
				t.Fatal(err)
			}
			var shell int
			defer func() {
				_ = proc.KillPID(shell)
				_ = gateway.Process.Kill()
				_ = gateway.Wait()
			}()
			waitUntil(t, 20*time.Second, "registered gateway started its shell", func() bool {
				data, err := os.ReadFile(pidfile)
				shell, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				return err == nil && shell > 0
			})
			birth, err := proc.Birth(gateway.Process.Pid)
			boot, ok := proc.BootStart(gateway.Process.Pid)
			if err != nil || !ok || liveBootID() == "" {
				t.Fatalf("gateway identity unreadable: %v %v", err, ok)
			}
			writeLedger(t, filepath.Join(RootOf(h), "spawn-ledger.json"), fmt.Sprintf(
				`{"pid": %d, "create_time": %s, "boot_id": %q, "boot_start": %s, "purpose": "gateway"}`,
				gateway.Process.Pid, strconv.FormatFloat(birth+step, 'g', -1, 64), liveBootID(), strconv.FormatFloat(boot, 'g', -1, 64)))
			age(t, 30, lane)
			mustPrune(t, scratch, 0)
			if goneOrZombie(shell) {
				t.Fatal("the gateway's shell was killed")
			}
			if _, err := os.Stat(lane); err != nil {
				t.Fatal("the entry the gateway's shell works in was deleted")
			}
		})
	}
}
