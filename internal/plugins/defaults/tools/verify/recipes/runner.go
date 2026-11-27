package recipes

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
)

// Defaults of the longest one phase command may take and of the wait for the started app to answer, in seconds
const (
	PhaseTimeout = 600.0
	ReadyTimeout = 60.0
)

// Characters of a command's output a result keeps, from its end
const tailChars = 2000

// Phases that run commands, in their order; start runs the app after them
var Phases = []string{"bootstrap", "build", "test"}

// One command of a phase as it ran: its exit code, nil when it timed out, how long it took and the end of its output
type PhaseResult struct {
	Phase    string
	Command  string
	ExitCode *int
	Duration float64
	Tail     string
	TimedOut bool
}

// Whether it exited zero in time
func (p *PhaseResult) OK() bool { return p.ExitCode != nil && *p.ExitCode == 0 && !p.TimedOut }

func (p *PhaseResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Phase    string  `json:"phase"`
		Command  string  `json:"command"`
		ExitCode *int    `json:"exitCode"`
		Duration float64 `json:"duration"`
		OK       bool    `json:"ok"`
		TimedOut bool    `json:"timedOut"`
		Tail     string  `json:"outputTail"`
	}{p.Phase, p.Command, p.ExitCode, round(p.Duration), p.OK(), p.TimedOut, p.Tail})
}

// How the started app answered: the URL asked, whether and with what status it answered, the last failure to reach it, and the end of
// its output
type Readiness struct {
	URL      string
	Ready    bool
	Status   *int
	Duration float64
	Error    string
	Tail     string
}

func (r *Readiness) MarshalJSON() ([]byte, error) {
	var failure *string
	if r.Error != "" {
		failure = &r.Error
	}
	return json.Marshal(struct {
		URL      string  `json:"url"`
		Ready    bool    `json:"ready"`
		Status   *int    `json:"statusCode"`
		Duration float64 `json:"duration"`
		Error    *string `json:"error"`
		Tail     string  `json:"outputTail"`
	}{r.URL, r.Ready, r.Status, round(r.Duration), failure, r.Tail})
}

// A whole verification pass
type Result struct {
	Recipe    string
	Phases    []*PhaseResult
	Readiness *Readiness
	// Where the recipe came from, manifest or detected, when the result is to say it
	Source string
}

// Whether every phase passed and the app, when it was started, answered
func (r *Result) OK() bool {
	return passed(r.Phases) && (r.Readiness == nil || r.Readiness.Ready)
}

func passed(phases []*PhaseResult) bool {
	for _, p := range phases {
		if !p.OK() {
			return false
		}
	}
	return true
}

func (r *Result) MarshalJSON() ([]byte, error) {
	phases := r.Phases
	if phases == nil {
		phases = []*PhaseResult{}
	}
	return json.Marshal(struct {
		Recipe    string         `json:"recipe"`
		OK        bool           `json:"ok"`
		Phases    []*PhaseResult `json:"phases"`
		Readiness *Readiness     `json:"readiness"`
		Source    string         `json:"source,omitempty"`
	}{r.Recipe, r.OK(), phases, r.Readiness, r.Source})
}

// Seconds rounded to milliseconds
func round(seconds float64) float64 { return math.Round(seconds*1000) / 1000 }

// Last characters of an output, counted in code points
func tail(output string) string {
	runes := []rune(output)
	return string(runes[max(0, len(runes)-tailChars):])
}

// Output as text: invalid bytes replaced, line ends made \n
func decode(data []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(terminal.Decode(data), "\r\n", "\n"), "\r", "\n")
}

// What a pass runs: the phases chosen, every one when empty, their limits, whether the app is started, the port to ask it on, and
// whether a failed command stops the pass
type Options struct {
	Phases       []string
	PhaseTimeout time.Duration
	ReadyTimeout time.Duration
	SkipStart    bool
	Port         int
	StopOnFail   bool
	// Process the app's group is owned by, so it dies with icebath however icebath dies, and whose spawn ledger records it
	Supervisor *terminal.Supervisor
	Config     *config.Config
}

// Options of a pass of every phase with the default limits, stopping at the first failure
func Defaults() Options {
	return Options{PhaseTimeout: seconds(PhaseTimeout), ReadyTimeout: seconds(ReadyTimeout), StopOnFail: true}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Shell command of a recipe line, run in the project's root in a group of its own
func shell(command, root string) *exec.Cmd {
	cmd := shellCommand(command)
	cmd.Dir = root
	terminal.NewSession(cmd)
	return cmd
}

// Runs one command of a phase as a group icebath owns, the whole group stopped and the output so far kept once it outlives the limit or the
// run is stopped
func runPhase(ctx context.Context, phase, command, root string, o Options) (*PhaseResult, error) {
	started := time.Now()
	proc, err := terminal.Start(shell(command, root), nil)
	if err != nil {
		return nil, err
	}
	if o.Supervisor != nil {
		proc.Own(o.Supervisor)
		defer proc.Release()
	}
	collected := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(proc.Output())
		collected <- data
	}()
	exited := make(chan int, 1)
	go func() {
		code, _ := proc.Wait(0)
		exited <- code
	}()
	var expire <-chan time.Time
	if o.PhaseTimeout > 0 {
		timer := time.NewTimer(o.PhaseTimeout)
		defer timer.Stop()
		expire = timer.C
	}
	result := &PhaseResult{Phase: phase, Command: command}
	select {
	case code := <-exited:
		result.ExitCode = &code
	case <-expire:
		proc.KillTree()
		<-exited
		result.TimedOut = true
	case <-ctx.Done():
		proc.KillTree()
		<-exited
		proc.Output().Close()
		return nil, ctx.Err()
	}
	var output []byte
	select {
	case output = <-collected:
	case <-time.After(time.Second):
		// A descendant that escaped the group keeps the pipe open; what came so far is kept
		proc.Output().Close()
		output = <-collected
	}
	proc.Output().Close()
	result.Duration = time.Since(started).Seconds()
	result.Tail = tail(decode(output))
	return result, nil
}

// Starts the app as a group icebath owns, polls it until it answers or the wait runs out, then stops the group and keeps the end of its
// output
func runStart(ctx context.Context, recipe *Recipe, root string, o Options) (*Readiness, error) {
	port := 8000
	switch {
	case o.Port != 0:
		port = o.Port
	case recipe.Port != 0:
		port = recipe.Port
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, recipe.ReadinessPath)
	if err := checkURL(url); err != nil {
		return nil, err
	}
	started := time.Now()
	proc, err := terminal.Start(shell(recipe.Start, root), nil)
	if err != nil {
		return nil, err
	}
	if o.Supervisor != nil {
		proc.Own(o.Supervisor)
		defer proc.Release()
	}
	if o.Config != nil {
		terminal.RegisterChild(o.Config, proc.Pid(), "verify-app")
	}
	collected := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(proc.Output())
		collected <- data
	}()
	ready, status, failure := poll(ctx, url, o.ReadyTimeout, time.Second)
	stop(proc)
	var output []byte
	select {
	case output = <-collected:
	case <-time.After(time.Second):
		proc.Output().Close()
		output = <-collected
	}
	proc.Output().Close()
	return &Readiness{URL: url, Ready: ready, Status: status, Duration: time.Since(started).Seconds(), Error: failure, Tail: tail(decode(output))},
		ctx.Err()
}

// Stops the app's group: TERM, then KILL after ten seconds for what is left
func stop(proc *terminal.HostProc) {
	if _, done := proc.Poll(); done {
		return
	}
	if err := terminal.SignalGroup(proc.Pid(), false); err != nil && !errors.Is(err, terminal.ErrGone) {
		proc.Kill()
	}
	if _, err := proc.Wait(10 * time.Second); err == nil {
		return
	}
	if err := terminal.SignalGroup(proc.Pid(), true); err != nil && !errors.Is(err, terminal.ErrGone) {
		proc.Kill()
	}
	proc.Wait(5 * time.Second)
}

// Why docker compose build and up must not run in the root, empty to go ahead: its project already has containers running, whose
// state a rebuild would destroy, or that could not be ruled out
func composeLive(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "ps", "--status", "running", "--format", "{{.Name}}")
	cmd.Dir = root
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "docker compose ps timed out after 15s; live containers cannot be ruled out"
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		text := cmp.Or(strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
		last := "no output"
		if parts := strings.Split(text, "\n"); text != "" {
			last = strings.TrimSpace(parts[len(parts)-1])
		}
		return fmt.Sprintf("docker compose ps failed (exit %d): %s", exit.ExitCode(), last)
	}
	if err != nil {
		// docker is not installed, so nothing of the project runs
		return ""
	}
	var names []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			names = append(names, strings.TrimRight(line, "\r"))
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "this compose project already has running container(s): " + strings.Join(names, ", ")
}

func (r *Recipe) commands(phase string) []string {
	switch phase {
	case "bootstrap":
		return r.Bootstrap
	case "build":
		return r.Build
	}
	return r.Test
}

// Runs the chosen phases in order, then starts the app, waits for it to answer and stops it; a compose project with containers running
// is refused before anything that would rebuild them
func Run(ctx context.Context, root string, recipe *Recipe, o Options) (*Result, error) {
	selected := o.Phases
	if len(selected) == 0 {
		selected = append(slices.Clone(Phases), "start")
	}
	result := &Result{Recipe: recipe.Name}
	mutating := slices.Contains(selected, "build") || slices.Contains(selected, "start") && !o.SkipStart
	if recipe.Kind == "compose" && mutating {
		if reason := composeLive(root); reason != "" {
			command := "docker compose build"
			if len(recipe.Build) > 0 {
				command = recipe.Build[0]
			}
			one := 1
			result.Phases = append(result.Phases, &PhaseResult{Phase: "build", Command: command, ExitCode: &one,
				Tail: "Refusing to run: " + reason + ". `docker compose build` + `up` would replace live containers on an image-hash change, destroying " +
					"any container-local state they carry. If you intend to rebuild this live deployment, run `docker compose build`/`up` yourself."})
			return result, nil
		}
	}
	for _, phase := range Phases {
		if !slices.Contains(selected, phase) {
			continue
		}
		for _, command := range recipe.commands(phase) {
			ran, err := runPhase(ctx, phase, command, root, o)
			if err != nil {
				return nil, err
			}
			result.Phases = append(result.Phases, ran)
			if !ran.OK() && o.StopOnFail {
				return result, nil
			}
		}
	}
	if !o.SkipStart && slices.Contains(selected, "start") && recipe.Start != "" && passed(result.Phases) {
		readiness, err := runStart(ctx, recipe, root, o)
		if err != nil {
			return nil, err
		}
		result.Readiness = readiness
	}
	return result, nil
}
