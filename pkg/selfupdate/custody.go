package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	custodyUnavailable = "icebath: update custody unavailable"
	custodyRefusedExit = 87
	custodyDrain       = 5 * time.Second
	// Hidden first arguments a main or TestMain routes to the custody launchers through DispatchCustodyLauncher
	CustodyReapEntry      = "__icebath_custody_reap__"
	CustodyCustodianEntry = "__icebath_custody_custodian__"
	CustodyJoinEntry      = "__icebath_custody_join__"
)

var (
	custodyMoves = map[string]bool{"checkout": true, "merge": true, "read-tree": true, "reset": true, "switch": true}
	// Seams a test replaces to drive one platform's branch or the lock state
	custodyWindows               = func() bool { return runtime.GOOS == "windows" }
	custodyHeld                  = updaterecord.CheckoutHeld
	custodyDeathSignal           = updaterecord.DeathSignalAttr
	custodyStdout      io.Writer = os.Stdout
	custodyPopen                 = popenCustody
	// Latch proving this binary dispatches the launcher entries, the only state a launcher may re-execute it in
	custodyDispatched atomic.Bool
)

// Stream a child option reads from or writes to the null device
type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(p []byte) (int, error) { return len(p), nil }

// Null device stream for ChildOptions Stdin, Stdout or Stderr
var DevNull devNull

// Subprocess options of one custody child, a nil stream inherits this process's own
type ChildOptions struct {
	Dir        string
	Env        []string
	Input      *string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Capture    bool
	Text       bool
	Timeout    time.Duration
	Check      bool
	ExtraFiles []*os.File
	Attr       *syscall.SysProcAttr
}

// Outcome of one finished custody child
type CompletedProcess struct {
	Args       []string
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Custody child that exited nonzero under a checked run
type CalledProcessError struct {
	ReturnCode int
	Cmd        []string
	Output     string
	Stderr     string
}

func (e *CalledProcessError) Error() string {
	return (&pm.CalledProcessError{Argv: e.Cmd, Code: e.ReturnCode}).Error()
}

func init() {
	pm.SetUpdateLockHooks(pm.UpdateLockHooks{
		BindChild: func(cmd *exec.Cmd) (string, bool) {
			if err := updaterecord.BindChildToUpdateTree(cmd.Process.Pid); err != nil {
				return err.Error(), true
			}
			return "", false
		},
		ResumeSuspended: func(cmd *exec.Cmd) error { return updaterecord.ResumeSuspendedChild(cmd.Process) },
		Held:            updaterecord.CheckoutHeld,
		CheckoutLockFDs: func(root string) ([]int, error) {
			files, err := updaterecord.CheckoutLockFiles(root)
			fds := make([]int, 0, len(files))
			for _, f := range files {
				fds = append(fds, int(f.Fd()))
			}
			return fds, err
		},
	})
}

// Runs a custody launcher and exits when argv carries its entry, otherwise records that this binary handles them
func DispatchCustodyLauncher() {
	custodyDispatched.Store(true)
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case CustodyReapEntry:
		os.Exit(reapStandIn(os.Args[2:]))
	case CustodyCustodianEntry:
		os.Exit(reapCustodian(os.Args[2:]))
	case CustodyJoinEntry:
		os.Exit(joinJobMain(os.Args[2:], newJoinAPI()))
	}
}

// Path of this binary for a launcher, panicking when nothing in it dispatches the launcher entries
func launcherExecutable() (string, error) {
	if !custodyDispatched.Load() {
		panic("a custody launcher would re-execute a binary that never called selfupdate.DispatchCustodyLauncher, call it first in main or TestMain")
	}
	return os.Executable()
}

// Checkout lock files a mutator child inherits, none for other children and on Windows
func custodyLockFiles(inheritLock bool) []*os.File {
	if !inheritLock || runtime.GOOS == "windows" {
		return nil
	}
	return updaterecord.CustodySpawnFiles()
}

// Options with the lock files for a mutator or the parent death signal for any other child of a held lock
func custodyKwargs(inheritLock bool, opts ChildOptions) ChildOptions {
	if files := custodyLockFiles(inheritLock); len(files) > 0 {
		merged := append([]*os.File{}, opts.ExtraFiles...)
		for _, f := range files {
			if !slices.Contains(merged, f) {
				merged = append(merged, f)
			}
		}
		opts.ExtraFiles = merged
	} else if !inheritLock && custodyHeld() && opts.Attr == nil {
		if arm := custodyDeathSignal(); arm != nil {
			opts.Attr = arm
		}
	}
	return opts
}

// Child whose streams are wired the way subprocess wires its keyword arguments
type startedChild struct {
	cmd     *exec.Cmd
	argv    []string
	stdin   io.WriteCloser
	outR    *os.File
	errR    *os.File
	mu      sync.Mutex
	reaped  bool
	waitErr error
}

// Command for argv with the options' directory, environment, descriptors and streams set
func prepareChild(argv []string, opts ChildOptions) (*startedChild, []io.Closer, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.ExtraFiles, cmd.SysProcAttr = opts.Dir, opts.Env, opts.ExtraFiles, opts.Attr
	child := &startedChild{cmd: cmd, argv: argv}
	var parentEnds []io.Closer
	closeAll := func() {
		for _, c := range parentEnds {
			c.Close()
		}
	}
	switch {
	case opts.Input != nil:
		w, err := cmd.StdinPipe()
		if err != nil {
			return nil, nil, err
		}
		child.stdin = w
	case opts.Stdin == nil:
		cmd.Stdin = os.Stdin
	case opts.Stdin == io.Reader(DevNull):
	default:
		cmd.Stdin = opts.Stdin
	}
	stream := func(w io.Writer, inherit *os.File, read **os.File, target *io.Writer) error {
		switch {
		case opts.Capture:
			r, pw, err := os.Pipe()
			if err != nil {
				return err
			}
			*read, *target = r, pw
			parentEnds = append(parentEnds, pw)
		case w == nil:
			*target = inherit
		case w == io.Writer(DevNull):
		default:
			*target = w
		}
		return nil
	}
	if err := stream(opts.Stdout, os.Stdout, &child.outR, &cmd.Stdout); err != nil {
		return nil, nil, err
	}
	if err := stream(opts.Stderr, os.Stderr, &child.errR, &cmd.Stderr); err != nil {
		closeAll()
		if child.outR != nil {
			child.outR.Close()
		}
		return nil, nil, err
	}
	return child, parentEnds, nil
}

// Starts a prepared child, closing this process's write ends of its pipes
func (c *startedChild) start(parentEnds []io.Closer) error {
	err := c.cmd.Start()
	for _, end := range parentEnds {
		end.Close()
	}
	if err != nil {
		for _, r := range []*os.File{c.outR, c.errR} {
			if r != nil {
				r.Close()
			}
		}
	}
	return err
}

// Whether the child was already reaped, so its pid may name another process
func (c *startedChild) wasReaped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reaped
}

func popenChild(argv []string, opts ChildOptions) (*startedChild, error) {
	child, ends, err := prepareChild(argv, opts)
	if err != nil {
		return nil, err
	}
	return child, child.start(ends)
}

// Feeds input, collects output and waits, ending the child through onTimeout or onCancel when the wait cannot finish
func (c *startedChild) communicate(ctx context.Context, opts ChildOptions, onTimeout func(*startedChild) bool, onCancel func(*startedChild)) (*CompletedProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.stdin != nil {
		go func() {
			_, _ = io.WriteString(c.stdin, *opts.Input)
			c.stdin.Close()
		}()
	}
	var stdout, stderr []byte
	var readers sync.WaitGroup
	for _, pair := range []struct {
		r   *os.File
		dst *[]byte
	}{{c.outR, &stdout}, {c.errR, &stderr}} {
		if pair.r == nil {
			continue
		}
		readers.Add(1)
		go func(r *os.File, dst *[]byte) {
			*dst, _ = io.ReadAll(r)
			readers.Done()
		}(pair.r, pair.dst)
	}
	exited := make(chan struct{})
	go func() {
		werr := c.cmd.Wait()
		c.mu.Lock()
		c.reaped, c.waitErr = true, werr
		c.mu.Unlock()
		close(exited)
	}()
	drained := make(chan struct{})
	go func() {
		<-exited
		readers.Wait()
		close(drained)
	}()
	var timer <-chan time.Time
	if opts.Timeout > 0 {
		t := time.NewTimer(opts.Timeout)
		defer t.Stop()
		timer = t.C
	}
	closeReaders := func() {
		for _, r := range []*os.File{c.outR, c.errR} {
			if r != nil {
				r.Close()
			}
		}
	}
	select {
	case <-drained:
	case <-timer:
		if onTimeout(c) {
			select {
			case <-drained:
			case <-time.After(custodyDrain):
			}
		} else {
			<-exited
		}
		closeReaders()
		return nil, &updaterecord.GitTimeout{Args: c.argv, Timeout: opts.Timeout}
	case <-ctx.Done():
		onCancel(c)
		closeReaders()
		return nil, ctx.Err()
	}
	closeReaders()
	code := c.cmd.ProcessState.ExitCode()
	if status, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		code = -int(status.Signal())
	}
	done := &CompletedProcess{Args: c.argv, ReturnCode: code, Stdout: string(stdout), Stderr: string(stderr)}
	if opts.Text {
		done.Stdout, done.Stderr = decodeChildText(stdout), decodeChildText(stderr)
	}
	if opts.Check && code != 0 {
		return done, &CalledProcessError{ReturnCode: code, Cmd: c.argv, Output: done.Stdout, Stderr: done.Stderr}
	}
	return done, nil
}

func decodeChildText(raw []byte) string {
	return pystr.TranslateNewlines(pystr.DecodeUTF8Replace(raw))
}

// Kills the child process alone unless it was already reaped
func killChildOnly(c *startedChild) {
	if !c.wasReaped() {
		_ = c.cmd.Process.Kill()
	}
}

// Starts argv in the update tree's custody, bound to the update job on Windows before it runs
func Popen(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*exec.Cmd, error) {
	child, err := popenCustody(ctx, argv, inheritLock, opts)
	if err != nil {
		return nil, err
	}
	return child.cmd, nil
}

func popenCustody(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*startedChild, error) {
	if custodyWindows() && custodyHeld() {
		opts.Attr = suspendedAttr(opts.Attr)
		child, err := popenChild(argv, opts)
		if err != nil {
			return nil, err
		}
		if err := bindSuspended(ctx, child); err != nil {
			killChildOnly(child)
			return nil, err
		}
		return child, nil
	}
	return popenChild(argv, custodyKwargs(inheritLock, opts))
}

// Binds a suspended child to the update job then resumes it, a refused bind kills it before it ran
func bindSuspended(ctx context.Context, child *startedChild) error {
	if err := updaterecord.BindChildToUpdateTree(child.cmd.Process.Pid); err != nil {
		_ = child.cmd.Process.Kill()
		_ = child.cmd.Wait()
		return updaterecord.RefuseCustodyChild(ctx, child.argv, err, false)
	}
	if err := updaterecord.ResumeSuspendedChild(child.cmd.Process); err != nil {
		return err
	}
	updaterecord.MarkCustodyRan()
	return nil
}

// Runs argv to completion in the update tree's custody with the lock files for a mutator
func Run(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
	if !(custodyWindows() && custodyHeld()) {
		child, err := popenChild(argv, custodyKwargs(inheritLock, opts))
		if err != nil {
			return nil, err
		}
		return child.communicate(ctx, opts, func(c *startedChild) bool { killChildOnly(c); return false }, killChildOnly)
	}
	child, err := custodyPopen(ctx, argv, inheritLock, opts)
	if err != nil {
		return nil, err
	}
	return child.communicate(ctx, opts, func(c *startedChild) bool {
		if !c.wasReaped() {
			proc.KillTree(c.cmd.Process.Pid)
		}
		return true
	}, killChildOnly)
}

// Starts a checkout writer after the commit point, a refused job bind is printed and receipted and it still runs
func PopenPostCommit(ctx context.Context, argv []string, label string, opts ChildOptions) (*exec.Cmd, error) {
	if !(custodyWindows() && custodyHeld()) {
		child, err := popenChild(argv, custodyKwargs(true, opts))
		if err != nil {
			return nil, err
		}
		return child.cmd, nil
	}
	opts.Attr = suspendedAttr(opts.Attr)
	child, err := popenChild(argv, opts)
	if err != nil {
		return nil, err
	}
	if refusal := updaterecord.BindChildToUpdateTree(child.cmd.Process.Pid); refusal != nil {
		detail := fmt.Sprintf("the update's job would not take the %s (%v), so it runs outside the job, holding its own checkout lease", label, refusal)
		fmt.Fprintf(custodyStdout, "  ⚠ %s\n", detail)
		updaterecord.RecordStep(ctx, "update_custody", false, detail)
	}
	if err := updaterecord.ResumeSuspendedChild(child.cmd.Process); err != nil {
		_ = child.cmd.Process.Kill()
		_ = child.cmd.Wait()
		return nil, err
	}
	return child.cmd, nil
}

// Custody runner of an updater git outside the owner watched path, the update_custody.run hermes tests spy on
var runInCustody = Run

// Runs git in the updater's custody, the lock files only into local mutators, a partial clone prefetched first
func RunGit(ctx context.Context, gitCmd, args []string, opts ChildOptions) (*CompletedProcess, error) {
	argv := updaterecord.GitArgv(gitCmd, args)
	mutator := updaterecord.IsLocalMutator(argv[1:])
	if mutator {
		if err := prefetchForMove(ctx, gitCmd, args, opts); err != nil {
			return nil, err
		}
	}
	if !custodyWindows() && custodyHeld() && opts.Attr == nil && custodyDeathSignal() == nil {
		return runOwnerWatched(ctx, argv, custodyKwargs(mutator, opts), !mutator)
	}
	return runInCustody(ctx, argv, mutator, opts)
}

// Whether the repository has a promisor remote recorded in its common config
func partialClone(gitCmd []string, dir string) (bool, error) {
	cwd := dir
	for i := 0; i+1 < len(gitCmd); i++ {
		if gitCmd[i] == "-C" {
			cwd = joinPyPath(cwd, gitCmd[i+1])
		}
	}
	if cwd == "" {
		cwd = "."
	}
	common, err := updaterecord.GitCommonDir(cwd)
	if err != nil {
		return false, err
	}
	if common == "" {
		return false, nil
	}
	raw, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		return false, nil
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	return strings.Contains(pystr.Lower(pystr.DecodeUTF8Replace(raw)), "partialclone"), nil
}

// Path(base or ".") / value, an absolute value replacing the base
func joinPyPath(base, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	if base == "" {
		base = "."
	}
	return strings.TrimRight(base, string(filepath.Separator)) + string(filepath.Separator) + value
}

// Fetches what a move in a partial clone reads, without the lock files and with the user's credential helpers
func prefetchForMove(ctx context.Context, gitCmd, args []string, opts ChildOptions) error {
	sub := updaterecord.GitSubcommand(args)
	if !custodyMoves[sub] {
		return nil
	}
	partial, err := partialClone(gitCmd, opts.Dir)
	if err != nil || !partial {
		return err
	}
	common := ChildOptions{Dir: opts.Dir, Env: opts.Env}
	resolve := func(stdin *string, query ...string) (string, error) {
		o := common
		o.Capture, o.Text = true, true
		if stdin != nil {
			o.Input = stdin
		} else {
			o.Stdin = DevNull
		}
		out, err := Run(ctx, updaterecord.GitArgv(gitCmd, query), false, o)
		if err != nil {
			return "", err
		}
		if out.ReturnCode == 0 && pystr.Strip(out.Stdout) != "" {
			return pystr.Strip(out.Stdout), nil
		}
		return "", nil
	}
	err = func() error {
		var targets []string
		for _, arg := range args[slices.Index(args, sub)+1:] {
			if arg == "--" {
				break
			}
			if !strings.HasPrefix(arg, "-") {
				target, err := resolve(nil, "rev-parse", "--verify", "-q", arg+"^{commit}")
				if err != nil {
					return err
				}
				if target != "" && !slices.Contains(targets, target) {
					targets = append(targets, target)
				}
			}
		}
		if len(targets) == 0 {
			return nil
		}
		base, err := resolve(nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
		if err != nil {
			return err
		}
		if base == "" {
			empty := ""
			if base, err = resolve(&empty, "hash-object", "-t", "tree", "--stdin"); err != nil {
				return err
			}
		}
		for _, target := range targets {
			if base != "" && target != base {
				o := common
				o.Stdin, o.Stdout, o.Stderr = DevNull, DevNull, DevNull
				if _, err := Run(ctx, updaterecord.GitArgv(gitCmd, []string{"diff", "--binary", "--no-ext-diff", "--no-textconv", base, target}), false, o); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		slog.Debug(fmt.Sprintf("Could not prefetch the objects for git %s: %v", sub, err))
	}
	return nil
}

// Notice a refused job join left in its report file, logged and receipted, nil when the child joined
func reportRefusedJoin(ctx context.Context, report string, argv []string) *string {
	raw, err := os.ReadFile(report)
	note := ""
	if err == nil {
		if text, derr := pystr.DecodeUTF8Sig(raw); derr == nil {
			note = pystr.Strip(text)
		}
	}
	_ = os.Remove(report)
	if note == "" {
		return nil
	}
	head := argv
	if len(head) > 2 {
		head = head[:2]
	}
	slog.Warn(note + ": " + strings.Join(head, " "))
	updaterecord.RecordStep(ctx, "update_custody", false, note+": "+strings.Join(head, " "))
	return &note
}

// Command a runner that hides its child starts, kept in checkout custody by a launcher
type Contained struct {
	Argv       []string
	ExtraFiles []*os.File
	Attr       *syscall.SysProcAttr
	finish     func() error
}

// Ends the contained run, returning the refusal a launcher reported when the command never ran
func (c *Contained) Close() error {
	if c.finish == nil {
		return nil
	}
	finish := c.finish
	c.finish = nil
	return finish()
}

// Argv and spawn options for a checkout writer run by a runner that hides its child, under a custody launcher
func ContainedCommand(ctx context.Context, argv []string, inheritLock bool, root string) (*Contained, error) {
	argv = append([]string{}, argv...)
	if !(custodyWindows() && custodyHeld()) {
		var files []*os.File
		if inheritLock && root != "" {
			found, err := updaterecord.CheckoutLockFiles(root)
			if err != nil {
				return nil, err
			}
			files = found
		} else {
			files = custodyLockFiles(inheritLock)
		}
		if len(files) == 0 {
			return &Contained{Argv: argv}, nil
		}
		exe, err := launcherExecutable()
		if err != nil {
			return nil, err
		}
		fds := make([]string, len(files))
		for i := range files {
			fds[i] = fmt.Sprint(3 + i)
		}
		return &Contained{Argv: append([]string{exe, CustodyReapEntry, strings.Join(fds, ",")}, argv...), ExtraFiles: files}, nil
	}
	return containedInJob(ctx, argv)
}

// Join launcher argv for a command that must run inside the update's job, refused when the job cannot be handed over
func containedJoin(ctx context.Context, argv []string, handle uintptr, closeHandle func()) (*Contained, error) {
	exe, err := launcherExecutable()
	if err != nil {
		closeHandle()
		return nil, err
	}
	f, err := os.CreateTemp("", "icebath-custody-*.txt")
	if err != nil {
		closeHandle()
		return nil, err
	}
	report := f.Name()
	f.Close()
	return &Contained{
		Argv: append([]string{exe, CustodyJoinEntry, fmt.Sprint(handle), report}, argv...),
		Attr: inheritHandleAttr(handle),
		finish: func() error {
			closeHandle()
			if note := reportRefusedJoin(ctx, report, argv); note != nil {
				cause, _, _ := strings.Cut(strings.TrimPrefix(*note, custodyUnavailable+" ("), ")")
				return updaterecord.RefuseCustodyChild(ctx, argv, cause, true)
			}
			updaterecord.MarkCustodyRan()
			return nil
		},
	}, nil
}

// Join launcher run for a command inside the update's job, refused when the job handle cannot be handed over
func containedInJob(ctx context.Context, argv []string) (*Contained, error) {
	handle, closeHandle, err := inheritableJobHandle()
	if err != nil {
		return nil, updaterecord.RefuseCustodyChild(ctx, argv, err, false)
	}
	return containedJoin(ctx, argv, handle, closeHandle)
}
