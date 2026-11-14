package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestRuntimeRecordStampedForAnotherHomeIsNotThisHomesGateway(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", root)
	const livePID = 4242
	patch(t, &statusPidExists, func(pid int) bool { return pid == livePID })
	patch(t, &statusProcessStartTime, fixedInt(int64(1000)))
	patch(t, &statusReadProcessCmdline, cmdline("python -m icebath_cli.main -p eagle gateway run"))
	patch(t, &statusMultiplexerLivenessForProfile, func(string) (int, *pyjson.Object, bool, error) { return 0, nil, false, nil })
	copied := filepath.Join(tmp, "sandbox-root", "profiles", "eagle")
	if err := os.MkdirAll(copied, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "profiles", "eagle")
	foreign := rec("pid", int64(livePID), "kind", "icebath-gateway", "gateway_state", "running", "start_time", int64(1000),
		"argv", strs("python", "-m", "icebath_cli.main", "-p", "eagle", "gateway", "run"), "hermes_home", original)
	writeRecord(t, filepath.Join(copied, "gateway_state.json"), foreign)
	if pid, ok, err := GetRuntimeStatusRunningPid(foreign, copied); err != nil || ok {
		t.Fatalf("copied home owns %d %v %v", pid, ok, err)
	}
	if got := ResolveGatewayLiveness(LivenessOptions{ProfileDir: copied, NoCache: true}); got.Running {
		t.Fatalf("copied home running %+v", got)
	}
	if pid, ok, err := GetRuntimeStatusRunningPid(foreign, original); err != nil || !ok || pid != livePID {
		t.Fatalf("original home %d %v %v", pid, ok, err)
	}
}
