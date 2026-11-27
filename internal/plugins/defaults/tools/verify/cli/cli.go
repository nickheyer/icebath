package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/recipes"
	verifytools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/tools"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
)

// Ends the command with the exit code, after its output was written
type exit int

func (e exit) Error() string { return "" }
func (e exit) ExitCode() int { return int(e) }

type commands struct {
	config   *config.Config
	ledger   *verifytools.Ledger
	terminal *terminal.Terminal
}

func Register(registry *cli.Registry, cfg *config.Config, ledger *verifytools.Ledger, host *terminal.Terminal) {
	registry.Register(commands{config: cfg, ledger: ledger, terminal: host})
}

// What a verify run is asked to do
type options struct {
	detectOnly, save, skipStart, json bool
	phases                            []string
	port                              int
	timeout, readyTimeout             float64
}

func (c commands) Commands(*connect.Client) []*cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "verify [path]",
		Short: "Detect a project's run recipe and smoke-test it",
		Long: "Detect how the current project is built, tested, and started (or load the saved manifest at .icebath/environment.json), then run a " +
			"verification pass: bootstrap -> build -> test -> start in background -> poll readiness -> teardown.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			for _, phase := range o.phases {
				if !slices.Contains(append(slices.Clone(recipes.Phases), "start"), phase) {
					return fmt.Errorf("invalid choice for --phase: %q (choose from bootstrap, build, test, start)", phase)
				}
			}
			return c.run(cmd, path, o)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&o.detectOnly, "detect-only", false, "Only detect and print the recipe as JSON; run nothing")
	flags.BoolVar(&o.save, "save", false, "Save the recipe as .icebath/environment.json in the project")
	flags.BoolVar(&o.skipStart, "skip-start", false, "Run command phases but skip starting the app / readiness poll")
	flags.StringArrayVar(&o.phases, "phase", nil, "Run only the given phase(s); repeatable (bootstrap, build, test, start)")
	flags.IntVar(&o.port, "port", 0, "Override the port used for the readiness poll")
	flags.Float64Var(&o.timeout, "timeout", recipes.PhaseTimeout, fmt.Sprintf("Per-phase timeout in seconds (default: %.0f)", recipes.PhaseTimeout))
	flags.Float64Var(&o.readyTimeout, "ready-timeout", recipes.ReadyTimeout, fmt.Sprintf("Readiness poll timeout in seconds (default: %.0f)",
		recipes.ReadyTimeout))
	flags.BoolVar(&o.json, "json", false, "Emit a machine-readable JSON result")
	return []*cobra.Command{cmd}
}

func (c commands) run(cmd *cobra.Command, path string, o options) error {
	out, errs := cmd.OutOrStdout(), cmd.ErrOrStderr()
	root, err := filepath.Abs(path)
	if err == nil {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		fmt.Fprintf(errs, "error: not a directory: %s\n", root)
		return exit(2)
	}
	recipe, source, err := recipes.LoadOrDetect(root)
	if err != nil {
		fmt.Fprintln(errs, "error: "+err.Error())
		return exit(1)
	}
	if recipe == nil {
		if o.json {
			emit(out, map[string]any{"ok": false, "error": "no-recipe", "root": root}, false)
		} else {
			fmt.Fprintf(errs, "No recognizable project found at %s.\nCreate %s to define a recipe manually.\n", root, recipes.ManifestPath(root))
		}
		return exit(1)
	}
	if source == "detected" {
		merge(root, recipe)
	}
	if o.port != 0 {
		recipe.Port = o.port
	}
	if o.save {
		saved, err := recipes.SaveManifest(root, recipe)
		if err != nil {
			fmt.Fprintln(errs, "error: "+err.Error())
			return exit(1)
		}
		if !o.json {
			fmt.Fprintf(out, "Saved manifest: %s\n", saved)
		}
	}
	if o.detectOnly {
		emit(out, struct {
			Source string          `json:"source"`
			Recipe *recipes.Recipe `json:"recipe"`
		}{source, recipe}, !o.json)
		return nil
	}
	run := recipes.Defaults()
	run.Phases, run.SkipStart, run.Port = o.phases, o.skipStart, o.port
	run.PhaseTimeout, run.ReadyTimeout = seconds(o.timeout), seconds(o.readyTimeout)
	run.Supervisor, run.Config = c.terminal.Supervisor, c.config
	result, err := recipes.Run(cmd.Context(), root, recipe, run)
	if err != nil {
		fmt.Fprintln(errs, "error: "+err.Error())
		return exit(1)
	}
	c.record(cmd, root, recipe, result, len(o.phases) > 0 || o.skipStart)
	code := 1
	if result.OK() {
		code = 0
	}
	if o.json {
		result.Source = source
		emit(out, result, false)
	} else {
		report(out, recipe, source, result)
	}
	if code != 0 {
		return exit(code)
	}
	return nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Adds the checks the project's files name to the test phase of a detected recipe, a saved manifest being left as it is
func merge(root string, recipe *recipes.Recipe) {
	project := tools.ProjectAt(root)
	var existing []string
	for _, command := range slices.Concat(recipe.Bootstrap, recipe.Build, recipe.Test) {
		existing = append(existing, strings.TrimSpace(command))
	}
	for _, command := range project.Verify {
		if command = strings.TrimSpace(command); command != "" && !slices.Contains(existing, command) {
			recipe.Test = append(recipe.Test, command)
			existing = append(existing, command)
		}
	}
}

// Records the run as evidence for the project's workspace, a partial one as targeted; a ledger that fails leaves the run as it is
func (c commands) record(cmd *cobra.Command, root string, recipe *recipes.Recipe, result *recipes.Result, partial bool) {
	var tails []string
	for _, p := range result.Phases {
		if p.Tail != "" {
			tails = append(tails, fmt.Sprintf("[%s] %s\n%s", p.Phase, p.Command, p.Tail))
		}
	}
	if result.Readiness != nil {
		tails = append(tails, fmt.Sprintf("[start] %s -> %s", start(recipe), readiness(result.Readiness)))
	}
	scope := "full"
	if partial {
		scope = "targeted"
	}
	c.ledger.Verify(cmd.Context(), c.config, root, os.Getenv("ICEBATH_SESSION_ID"), result.OK(), scope, strings.Join(tails, "\n"))
}

// Start command of the recipe as the report shows it
func start(recipe *recipes.Recipe) string {
	return cmp.Or(recipe.Start, "(none)")
}

// ready (HTTP code), or not ready (why)
func readiness(r *recipes.Readiness) string {
	if r.Ready {
		return fmt.Sprintf("ready (HTTP %d)", *r.Status)
	}
	reason := "timeout"
	if r.Error != "" {
		reason = r.Error
	}
	return fmt.Sprintf("not ready (%s)", reason)
}

// Writes the value as JSON on one line, or indented when pretty, its text left unescaped
func emit(w io.Writer, value any, pretty bool) {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if pretty {
		encoder.SetIndent("", "  ")
	}
	encoder.Encode(value)
}

// The table of phases, the readiness line, the verdict and the end of the output of what failed
func report(w io.Writer, recipe *recipes.Recipe, source string, result *recipes.Result) {
	fmt.Fprintf(w, "Recipe: %s (%s) — source: %s\n\n", recipe.Name, recipe.Kind, source)
	width := 0
	for _, p := range result.Phases {
		width = max(width, utf8.RuneCountInString(p.Phase))
	}
	if len(result.Phases) == 0 {
		fmt.Fprintln(w, "  (no phases executed)")
	}
	for _, p := range result.Phases {
		status := "FAIL"
		switch {
		case p.OK():
			status = "PASS"
		case p.TimedOut:
			status = "TIMEOUT"
		}
		fmt.Fprintf(w, "  %-*s  %-7s  %6.1fs  %s\n", width, p.Phase, status, p.Duration, p.Command)
	}
	if r := result.Readiness; r != nil {
		status := "FAIL"
		if r.Ready {
			status = "PASS"
		}
		fmt.Fprintf(w, "  %-*s  %-7s  %6.1fs  %s\n\n", max(5, width), "start", status, r.Duration, start(recipe))
		fmt.Fprintf(w, "Readiness: %s -> %s\n", r.URL, readiness(r))
	}
	verdict := "FAILED"
	if result.OK() {
		verdict = "OK"
	}
	fmt.Fprintf(w, "\nResult: %s\n", verdict)
	for _, p := range result.Phases {
		if !p.OK() {
			fmt.Fprintf(w, "\n--- output tail: %s (%s) ---\n%s\n", p.Phase, p.Command, strings.TrimRightFunc(p.Tail, unicode.IsSpace))
		}
	}
	if r := result.Readiness; r != nil && !r.Ready && r.Tail != "" {
		fmt.Fprintf(w, "\n--- output tail: start ---\n%s\n", strings.TrimRightFunc(r.Tail, unicode.IsSpace))
	}
}
