package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// The re-executed spawn test modes run only on POSIX hosts
func dispatchUnmanagedSpawnTestModes() (int, bool) { return 0, false }

func TestNativeWindowsDiscoveryUsesSameUserPipe(t *testing.T) {
	h, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := NewNativeControlServer(h, func(raw []byte, subject string) []byte {
		var request map[string]any
		if !strings.HasPrefix(subject, "sid:") || json.Unmarshal(raw, &request) != nil || request["verb"] != "identify" {
			return nil
		}
		body, _ := json.Marshal(map[string]any{"protocol": 1, "id": 1, "ok": true, "result": map[string]any{
			"runtime_protocol": 1, "state": "starting", "instance_id": "native-peer",
			"served_profiles": []any{map[string]any{"home": h, "profile_id": h}}}})
		return append(body, '\n')
	})
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if got := DiscoverGatewayEndpoint(context.Background(), h, 2e9).State; got != "starting" {
		t.Fatal(got)
	}
}

func TestNativeWindowsSpawnNeverRetriesWithoutBreakaway(t *testing.T) {
	var calls []*exec.Cmd
	swapSeam(t, &gatewayPopen, func(cmd *exec.Cmd) (SpawnedGateway, error) {
		calls = append(calls, cmd)
		return nil, &os.PathError{Op: "CreateProcess", Path: cmd.Path, Err: os.ErrPermission}
	})
	_, err := SpawnUnmanagedGateway(context.Background(), t.TempDir(), pythread.Monotonic()+5, false, false)
	var start *RuntimeStartError
	if !errors.As(err, &start) || start.Reason != "windows_breakaway_unavailable" {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].SysProcAttr.CreationFlags != home.DetachAttr(true).CreationFlags {
		t.Fatal(len(calls))
	}
}
