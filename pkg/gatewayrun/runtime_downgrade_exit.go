package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Poll cadence, consecutive confirmations and the schema line that marks a state store with the runtime ledger
const (
	runtimeDowngradePollS         = 30.0
	runtimeDowngradeConfirmations = 2
	runtimeSchemaMarker           = "CREATE TABLE IF NOT EXISTS session_admissions"
)

// Whether the checkout's state store sources are readable and none defines the runtime ledger
func treePredatesRuntime(root string) bool {
	paths, err := filepath.Glob(filepath.Join(root, "pkg", "state", "*.go"))
	if err != nil {
		return false
	}
	found := false
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		found = true
		if strings.Contains(string(data), runtimeSchemaMarker) {
			return false
		}
	}
	return found
}

// Code skew between the boot revision and the checkout, the pair of short revisions when the tree moved
type codeSkew struct{ boot, disk string }

// Log line when the gateway must stop for a downgraded tree, empty otherwise
func downgradeReason(skew *codeSkew, updateInProgress bool, predates func() bool) string {
	if skew == nil || updateInProgress || !predates() {
		return ""
	}
	return fmt.Sprintf("This gateway loaded %s, but its checkout is now at %s, a release that predates the gateway "+
		"runtime. Downgrading is unsupported; stopping so the service manager starts the older "+
		"release's own gateway.", skew.boot, skew.disk)
}

// Runner surface the downgrade watcher reads and stops
type downgradeRunner interface {
	downgradeRunning() bool
	downgradeExit(reason string, code int)
	downgradeStop(ctx context.Context) error
	keepDowngradeStop(task *gatewayTask)
}

func (r *GatewayRunner) downgradeRunning() bool { return r.running }

func (r *GatewayRunner) downgradeExit(reason string, code int) {
	r.exitReason, r.exitCode = &reason, &code
}

func (r *GatewayRunner) downgradeStop(ctx context.Context) error {
	return r.Stop(ctx, false, false, false)
}

func (r *GatewayRunner) keepDowngradeStop(task *gatewayTask) { r.runtimeDowngradeStop = task }

// Runs the normal drain and hard exits when the stop fails on the downgraded tree
func stopOrExit(ctx context.Context, runner downgradeRunner, hardExit func(int)) {
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return runner.downgradeStop(ctx)
	}()
	if err != nil {
		runLog().Error("Gateway stop failed on the downgraded checkout; exiting now", "exc_info", err)
		hardExit(1)
	}
}

// Probes of the downgrade watcher, zero values binding the code skew, update lock and checkout readers
type downgradeWatch struct {
	pollS       *float64
	skew        func() *codeSkew
	updateProbe func() (bool, error)
	predates    func() bool
	maxPolls    *int
}

// Stops the gateway after two consecutive reads of a checkout moved to a release without the runtime ledger
func runtimeDowngradeWatcher(ctx context.Context, runner downgradeRunner, w downgradeWatch) error {
	pollS := runtimeDowngradePollS
	if w.pollS != nil {
		pollS = *w.pollS
	}
	if w.skew == nil {
		w.skew = func() *codeSkew {
			boot, disk, ok := gateway.DetectCodeSkew()
			if !ok {
				return nil
			}
			return &codeSkew{boot, disk}
		}
	}
	if w.updateProbe == nil {
		w.updateProbe = func() (bool, error) { return updaterecord.UpdateInProgress(ctx, "") }
	}
	if w.predates == nil {
		root := pm.RepoRoot()
		w.predates = func() bool { return treePredatesRuntime(root) }
	}
	read := func() string {
		busy, err := func() (busy bool, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return w.updateProbe()
		}()
		if err != nil {
			busy = true
		}
		return downgradeReason(w.skew(), busy, w.predates)
	}
	seen, polls := 0, 0
	for runner.downgradeRunning() && (w.maxPolls == nil || polls < *w.maxPolls) {
		polls++
		reason := read()
		if reason != "" {
			seen++
		} else {
			seen = 0
		}
		if seen >= runtimeDowngradeConfirmations {
			runLog().Error(reason)
			runner.downgradeExit(reason, gateway.GatewayServiceRestartExitCode)
			runner.keepDowngradeStop(spawnGatewayTask(context.WithoutCancel(ctx), "runtime-downgrade-stop", func(c context.Context) (any, error) {
				stopOrExit(c, runner, os.Exit)
				return nil, nil
			}))
			return nil
		}
		if err := sleepCtx(ctx, pollS); err != nil {
			return err
		}
	}
	return nil
}
