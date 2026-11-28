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
	"github.com/nickheyer/icebath/pkg/strs"
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
		Use:     "verify [path]",
		GroupID: cli.GroupAbilities,
		Short:   "Find out how to build and run a project, then try it",
		Long: "Works out how the project in the folder is set up, built, tested and started, or reads the recipe saved in " +
			".icebath/environment.json. Then verify runs the bootstrap, build and test steps, starts the app in the background, waits " +
			"until it answers and stops it again. Use verify to check that a project works before or after the agent changes the project.",
		Example: "  icebath verify\n  icebath verify ./api --phase build --phase test\n  icebath verify --detect-only --save",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			for _, phase := range o.phases {
				if !slices.Contains(append(slices.Clone(recipes.Phases), "start"), phase) {
					return fmt.Errorf("“%s” is not a step. Choose bootstrap, build, test or start.", phase)
				}
			}
			return c.run(cmd, path, o)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&o.detectOnly, "detect-only", false, "Only print the recipe as JSON, without running anything")
	flags.BoolVar(&o.save, "save", false, "Save the recipe as .icebath/environment.json in the project")
	flags.BoolVar(&o.skipStart, "skip-start", false, "Run the steps, but do not start the app or wait for it to answer")
	flags.StringArrayVar(&o.phases, "phase", nil, "Step to run: bootstrap, build, test or start, repeatable")
	flags.IntVar(&o.port, "port", 0, "Port to check for an answer, instead of the one found")
	flags.Float64Var(&o.timeout, "timeout", recipes.PhaseTimeout, "Time limit in seconds for each step")
	flags.Float64Var(&o.readyTimeout, "ready-timeout", recipes.ReadyTimeout, "Time limit in seconds for the app to start answering")
	flags.BoolVar(&o.json, "json", false, "Print the result as JSON")
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
		fmt.Fprintf(errs, "✗ %s is not a folder.\n", root)
		return exit(2)
	}
	recipe, source, err := recipes.LoadOrDetect(root)
	if err != nil {
		fmt.Fprintln(errs, "✗ Could not read the recipe of the project. "+strs.Full(err.Error()))
		return exit(1)
	}
	if recipe == nil {
		if o.json {
			emit(out, map[string]any{"ok": false, "error": "no-recipe", "root": root}, false)
		} else {
			fmt.Fprintf(errs, "✗ %s holds no project that icebath recognizes.\n  Write a recipe of your own in %s.\n", root, recipes.ManifestPath(root))
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
			fmt.Fprintln(errs, "✗ Could not save the recipe. "+strs.Full(err.Error()))
			return exit(1)
		}
		if !o.json {
			fmt.Fprintf(out, "✓ Saved the recipe to %s.\n", saved)
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
		fmt.Fprintln(errs, "✗ Could not run the recipe. "+strs.Full(err.Error()))
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

// Length of a step in seconds as words, to a tenth of a second
func took(seconds float64) string {
	return strs.Span(time.Duration(seconds * float64(time.Second)).Round(100 * time.Millisecond))
}

// The table of phases, the readiness line, the verdict and the end of the output of what failed
func report(w io.Writer, recipe *recipes.Recipe, source string, result *recipes.Result) {
	from := "worked out from the files of the project"
	if source == "manifest" {
		from = "read from .icebath/environment.json"
	}
	fmt.Fprintf(w, "Recipe %s for a %s project, %s.\n\n", recipe.Name, recipe.Kind, from)
	var rows [][4]string
	for _, p := range result.Phases {
		status := "Failed"
		switch {
		case p.OK():
			status = "Passed"
		case p.TimedOut:
			status = "Timed out"
		}
		rows = append(rows, [4]string{p.Phase, status, took(p.Duration), p.Command})
	}
	if r := result.Readiness; r != nil {
		status := "Failed"
		if r.Ready {
			status = "Passed"
		}
		rows = append(rows, [4]string{"start", status, took(r.Duration), cmp.Or(recipe.Start, "None")})
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "  No step ran.")
	}
	widths := [3]int{}
	for _, row := range rows {
		for i := range widths {
			widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
		}
	}
	pad := func(text string, width int) string {
		return text + strings.Repeat(" ", width-utf8.RuneCountInString(text))
	}
	for _, row := range rows {
		fmt.Fprintf(w, "  %s  %s  %s  %s\n", pad(row[0], widths[0]), pad(row[1], widths[1]), pad(row[2], widths[2]), row[3])
	}
	if r := result.Readiness; r != nil {
		switch {
		case r.Ready:
			fmt.Fprintf(w, "\nThe app answered at %s with HTTP status %d.\n", r.URL, *r.Status)
		case r.Error != "":
			fmt.Fprintf(w, "\nThe app did not answer at %s: %s.\n", r.URL, strings.TrimSuffix(r.Error, "."))
		default:
			fmt.Fprintf(w, "\nThe app did not answer at %s in time.\n", r.URL)
		}
	}
	if result.OK() {
		fmt.Fprintln(w, "\n✓ Every step passed.")
	} else {
		fmt.Fprintln(w, "\n✗ Not every step passed.")
	}
	for _, p := range result.Phases {
		if !p.OK() {
			fmt.Fprintf(w, "\nLast lines of %s, %s:\n%s\n", p.Phase, p.Command, strings.TrimRightFunc(p.Tail, unicode.IsSpace))
		}
	}
	if r := result.Readiness; r != nil && !r.Ready && r.Tail != "" {
		fmt.Fprintf(w, "\nLast lines of start:\n%s\n", strings.TrimRightFunc(r.Tail, unicode.IsSpace))
	}
}
