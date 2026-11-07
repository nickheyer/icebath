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
	if n, err := ReadGatewayExitCode(home.Dir(ctx)); err != nil || n.Int64() != 1 {
		t.Fatalf("an open failure %v %v", n, err)
	}
	BeginUpdateReceipt(ctx, nil, "")
	FinalizeUpdateReceipt(ctx, "success", nil, "")
	WriteGatewayUpdateExitCode(ctx, false)
	if n, err := ReadGatewayExitCode(home.Dir(ctx)); err != nil || n.Int64() != 0 {
		t.Fatalf("a committed success read failed %v %v", n, err)
	}
}

func TestExitCodeReaderParsesLikeTheGatewayWatcher(t *testing.T) {
	h := t.TempDir()
	for text, want := range map[string]int64{"0\n": 0, "": 1, " \t\n": 1, "\xef\xbb\xbf3": 3, "1_0": 10, "-2 ": -2, "+7": 7} {
		os.WriteFile(GatewayExitCodePath(h), []byte(text), 0o644)
		if n, err := ReadGatewayExitCode(h); err != nil || n.Int64() != want {
			t.Fatalf("%q read %v %v", text, n, err)
		}
	}
	os.WriteFile(GatewayExitCodePath(h), []byte("done"), 0o644)
	if _, err := ReadGatewayExitCode(h); err == nil || err.Error() != "invalid literal for int() with base 10: 'done'" {
		t.Fatalf("bad text %v", err)
	}
	os.WriteFile(GatewayExitCodePath(h), []byte{0xff}, 0o644)
	if _, err := ReadGatewayExitCode(h); err == nil {
		t.Fatal("undecodable text read as a code")
	}
	os.Remove(GatewayExitCodePath(h))
	if _, err := ReadGatewayExitCode(h); !os.IsNotExist(err) {
		t.Fatalf("missing marker %v", err)
	}
}
