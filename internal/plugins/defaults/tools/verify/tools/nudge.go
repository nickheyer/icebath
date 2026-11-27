package tools

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/contexts"
	verifyconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/recipes"
	"github.com/nickheyer/icebath/internal/tools"
)

const (
	// Changed paths the nudge names, and characters of the newest check's output it quotes
	pathsShown   = 8
	summaryShown = 1200
	// Characters of a check that did not count the nudge quotes
	hiddenShown = 200
)

// How to verify, added to the nudge unless tools.verify.guidance turns it off
const Guidance = "[Coding] Before you run tests/linters or call this done: if this is creative UI/visual work, hold off on tests and linters until " +
	"the user says they like the result or you're about to commit. And before every commit, clean your work: keep it KISS/DRY, match the " +
	"surrounding code style, and be elitist, shorthand, clever, concise, efficient, and elegant."

const hiddenWarning = " Your last check run (`%s`) did not count: the shell could not tell the check's own exit status apart from the commands " +
	"around it. Run the check directly; if you pipe it, keep the check as the first command in the pipeline. The terminal already truncates long " +
	"output and spills the rest to a file."

var (
	// Files with nothing to verify: documentation and prose, by extension, and by name for those without one
	proseExtensions = []string{".md", ".markdown", ".mdx", ".rst", ".txt", ".text", ".adoc", ".asciidoc", ".org", ".log", ".csv", ".tsv"}
	proseNames      = []string{"license", "licence", "notice", "authors", "contributors", "changelog", "codeowners"}
)

func prose(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return slices.Contains(proseExtensions, ext) || ext == "" && slices.Contains(proseNames, strings.ToLower(filepath.Base(path)))
}

// Folders of the changed paths, each once in order: a folder itself, else a file's parent
func folders(paths []string) []string {
	var out []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		dir := resolve(path)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			dir = filepath.Dir(dir)
		}
		if !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	return out
}

// State and project of the first changed workspace that lacks passing evidence, else of the first workspace found
func (l *Ledger) snapshot(ctx context.Context, cfg *config.Config, session string, paths []string) (*status, *tools.Project, error) {
	var first *status
	var firstProject *tools.Project
	for _, dir := range folders(paths) {
		project, ok := tools.ProjectOf(dir)
		if !ok {
			continue
		}
		st, err := l.status(ctx, cfg, session, dir)
		if err != nil {
			return nil, nil, err
		}
		if first == nil {
			first, firstProject = &st, &project
		}
		if st.State != "passed" {
			return &st, &project, nil
		}
	}
	return first, firstProject, nil
}

// Whether icebath verify can run the workspace: it saves a manifest, or its files make a recipe that starts the app
func runnable(root string) bool {
	if info, err := os.Stat(recipes.ManifestPath(root)); err == nil && info.Mode().IsRegular() {
		return true
	}
	recipe, err := recipes.Detect(root)
	return err == nil && recipe != nil && recipe.Start != ""
}

// The state with the newest check and the end of its output
func detail(st *status) string {
	if st.Event == nil {
		return st.State
	}
	parts := []string{st.State}
	if command := cmp.Or(st.Event.Canonical, st.Event.Command); command != "" {
		parts = append(parts, "last command `"+command+"`")
	}
	if summary := strings.TrimSpace(st.Event.Summary); summary != "" {
		parts = append(parts, "last output:\n"+contexts.Elide(summary, summaryShown))
	}
	return strings.Join(parts, "\n")
}

// Changed paths as a list, the rest counted
func listed(paths []string) string {
	var lines []string
	for _, path := range paths[:min(len(paths), pathsShown)] {
		lines = append(lines, "- `"+path+"`")
	}
	if len(paths) > pathsShown {
		lines = append(lines, fmt.Sprintf("- ... and %d more", len(paths)-pathsShown))
	}
	return strings.Join(lines, "\n")
}

// Message sending the agent back to check the code it changed in the turn while its workspace has no fresh passing evidence; empty when
// the changes are prose alone, lie in no project or were checked
func (l *Ledger) nudge(ctx context.Context, cfg *config.Config, session string, changed []string) (string, error) {
	var paths []string
	for _, path := range changed {
		if path != "" && !prose(path) && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	if len(paths) == 0 {
		return "", nil
	}
	st, project, err := l.snapshot(ctx, cfg, session, paths)
	if err != nil || project == nil || st.State == "passed" {
		return "", err
	}
	since := ""
	if st.Event != nil {
		since = st.Event.Created
	}
	warning := ""
	if command := l.hidden(st.Session, st.Root, since); command != "" {
		warning = fmt.Sprintf(hiddenWarning, contexts.Elide(command, hiddenShown))
	}
	var commands []string
	for _, command := range project.Verify {
		if command = strings.TrimSpace(command); command != "" {
			commands = append(commands, command)
		}
	}
	recipe := runnable(project.Root)
	var instruction string
	switch {
	case len(commands) > 0:
		var quoted []string
		for _, command := range commands[:min(len(commands), 3)] {
			quoted = append(quoted, "`"+command+"`")
		}
		more := ""
		if len(commands) > 3 {
			more = ", ..."
		}
		instruction = "Run the relevant verification command now (" + strings.Join(quoted, ", ") + more + "), read any failure, repair the code, and " +
			"summarize what passed."
		if recipe {
			instruction += " For a full check including a runtime boot (build + test + start + readiness), prefer `icebath verify --json` — a passing " +
				"run records verification evidence for this workspace."
		}
	case recipe:
		instruction = "No canonical test/lint/build command was detected, but the project has a runnable verification recipe. Run `icebath verify " +
			"--json` (detect -> build -> test -> boot -> readiness poll); a passing run records verification evidence for this workspace. Read any " +
			"failure, repair the code, and summarize what passed."
	default:
		instruction = "No canonical test/lint/build command was detected. Create a focused temporary verification script under `" + resolve(os.TempDir()) +
			"` using an OS-safe `tempfile` path with a `icebath-verify-` filename prefix, run it against the changed behavior, clean it up when " +
			"possible, and summarize it explicitly as ad-hoc verification rather than suite green."
	}
	addendum := ""
	if cfg.GetBool(verifyconfig.Guidance) {
		addendum = "\n\n" + Guidance
	}
	return "[System: You edited code in this turn, but the workspace does not have fresh passing verification evidence yet.\n\n" +
		"Verification status: " + detail(st) + "\n\n" +
		"Changed paths:\n" + listed(paths) + "\n\n" +
		instruction + warning + " If verification is not possible, explain the concrete blocker instead of claiming the work is fully verified." +
		addendum + "]", nil
}
