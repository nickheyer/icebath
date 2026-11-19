package updaterecord

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestExitCodeFleetWriterNeverExposesAnEmptyExitCode(t *testing.T) {
	ctx, _ := rcHome(t)
	marker := filepath.Join(home.Dir(ctx), ".update_exit_code")
	var stop atomic.Bool
	var wg sync.WaitGroup
	var seen []string
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if data, err := os.ReadFile(marker); err == nil {
				seen = append(seen, string(data))
			}
		}
	}()
	for i := range 200 {
		WriteGatewayUpdateExitCode(ctx, i%2 == 0)
	}
	WriteGatewayUpdateExitCode(ctx, true)
	stop.Store(true)
	wg.Wait()
	for _, text := range seen {
		if text == "" {
			t.Fatalf("a poll read an empty exit code among %d reads", len(seen))
		}
	}
	if data, _ := os.ReadFile(marker); string(data) != "0" {
		t.Fatalf("final exit code %q", data)
	}
	entries, _ := os.ReadDir(home.Dir(ctx))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".publish-") {
			t.Fatalf("a publish temp file was left behind %s", e.Name())
		}
	}
}

func TestExitCodeCommittedSuccessIsNeverReportedFailed(t *testing.T) {
	ctx, _ := rcHome(t)
	WriteGatewayUpdateExitCode(ctx, false)
	if data, err := os.ReadFile(GatewayExitCodePath(home.Dir(ctx))); err != nil || string(data) != "1" {
		t.Fatalf("an open failure %q %v", data, err)
	}
	BeginUpdateReceipt(ctx, nil, "")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	WriteGatewayUpdateExitCode(ctx, false)
	if data, err := os.ReadFile(GatewayExitCodePath(home.Dir(ctx))); err != nil || string(data) != "0" {
		t.Fatalf("a committed success read failed %q %v", data, err)
	}
}
