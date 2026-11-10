package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// The hermes_cli.cli_output functions platform wizards resolve at call time, a table tests replace like the patched module
type CLIOutputFns struct {
	Prompt       func(question, def string, password bool) string
	PromptYesNo  func(ctx context.Context, question string, def bool) bool
	PrintHeader  func(text string)
	PrintInfo    func(text string)
	PrintSuccess func(text string)
	PrintWarning func(text string)
	PrintError   func(text string)
}

// The hermes_cli.cli_output module as the platform wizards see it, reading and printing on the setup console
var WizardCLI = CLIOutputFns{
	Prompt: func(question, def string, password bool) string {
		return provider.SetupConsole().Prompt(question, def, password)
	},
	PromptYesNo: func(ctx context.Context, question string, def bool) bool {
		return provider.SetupConsole().PromptYesNo(ctx, question, def)
	},
	PrintHeader:  func(text string) { provider.SetupConsole().PrintHeader(text) },
	PrintInfo:    provider.SetupPrintInfo,
	PrintSuccess: provider.SetupPrintSuccess,
	PrintWarning: provider.SetupPrintWarning,
	PrintError:   provider.SetupPrintError,
}

// The hermes_cli.config env file functions platform wizards resolve at call time
type ConfigFns struct {
	GetEnvValue    func(ctx context.Context, key string) (string, error)
	SaveEnvValue   func(ctx context.Context, key, value string) error
	RemoveEnvValue func(ctx context.Context, key string) (bool, error)
}

// The hermes_cli.config module as the platform wizards see it, every call on the home in ctx
var WizardConfig = ConfigFns{
	GetEnvValue: func(ctx context.Context, key string) (string, error) {
		v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), key)
		return v, err
	},
	SaveEnvValue: func(ctx context.Context, key, value string) error {
		return config.SaveEnvValueContext(ctx, home.Dir(ctx), key, value)
	},
	RemoveEnvValue: func(ctx context.Context, key string) (bool, error) {
		return config.RemoveHomeEnvValue(ctx, home.Dir(ctx), key)
	},
}

// Prints a line on the setup console like a bare print in a platform wizard
func WizardPrintln(text string) { fmt.Fprintln(provider.SetupOutput(), text) }

// The KeyboardInterrupt a long running wizard step raises when Ctrl-C arrives
var ErrKeyboardInterrupt = errors.New("KeyboardInterrupt")

// Ctrl-C observer of a polling wizard step, the signal turned into ErrKeyboardInterrupt at the next sleep or check
type InterruptWatch struct{ ch chan os.Signal }

// Starts observing Ctrl-C until Stop
func WatchInterrupt() *InterruptWatch {
	w := &InterruptWatch{ch: make(chan os.Signal, 1)}
	signal.Notify(w.ch, os.Interrupt)
	return w
}

// Stops observing Ctrl-C
func (w *InterruptWatch) Stop() { signal.Stop(w.ch) }

// ErrKeyboardInterrupt when Ctrl-C arrived since the last check
func (w *InterruptWatch) Check() error {
	select {
	case <-w.ch:
		return ErrKeyboardInterrupt
	default:
		return nil
	}
}

// Sleeps the seconds, cut short by ErrKeyboardInterrupt when Ctrl-C arrives first
func (w *InterruptWatch) Sleep(seconds float64) error {
	t := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer t.Stop()
	select {
	case <-w.ch:
		return ErrKeyboardInterrupt
	case <-t.C:
		return nil
	}
}
