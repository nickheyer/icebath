package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Rust target triple of each supported target
var rustTriple = map[string]string{
	"win32-x64":        "x86_64-pc-windows-msvc",
	"win32-arm64":      "aarch64-pc-windows-msvc",
	"linux-x64":        "x86_64-unknown-linux-gnu",
	"linux-arm64":      "aarch64-unknown-linux-gnu",
	"linux-x64-musl":   "x86_64-unknown-linux-musl",
	"linux-arm64-musl": "aarch64-unknown-linux-musl",
	"darwin-x64":       "x86_64-apple-darwin",
	"darwin-arm64":     "aarch64-apple-darwin",
}

// Node release platform name of each supported target
var nodePlat = map[string]string{
	"win32-x64":        "win-x64",
	"win32-arm64":      "win-arm64",
	"linux-x64":        "linux-x64",
	"linux-arm64":      "linux-arm64",
	"linux-x64-musl":   "linux-x64-musl",
	"linux-arm64-musl": "linux-arm64-musl",
	"darwin-x64":       "darwin-x64",
	"darwin-arm64":     "darwin-arm64",
}

// Target the catalogue compares against, replaced by tests the way they patch the module name
var packagesCurrentTarget = CurrentTarget

// Python KeyError text for a missing mapping key
func keyError(key string) error { return fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key)) }

// Python ValueError text of unpacking a split into exactly two names
func unpackTwo(parts []string) (string, string, error) {
	switch {
	case len(parts) > 2:
		return "", "", fmt.Errorf("ValueError: too many values to unpack (expected 2, got %d)", len(parts))
	case len(parts) < 2:
		return "", "", fmt.Errorf("ValueError: not enough values to unpack (expected 2, got %d)", len(parts))
	}
	return parts[0], parts[1], nil
}

// Splits at the first separator the way Python str partition does
func partition(s, sep string) (string, string) {
	before, after, _ := strings.Cut(s, sep)
	return before, after
}

// BinaryPackage is a downloaded archive exposing one binary, the shape most tools take
type BinaryPackage struct {
	BasePackage
	BinaryRel map[string]string
	NoFlatten bool
	NoProbe   bool
	ProbeArgs []string
	ProbeCwd  bool
}

// Argv after the binary for the smoke probe, the GNU version flag unless overridden
func (b *BinaryPackage) probeArgs() []string {
	if b.ProbeArgs == nil {
		return []string{"--version"}
	}
	return b.ProbeArgs
}

// Relative binary path for the exact target, then for its platform family
func (b *BinaryPackage) rel(target string) string {
	if rel := b.BinaryRel[target]; rel != "" {
		return rel
	}
	if strings.HasPrefix(target, "win32") {
		return b.BinaryRel["win32"]
	}
	return b.BinaryRel["posix"]
}

func (b *BinaryPackage) Stage(store *Store, staged, version, target string) error {
	if !b.NoFlatten {
		return FlattenSingleDir(staged)
	}
	return nil
}

func (b *BinaryPackage) Binary(entry, target string) string {
	rel := b.rel(target)
	if rel == "" {
		return ""
	}
	return filepath.Join(entry, filepath.FromSlash(rel))
}

// Checks file and architecture evidence everywhere and runs the smoke probe only on the native target
func (b *BinaryPackage) Verify(entry, target string) string {
	bin := b.dispatch().Binary(entry, target)
	if bin == "" {
		return "no binary_rel for this target"
	}
	if reason := b.BinaryReason(bin, entry, target); reason != "" {
		return reason
	}
	if b.NoProbe {
		return ""
	}
	current, err := packagesCurrentTarget()
	if err != nil {
		return err.Error()
	}
	if target != current {
		return ""
	}
	args := b.probeArgs()
	described := Native(bin) + " " + strings.Join(args, " ")
	env, err := b.probeEnvironment()
	if err != nil {
		return fmt.Sprintf("could not compose the environment for %s: %s", described, err)
	}
	spec := processSpec{Argv: append([]string{Native(bin)}, args...), Env: env, Capture: true, Timeout: 60 * time.Second}
	if b.ProbeCwd {
		spec.Dir = Native(filepath.Dir(bin))
	}
	result, err := runProcess(spec)
	var timeout *processTimeoutError
	switch {
	case errors.As(err, &timeout):
		return described + " timed out after 60s"
	case err != nil:
		return fmt.Sprintf("could not exec %s: %s", described, pypath.OSErrorText(err))
	case result.Code != 0:
		return ProbeReason(bin, result.Code, result.Stdout, result.Stderr)
	}
	return ""
}

// Probe environment from the concrete package's ProbeEnv, else the deps composed over this process
func (b *BinaryPackage) probeEnvironment() (map[string]string, error) {
	if p, ok := b.dispatch().(interface {
		ProbeEnv() (map[string]string, error)
	}); ok {
		return p.ProbeEnv()
	}
	return b.depsProbeEnv()
}

// Process environment, or the installed deps' env composed in so a shim finds the runtime it extends
func (b *BinaryPackage) depsProbeEnv() (map[string]string, error) {
	if len(b.Deps) == 0 {
		return environMap(), nil
	}
	return envFor(context.Background(), b.Deps, nil)
}

// Target of the Android termux userland whose tools arrive as termux debs
const bionicTarget = "linux-arm64-bionic"

// bionicDebArm stages a termux deb on the bionic target and the binary archive everywhere else
type bionicDebArm struct {
	BinaryPackage
	DebName    string
	MainBinRel string
}

func (t *bionicDebArm) MainRel(target string) string { return t.MainBinRel }

// Deb view of this package so the hardened deb behaviour dispatches to the concrete package
func (t *bionicDebArm) debView() *DebPackage {
	view := &DebPackage{BasePackage: t.BasePackage, DebName: t.DebName}
	if view.self == nil {
		view.self = t
	}
	return view
}

// Control field name the termux deb declares
func (t *bionicDebArm) DebPackageName() string { return t.debView().DebPackageName() }

func (t *bionicDebArm) Unpack(archive, staged, target string) error {
	if target == bionicTarget {
		return t.debView().Unpack(archive, staged, target)
	}
	return t.BinaryPackage.Unpack(archive, staged, target)
}

func (t *bionicDebArm) Stage(store *Store, staged, version, target string) error {
	if target != bionicTarget {
		return t.BinaryPackage.Stage(store, staged, version, target)
	}
	return nil
}

func (t *bionicDebArm) Binary(entry, target string) string {
	if target == bionicTarget {
		view := t.debView()
		main := view.dispatch().(DebMain).MainRel(target)
		return filepath.Join(entry, filepath.FromSlash(view.Prefix()), filepath.FromSlash(main))
	}
	return t.BinaryPackage.Binary(entry, target)
}

func (t *bionicDebArm) Verify(entry, target string) string {
	if target == bionicTarget {
		return t.debView().Verify(entry, target)
	}
	return t.BinaryPackage.Verify(entry, target)
}

// EnvChecker is a package whose env refuses an entry that cannot supply it
type EnvChecker interface {
	CheckEnv(entry, target string) error
}

// Env diff of an entry, the error a package raises when its entry cannot supply one
func PackageEnv(p Package, entry, target string) (EnvDiff, error) {
	if checker, ok := p.(EnvChecker); ok {
		if err := checker.CheckEnv(entry, target); err != nil {
			return nil, err
		}
	}
	return p.Env(entry, target), nil
}

// PinOnlyPackage is a pin whose bytes pm never stages, its consumers verifying the digest themselves
type PinOnlyPackage interface {
	PinOnly() bool
}

// processSpec is one child run the way subprocess.run runs it
type processSpec struct {
	Argv      []string
	Dir       string
	Env       map[string]string
	Capture   bool
	NullStdin bool
	NoWindow  bool
	Timeout   time.Duration
}

// processResult is a finished child's exit status and captured output
type processResult struct {
	Code   int
	Stdout []byte
	Stderr []byte
}

// processTimeoutError is a child killed at its deadline, Python subprocess TimeoutExpired
type processTimeoutError struct {
	Argv    []string
	Timeout time.Duration
}

func (e *processTimeoutError) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", pyrepr.Repr(e.Argv), strconv.FormatFloat(e.Timeout.Seconds(), 'f', -1, 64))
}

// Runs a child to completion, replaced by tests the way they patch subprocess run
var runProcess = startProcess

// Spawns the child, drains captured pipes and kills it at the deadline as subprocess run does
func startProcess(spec processSpec) (processResult, error) {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = EnvList(spec.Env)
	}
	if !spec.NullStdin {
		cmd.Stdin = os.Stdin
	}
	hideWindow(cmd, spec.NoWindow)
	var stdout, stderr bytes.Buffer
	var readers []*os.File
	readersDone := make(chan struct{})
	if spec.Capture {
		outR, outW, err := os.Pipe()
		if err != nil {
			return processResult{}, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			outR.Close()
			outW.Close()
			return processResult{}, err
		}
		readers = []*os.File{outR, errR}
		cmd.Stdout, cmd.Stderr = outW, errW
		if err := cmd.Start(); err != nil {
			for _, f := range []*os.File{outR, outW, errR, errW} {
				f.Close()
			}
			return processResult{}, err
		}
		outW.Close()
		errW.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(&stdout, outR) }()
		go func() { defer wg.Done(); _, _ = io.Copy(&stderr, errR) }()
		go func() { wg.Wait(); close(readersDone) }()
	} else {
		if err := cmd.Start(); err != nil {
			return processResult{}, err
		}
		close(readersDone)
	}
	closeReaders := func() {
		for _, f := range readers {
			f.Close()
		}
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var deadline <-chan time.Time
	if spec.Timeout > 0 {
		timer := time.NewTimer(spec.Timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	var waitErr error
	exited, drained := false, false
	pending := readersDone
	for !exited || !drained {
		select {
		case waitErr = <-waitDone:
			exited = true
		case <-pending:
			drained, pending = true, nil
		case <-deadline:
			_ = cmd.Process.Kill()
			if !exited {
				<-waitDone
			}
			if runtime.GOOS == "windows" && !drained {
				<-pending
			}
			closeReaders()
			return processResult{}, &processTimeoutError{Argv: spec.Argv, Timeout: spec.Timeout}
		}
	}
	closeReaders()
	result := processResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.Code = pyReturnCode(exitErr)
	} else if waitErr != nil {
		return processResult{}, waitErr
	}
	return result, nil
}

// Reports whether a path is itself a symlink
func isSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// Case and separator folding of os.path normcase on this platform
func osNormcase(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	}
	return path
}

// Registers every catalogue tool package
func init() {
	for _, p := range []Package{
		newDmgbuild(), newUv(), newPython(), newVenv(""), newNodejs(), newTermuxDocker(), newNpm(), newGit(), newGh(),
		newFfmpeg(), newRipgrep(), newCuaDriver(), newChromium(), newWhisperCppCpu(),
	} {
		MustRegister(p)
	}
	for _, p := range llamaBackends() {
		MustRegister(p)
	}
}
