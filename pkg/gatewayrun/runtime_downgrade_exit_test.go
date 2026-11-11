package gatewayrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Duck typed runner of the downgrade cases, counting stops and holding the exit it was given
type downgradeProbe struct {
	running  bool
	stopped  int
	exitCode *int
	reason   string
	stopTask *gatewayTask
	stopErr  error
}

func (p *downgradeProbe) downgradeRunning() bool { return p.running }

func (p *downgradeProbe) downgradeExit(reason string, code int) { p.reason, p.exitCode = reason, &code }

func (p *downgradeProbe) downgradeStop(context.Context) error {
	if p.stopErr != nil {
		return p.stopErr
	}
	p.stopped++
	p.running = false
	return nil
}

func (p *downgradeProbe) keepDowngradeStop(task *gatewayTask) { p.stopTask = task }

var downgradeSkew = &codeSkew{"aaaaaaaaaa", "bbbbbbbbbb"}

// Runs the watcher over a scripted skew sequence and lets the detached stop finish
func watchDowngrade(t *testing.T, skews []*codeSkew, update bool, predates func() bool) *downgradeProbe {
	t.Helper()
	runner := &downgradeProbe{running: true}
	i := 0
	poll, maxPolls := 0.0, len(skews)+1
	err := runtimeDowngradeWatcher(context.Background(), runner, downgradeWatch{
		pollS: &poll, maxPolls: &maxPolls, predates: predates,
		skew: func() *codeSkew {
			if i >= len(skews) {
				return nil
			}
			i++
			return skews[i-1]
		},
		updateProbe: func() (bool, error) { return update, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.stopTask != nil {
		runner.stopTask.wait(context.Background())
	}
	return runner
}

func TestAConfirmedMoveToAPreRuntimeTreeStopsWithoutARestart(t *testing.T) {
	yes := func() bool { return true }
	runner := watchDowngrade(t, []*codeSkew{downgradeSkew, downgradeSkew}, false, yes)
	if runner.stopped != 1 {
		t.Fatal(runner.stopped)
	}
	if runner.exitCode == nil || *runner.exitCode != gateway.GatewayServiceRestartExitCode {
		t.Fatal(runner.exitCode)
	}
	if !strings.Contains(runner.reason, "predates the gateway runtime") {
		t.Fatal(runner.reason)
	}
	if watchDowngrade(t, []*codeSkew{downgradeSkew, nil, downgradeSkew, nil}, false, yes).stopped != 0 {
		t.Fatal("a single blip must keep it up")
	}
	if watchDowngrade(t, []*codeSkew{downgradeSkew, downgradeSkew, downgradeSkew}, true, yes).stopped != 0 {
		t.Fatal("a live update must keep it up")
	}
	if watchDowngrade(t, []*codeSkew{downgradeSkew, downgradeSkew}, false, func() bool { return false }).stopped != 0 {
		t.Fatal("code that still has the runtime must keep it up")
	}
	var exits []int
	stopOrExit(context.Background(), &downgradeProbe{stopErr: errors.New("No module named 'gateway.session_acp_lifecycle'")}, func(code int) { exits = append(exits, code) })
	stopOrExit(context.Background(), &downgradeProbe{}, func(code int) { exits = append(exits, code) })
	if len(exits) != 1 || exits[0] != 1 {
		t.Fatal(exits)
	}
}

func TestTheTreeProbeReadsTheCheckoutOnDisk(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.email", "t@example.invalid")
	git("config", "user.name", "T")
	stateDir := filepath.Join(root, "pkg", "state")
	os.MkdirAll(stateDir, 0o755)
	source := filepath.Join(stateDir, "schema.go")
	os.WriteFile(source, []byte("package state\n\nconst schema = `CREATE TABLE IF NOT EXISTS sessions`\n"), 0o644)
	git("add", ".")
	git("commit", "-m", "old release")
	git("tag", "old")
	os.WriteFile(source, []byte("package state\n\nconst schema = `CREATE TABLE IF NOT EXISTS session_admissions`\n"), 0o644)
	git("commit", "-am", "runtime")
	if treePredatesRuntime(root) {
		t.Fatal("the runtime tree must not read as predating")
	}
	git("checkout", "-q", "--detach", "old")
	if !treePredatesRuntime(root) {
		t.Fatal("the old release must read as predating")
	}
	if treePredatesRuntime(filepath.Join(root, "missing")) {
		t.Fatal("nothing readable proves nothing")
	}
}

func TestTheDowngradeExitCodeReachesTheProcessExit(t *testing.T) {
	probe := watchDowngrade(t, []*codeSkew{downgradeSkew, downgradeSkew}, false, func() bool { return true })
	runner := allocGatewayRunner()
	runner.exitCode = probe.exitCode
	runner.restartRequested, runner.restartViaService = false, false
	_, err := resolveGatewayExitVerdict(runner, false)
	var exit *gateway.SystemExitError
	if !errors.As(err, &exit) || exit.Code != gateway.GatewayServiceRestartExitCode {
		t.Fatal(err)
	}
}
