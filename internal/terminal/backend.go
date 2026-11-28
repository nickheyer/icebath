package terminal

import (
	"context"
	"os"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// A place commands can run, what a backend plugin registers
type Backend struct {
	// Value of terminal.backend that picks it, lower case
	Name  string
	Title string
	About string
	// Where commands run as the system prompt names it when the backend isn't probed, such as "a Docker container (Linux)"
	Where string
	// Commands run off this machine
	Remote bool
	// Its filesystem is rooted away from the host, so host paths mean nothing inside it
	Container bool
	// Keeps commands away from the host, so risky ones need no approval
	Isolated bool
	// Whether the settings hand the sandbox folders of the host, which takes Isolated back so its risky commands need approval again
	Exposed func(cfg *config.Config) bool
	// Where the sandbox sees the profile folder, for the files it is handed; empty when host paths hold
	Home string
	// Folder commands start in when the working folder names none: ~ or an absolute path, /root when empty
	Cwd string
	// Variables holding the backend's own credentials, kept from every command of every backend
	Secrets []string
	// Key of the environment the sessions of the profile working in dir share, empty for one per session
	Shared func(cfg *config.Config, dir string) string
	// Host folder mounted into the sandbox for a session working in dir, and where the sandbox sees it; none when host is empty
	Mount func(cfg *config.Config, dir string) (host, sandbox string)
	// Folder of the sandbox a host folder stands for, such as one under ~ for a path in the host's home on a remote machine
	Remap func(cfg *config.Config, cwd string) string
	// Why the backend can't run with the settings and what to do about it, nil when it can; a *MissingError when this computer lacks what
	// it needs, such as its program
	Check func(cfg *config.Config) error
	// What was checked of what the backend needs, as its page in Sandbox lists it
	Doctor func(cfg *config.Config) []Check
	// Readies the backend ahead of its first command, such as building its image, saying how it goes line by line
	Prepare func(ctx context.Context, cfg *config.Config, say func(line string)) error
	// What Prepare does, a sentence its page says of it
	Readies string
	// Its environments keep processes running between commands and reach them through a command of this machine, so they can host the
	// Agent desktop
	Streams bool
	// Image change the profile's sandboxes wait on: those still running an older default image; nil when none waits
	Switch func(cfg *config.Config) *Switch
	// Keeps the profile's sandboxes on the image they run, or with approve moves them to the new one, answering the image chosen
	Decide func(cfg *config.Config, s *Switch, approve bool) (string, error)
	// Makes an environment
	Create func(ctx context.Context, spec *Spec) (Environment, error)
}

// Default image of a backend that changed under sandboxes still running the one before
type Switch struct {
	// Image most of them run, the one they would move to, and the sandboxes running another than that one
	Current, Target string
	Sandboxes       []string
}

// One thing checked of what a backend needs
type Check struct {
	OK bool
	// What was checked, in a few words, such as Docker daemon
	Label string
	// How it stands in a word or two, such as Running or Not set
	State string
	// What was found, a sentence; empty when the state says it all
	Detail string
	// What to do about it when it failed, in a person's words
	Fix string
	// Key of the setting that fixes it, empty when the fix is outside icebath
	Setting string
}

// What an environment is made for
type Spec struct {
	// Host the environment is kept by, whose supervisor and fence its processes go through
	Terminal *Terminal
	// Settings of the profile the environment serves
	Config *config.Config
	// Folder of that profile
	Dir string
	// Key the environment is kept under, a session or a profile wide key, safe to name a sandbox after with SafeName
	Key string
	// Session the environment serves, empty for one every session of the profile shares
	Session string
	// Folder commands start in
	Cwd string
	// Host folder mounted into the sandbox, empty for none
	HostCwd string
	// Folder of this machine the session that made the environment works in, where its project's own skills are; empty for none
	Folder string
	// Default for a command that names no timeout
	Timeout time.Duration
	// Made only to see where commands run, torn down right after
	Probe bool
}

// One backend's shell, sandbox or machine, kept for a session or a profile
type Environment interface {
	Execute(ctx context.Context, command string, opts Exec) (*Result, error)
	// Removes what the environment made, force reaching backends that keep sandboxes otherwise
	Cleanup(force bool) error
	Backend() string
	// Folder the next command starts in
	Cwd() string
	SetCwd(cwd string)
	// Sandbox side folder of the environment's own files
	TempDir() string
	// Host folder mounted into the sandbox and where the sandbox sees it, both empty when none is
	Mount() (host, sandbox string)
	// Home of the sandbox user when the backend declares one
	SandboxHome() string
	// Keeps its state across sessions
	Persistent() bool
	// Has an identity of its own session
	SessionScoped() bool
	// The file tools may read and write its files on the host
	HostFiles() bool
	// Copies a regular file of at most max bytes out of the environment
	FetchFile(ctx context.Context, remote, local string, max int) error
	// Path the environment resolves remote to, empty when it can't
	FetchRealpath(ctx context.Context, remote string) string
}

// How one command runs
type Exec struct {
	// Folder it runs in, the environment's current one when empty
	Cwd string
	// How long it may run, the environment's default when zero
	Timeout time.Duration
	// Text it reads on stdin, none when nil
	Stdin *string
	// Leaves `A && B &` as written, for a script the caller made safe already
	Raw bool
	// Most characters of output kept, its head and tail, the rest counted; all of it is kept when zero
	Limit int
	// File the whole output is copied to once it passes Limit
	Spill string
	// Whether the user asked the running tool to hand its work to the background, consumed when it answers true
	Yield func() bool
	// Takes the still running command into the background with the output so far, its result ending the call; nil keeps waiting
	Handoff func(p Proc, output string) *Result
	// Variables of this command alone, such as those naming the session it runs for, set over the session's own after its snapshot is
	// sourced; a name given no value is unset
	Env map[string]string
}

// How a command ended
type Result struct {
	Output string
	Code   int
	// Folder the command ended in, empty when it reported none
	Cwd string
	// Exit codes of the last pipeline, one per command
	Pipeline []int
	// Characters the command wrote and where all of them were kept, when more than the limit
	Total   int
	Spilled string
	// The command ran out its time, or was stopped
	TimedOut    bool
	Interrupted bool
	// The sandbox was gone and made again while the command ran
	Recreated bool
	// Why the command's stdin could not be written
	StdinError string
	// Background job the command went on as
	Yielded string
	Pid     int
}

// A running command
type Proc interface {
	// Exit code once it finished
	Poll() (int, bool)
	Kill() error
	// Exit code once it finishes within the timeout, waiting for ever when it is zero
	Wait(timeout time.Duration) (int, error)
	// Read end of its output, stdout and stderr merged
	Output() *os.File
	Pid() int
}

// Environment whose commands are processes of this machine, so its background jobs run here directly, read, signalled and written to
type Spawner interface {
	// Command running the script in the background in dir, with the user's shell and the environment commands get
	Command(ctx context.Context, script, dir string) (Command, error)
}

// Process to start: argv, folder and environment
type Command struct {
	Argv []string
	Dir  string
	Env  []string
}

// Environment that runs an argv through a command of this machine, which opens streams and forwards ports into it
type Streamer interface {
	// Argv of this machine that runs the rest inside the environment as user, with stdin kept open when interactive
	ExecPrefix(user string, interactive bool) ([]string, error)
}

// Streamer whose client joins the argv into one line for a remote login shell
type Joiner interface {
	ShellJoined() bool
}

// Environment in a container of a program of this machine, naming it so a later run can tell whether it is still there
type Boxed interface {
	// Id of the container and the program that runs it, the id empty while it is not started
	Box() (id, program string)
}

// Environment that brings back its sandbox when a command's output says it was removed behind its back
type Recoverer interface {
	RecoverGone(ctx context.Context, output string) bool
}

// Environment that can run sudo without a password, probed inside it
type Sudoer interface {
	SudoWithoutPassword(ctx context.Context) bool
}

// Persistent sandbox stopped rather than removed when it goes idle
type Stopper interface {
	StopIdle() error
}

// Environment whose removal finishes in the background, waited on at exit
type CleanupWaiter interface {
	WaitCleanup(timeout time.Duration)
}

// Infrastructure failure of a backend, never a command's own
type ConnectionError struct {
	Reason string
	// What the model should do about it
	Hint string
}

const connectionHint = "This is an infrastructure failure, not a command failure. Verify the backend is reachable (network, service running, " +
	"credentials), then retry the same command — recovery is automatic once the backend is back."

func NewConnectionError(reason, hint string) *ConnectionError {
	if hint == "" {
		hint = connectionHint
	}
	return &ConnectionError{Reason: reason, Hint: hint}
}

func (e *ConnectionError) Error() string { return e.Reason }

// A backend's SDK or helper program is not installed
type MissingError struct {
	Reason string
}

func (e *MissingError) Error() string { return e.Reason }

// A file that could not be copied out of an environment
type FetchError struct {
	Reason string
}

func (e *FetchError) Error() string { return e.Reason }
