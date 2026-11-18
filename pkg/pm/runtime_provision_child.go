package pm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Flag of the provisioning child naming the environment it provisions
const runtimeProvisionVenvFlag = "--venv"

// Handshake lines the provisioning child writes once it holds the store lock, finds it busy, or cannot try
const (
	provisionHandshakeHeld   = "held"
	provisionHandshakeBusy   = "busy"
	provisionHandshakeFailed = "failed "
)

// Most provisioning children one wait starts while other processes keep the store lock
const provisionJoinAttempts = 8

// Started provisioning child, its handshake stream when one was asked for and its exit wait
type provisionLaunch struct {
	handshake io.ReadCloser
	wait      func() error
}

// Command a binary runs as the detached provisioning child, its executable and hidden entry verb
type RuntimeProvisioner func() ([]string, error)

// Provisioning child command the binary's main registered, nil until it registers one
var runtimeProvisioner atomic.Pointer[RuntimeProvisioner]

// Records the command every runtime provisioning of this process starts as its detached child
func RegisterRuntimeProvisioner(provisioner RuntimeProvisioner) {
	runtimeProvisioner.Store(&provisioner)
}

// Starts the provisioning child of venv, replaced by tests with one that runs the child body in this process
var launchRuntimeProvisioner = startProvisionChild

// Starts the detached provisioning child of venv, its stdout carrying the handshake when asked for
func startProvisionChild(venv string, handshake bool) (*provisionLaunch, error) {
	provisioner := runtimeProvisioner.Load()
	if provisioner == nil || *provisioner == nil {
		return nil, errors.New("this binary registers no provisioning child for the runtime environment")
	}
	base, err := (*provisioner)()
	if err != nil {
		return nil, err
	}
	start := func(breakaway bool) (*exec.Cmd, io.ReadCloser, error) {
		cmd := exec.Command(base[0], append(append([]string{}, base[1:]...), runtimeProvisionVenvFlag, venv)...)
		cmd.Dir = filepath.Dir(venv)
		cmd.SysProcAttr = home.DetachAttr(breakaway)
		var out io.ReadCloser
		if handshake {
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				return nil, nil, err
			}
			out = pipe
		}
		return cmd, out, cmd.Start()
	}
	cmd, out, err := start(true)
	if err != nil && runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) {
		cmd, out, err = start(false)
	}
	if err != nil {
		return nil, err
	}
	return &provisionLaunch{handshake: out, wait: cmd.Wait}, nil
}

// Whether the child reported holding the store lock, an error when it could not try or ended before reporting
func readProvisionHandshake(r io.ReadCloser) (bool, error) {
	defer r.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	switch line = strings.TrimSuffix(line, "\n"); line {
	case provisionHandshakeHeld:
		return true, nil
	case provisionHandshakeBusy:
		return false, nil
	}
	if cause, failed := strings.CutPrefix(line, provisionHandshakeFailed); failed {
		return false, errors.New(cause)
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("the provisioning child exited before reporting whether it holds the store lock")
	}
	return false, err
}

// Waits for the exit of a started child unless ctx ends first
func waitProvisionChild(ctx context.Context, launch *provisionLaunch) error {
	done := make(chan error, 1)
	go func() { done <- launch.wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Waits for the single provisioning of venv, starting the detached child when none runs, returning venv or why not
func awaitRuntimeEnvironment(ctx context.Context, venv string) (string, error) {
	unprovisioned := func(err error) (string, error) { return "", &RuntimeUnprovisioned{Venv: venv, Err: err} }
	if !LazyInstallsAllowedFor(ctx) {
		return unprovisioned(ToolRuntime.refuse(ctx))
	}
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return "", err
	}
	gate, root := newProvisionGate(venv, requirements), RepoRoot()
	for range provisionJoinAttempts {
		ready, err := runtimeSetReady(ctx, root, venv, gate)
		if err != nil {
			return "", err
		}
		if ready {
			return venv, nil
		}
		if deferred := gate.deferral(); deferred != nil {
			return unprovisioned(deferred)
		}
		launch, err := launchRuntimeProvisioner(venv, true)
		if err != nil {
			return unprovisioned(err)
		}
		held, err := readProvisionHandshake(launch.handshake)
		if err != nil || !held {
			go launch.wait()
		}
		if err != nil {
			return unprovisioned(err)
		}
		if !held {
			if err := gate.awaitRelease(ctx); err != nil {
				return "", err
			}
			continue
		}
		exit := waitProvisionChild(ctx, launch)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if exit == nil || gate.deferral() != nil {
			continue
		}
		ready, err = runtimeSetReady(ctx, root, venv, gate)
		if err != nil {
			return "", err
		}
		if !ready {
			return unprovisioned(fmt.Errorf("the provisioning child ended before recording a result: %w", exit))
		}
	}
	return unprovisioned(fmt.Errorf("other processes held the provisioning lock through %d attempts to join a provisioning", provisionJoinAttempts))
}

// Starts the detached provisioning child unless venv is ready, deferred, already provisioning, or lazy installs are off
func StartRuntimeEnvironment(ctx context.Context) error {
	root := RepoRoot()
	if LiveCheckoutUnderTest(resolvePath(root)) {
		return nil
	}
	venv, err := SelectedVenv(ctx, root)
	if err != nil {
		return err
	}
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return err
	}
	gate := newProvisionGate(venv, requirements)
	ready, err := runtimeSetReady(ctx, root, venv, gate)
	if err != nil || ready || gate.deferral() != nil || !LazyInstallsAllowedFor(ctx) {
		return err
	}
	if running, err := gate.running(); err != nil || running {
		return err
	}
	launch, err := launchRuntimeProvisioner(venv, false)
	if err != nil {
		return err
	}
	go launch.wait()
	return nil
}

// Body of the detached provisioning child, the handshake on stdout and its log beside the store, its exit status
func ServeRuntimeProvisioning(ctx context.Context, args []string) int {
	if len(args) != 2 || args[0] != runtimeProvisionVenvFlag || args[1] == "" {
		os.Stdout.Close()
		fmt.Fprintln(os.Stderr, "runtime provisioning: usage: "+runtimeProvisionVenvFlag+" <environment>")
		return 2
	}
	attach := func(log *os.File) {
		os.Stderr = log
		if err := debug.SetCrashOutput(log, debug.CrashOptions{}); err != nil {
			fmt.Fprintf(log, "crash output stays on the inherited stderr: %s\n", err)
		}
	}
	if err := provisionDetached(ctx, args[1], os.Stdout, attach); err != nil {
		return 1
	}
	return 0
}

// Provisions venv when this run takes the store lock, telling the handshake whether it did and logging beside the store
func provisionDetached(ctx context.Context, venv string, handshake io.WriteCloser, attach func(*os.File)) error {
	requirements, err := ToolRuntime.requirementsText()
	if err != nil {
		return reportProvisionFailure(handshake, err)
	}
	gate := newProvisionGate(venv, requirements)
	release, held, err := gate.tryEnter()
	if err != nil {
		return reportProvisionFailure(handshake, err)
	}
	line := provisionHandshakeBusy
	if held {
		line = provisionHandshakeHeld
		defer release()
	}
	_, werr := io.WriteString(handshake, line+"\n")
	if cerr := handshake.Close(); werr == nil {
		werr = cerr
	}
	if !held {
		return werr
	}
	log, err := os.Create(filepath.Join(gate.store, "provision.log"))
	if err != nil {
		return gate.failed(err)
	}
	defer log.Close()
	if attach != nil {
		attach(log)
	}
	fmt.Fprintf(log, "%s provisioning %s\n", provisionNow().Format(time.RFC3339), venv)
	if deferred := gate.deferral(); deferred != nil {
		fmt.Fprintf(log, "deferred: %s\n", deferred)
		return nil
	}
	installed, err := ToolRuntime.installedIn(venv, true)
	if err != nil {
		fmt.Fprintf(log, "failed: %s\n", err)
		return gate.failed(err)
	}
	if installed && !gate.unfinished() {
		fmt.Fprintln(log, "already provisioned")
		return nil
	}
	if err := provisionUnderGate(ctx, gate, venv, RuntimeEnvironmentOptions{Output: log}, requirements); err != nil {
		fmt.Fprintf(log, "failed: %s\n", err)
		return gate.failed(err)
	}
	fmt.Fprintf(log, "%s provisioned\n", provisionNow().Format(time.RFC3339))
	return nil
}

// Tells the waiting parent why the child could not try the store lock and returns that cause
func reportProvisionFailure(handshake io.WriteCloser, cause error) error {
	io.WriteString(handshake, provisionHandshakeFailed+strings.ReplaceAll(cause.Error(), "\n", " ")+"\n")
	handshake.Close()
	return cause
}
