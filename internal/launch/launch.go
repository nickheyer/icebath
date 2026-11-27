// Package launch is what features do around the app as the command line opens it: flags of the launch they add, and their work before
// the app runs and once it ends, such as a git worktree the launch's sessions work in
package launch

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
)

// A flag of the launch
type Flag struct {
	// Spellings, such as -w and --worktree
	Names []string
	Help  string
	// What the value it takes names, such as "names", for a flag given as -s NAME or --skills=NAME, as often as wanted; empty for a flag
	// that takes none
	Value string
}

// Flags of a launch given, by their first spelling, with the values of those that take one in the order they were given
type Given map[string][]string

// Whether the flag was given
func (g Given) Has(name string) bool {
	_, ok := g[name]
	return ok
}

// Values the flag was given, each comma separated list split and blanks left out; none when it was not given
func (g Given) Values(name string) []string {
	var out []string
	for _, value := range g[name] {
		for part := range strings.SplitSeq(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// Copy with the flag given, its values after those it holds
func (g Given) With(name string, values ...string) Given {
	out := Given{}
	for key, held := range g {
		out[key] = slices.Clone(held)
	}
	out[name] = append(out[name], values...)
	return out
}

// Runs the app, the next feature's work around it included
type Run func(ctx context.Context) error

type Registrant interface {
	// Flags of the launch the feature takes
	Flags() []Flag
	// Runs the app, doing around it what the flags given, by their first spelling, and the settings ask of the feature
	Around(ctx context.Context, conn *connect.Client, given Given, run Run) error
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Flags() []Flag {
	var flags []Flag
	for _, registrant := range r.registrants {
		flags = append(flags, registrant.Flags()...)
	}
	return flags
}

// Runs the app inside the work of every registrant, the first registered outermost
func (r *Registry) Around(ctx context.Context, conn *connect.Client, given Given, run Run) error {
	for _, registrant := range slices.Backward(r.registrants) {
		next := run
		run = func(ctx context.Context) error { return registrant.Around(ctx, conn, given, next) }
	}
	return run(ctx)
}

// Takes the registered flags out of a launch of the app, a command line that starts with a flag or is empty, by their first spelling, a
// flag that takes a value with the word after it or what follows its = sign; any other command line as it is, none taken
func (r *Registry) Take(args []string) ([]string, Given) {
	given := Given{}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args, given
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		taken := false
		for _, flag := range r.Flags() {
			name, value, inline := strings.Cut(arg, "=")
			if !slices.Contains(flag.Names, name) || inline && flag.Value == "" {
				continue
			}
			taken = true
			switch {
			case flag.Value == "":
				given[flag.Names[0]] = given[flag.Names[0]]
			case inline:
				given[flag.Names[0]] = append(given[flag.Names[0]], value)
			case i+1 < len(args):
				i++
				given[flag.Names[0]] = append(given[flag.Names[0]], args[i])
			default:
				// A flag left without its value is given empty, so what reads it can say so
				given[flag.Names[0]] = given[flag.Names[0]]
			}
			break
		}
		if !taken {
			rest = append(rest, arg)
		}
	}
	return rest, given
}

// Context carrying the flags of the launch taken ahead of a command, such as icebath -w chat, which the command runs the work around it
// with
func WithGiven(ctx context.Context, given Given) context.Context {
	return context.WithValue(ctx, givenKey{}, given)
}

// Flags of the launch the context carries, by their first spelling; none when it carries none
func From(ctx context.Context) Given {
	given, _ := ctx.Value(givenKey{}).(Given)
	return given
}

type givenKey struct{}

type Launch struct {
	Registry
	config *config.Config
	// The process's cleanup runs, whose own watchdog the one a stop signal armed leaves the process to
	cleaning atomic.Bool
}

func New(cfg *config.Config) *Launch {
	launchconfig.Register(cfg)
	return &Launch{config: cfg}
}

// Ends the process should it still run twice the exit watchdog after the stop signal ctx ends with, unless its cleanup runs by then; kill
// ends the commands it started first
func (l *Launch) Signaled(ctx context.Context, kill func()) {
	context.AfterFunc(ctx, func() { l.watchdog(2*l.config.GetDuration(launchconfig.ExitWatchdog), true, kill) })
}

// Ends the process should its cleanup, which begins now, still run after the exit watchdog; kill ends the commands it started first
func (l *Launch) Cleaning(kill func()) {
	l.cleaning.Store(true)
	l.watchdog(l.config.GetDuration(launchconfig.ExitWatchdog), false, kill)
}

func (l *Launch) watchdog(after time.Duration, signaled bool, kill func()) {
	if after <= 0 {
		return
	}
	time.AfterFunc(after, func() {
		if signaled && l.cleaning.Load() {
			return
		}
		slog.Warn(fmt.Sprintf("Exit watchdog fired after %.0fs — forcing process exit (a cleanup step or non-daemon thread is wedged).", after.Seconds()))
		kill()
		os.Exit(0)
	})
}
