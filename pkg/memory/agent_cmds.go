package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Clears memory.provider in config.yaml so only the built-in memory stays active
func MemoryOff(ctx context.Context, out io.Writer) error {
	if err := TurnOffProvider(ctx); err != nil {
		return err
	}
	fmt.Fprint(out, "\n  ✓ Memory provider: built-in only\n")
	fmt.Fprint(out, "  Saved to config.yaml\n\n")
	return nil
}

// Deletes MEMORY.md, USER.md or both from the home memories dir after a typed yes unless yes is set
func MemoryReset(ctx context.Context, out io.Writer, in io.Reader, target string, yes bool) error {
	h := home.Dir(ctx)
	memDir := filepath.Join(h, "memories")
	shown := home.Display(h)
	existing := existingBuiltinMemory(ctx, target)
	if len(existing) == 0 {
		fmt.Fprintf(out, "\n  Nothing to reset — no memory files found in %s/memories/\n\n", shown)
		return nil
	}
	fmt.Fprint(out, "\n  This will permanently erase the following memory files:\n")
	for _, f := range existing {
		st, err := os.Stat(filepath.Join(memDir, f[0]))
		if err != nil {
			return err
		}
		size, err := pyrepr.Format(st.Size(), ",")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "    ◆ %s (%s) — %s bytes\n", f[0], f[1], size)
	}
	if !yes {
		line, err := (&cliui.Console{In: in, Out: out}).Input("\n  Type 'yes' to confirm: ")
		if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
			fmt.Fprint(out, "\n  Cancelled.\n\n")
			return nil
		}
		if err != nil {
			return err
		}
		if strings.ToLower(strings.TrimSpace(line)) != "yes" {
			fmt.Fprint(out, "  Cancelled.\n\n")
			return nil
		}
	}
	if _, err := eraseBuiltinMemory(ctx, existing, out); err != nil {
		return err
	}
	fmt.Fprint(out, "\n  Memory reset complete. New sessions will start with a blank slate.\n")
	fmt.Fprintf(out, "  Files were in: %s/memories/\n\n", shown)
	return nil
}

// Prints the terminal insights report for the home state.db, the no data line when absent and the error line on failure
func CmdInsights(ctx context.Context, out io.Writer, days int, source string) {
	path := home.StatePath(home.Dir(ctx))
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(out, "  %s\n", i18n.T(ctx, "cli.insights.no_session_data", "", nil))
		return
	}
	text, err := insightsTerminalReport(ctx, path, days, source)
	if err != nil {
		fmt.Fprintln(out, i18n.T(ctx, "gateway.insights.error", "", map[string]any{"error": err.Error()}))
		return
	}
	fmt.Fprintln(out, text)
}

// Store handle the insights command hands to the engine and always closes
type insightsSessionDB interface {
	InsightsStore
	Close() error
}

// Engine the insights command drives to generate and render the terminal report
type insightsTerminalEngine interface {
	Generate(ctx context.Context, days int, source string) (*InsightsReport, error)
	FormatTerminal(r *InsightsReport) string
	Close() error
}

// Session store constructor of the insights command, the SessionDB a Python case patches
var openInsightsSessionDB = func(path string, opts state.Options) (insightsSessionDB, error) {
	db, err := state.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Report engine constructor of the insights command, the InsightsEngine a Python case patches
var newInsightsTerminalEngine = func(db insightsSessionDB) (insightsTerminalEngine, error) {
	engine, err := OpenInsightsEngine(db)
	if err != nil {
		return nil, err
	}
	return engine, nil
}

// Opens the session store read only, renders the report and always closes the store
func insightsTerminalReport(ctx context.Context, path string, days int, source string) (string, error) {
	db, err := openInsightsSessionDB(path, state.Options{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	engine, err := newInsightsTerminalEngine(db)
	if err != nil {
		return "", err
	}
	defer func() { _ = engine.Close() }()
	report, err := engine.Generate(ctx, days, source)
	if err != nil {
		return "", err
	}
	return engine.FormatTerminal(report), nil
}
