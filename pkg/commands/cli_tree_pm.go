package commands

import (
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/spf13/cobra"
)

// Binds the pm verbs to the package manager, set by the build that links it and nil in a build without it
var pmHandlers func(c *CLI)

// Adds the pm tree, bound to pkg/pm handlers where the package manager is linked, plus the hidden update takeover entry
func (c *CLI) addPM(root *cobra.Command) {
	pm.SetCommandArgv(c.CommandArgv)
	pmCmd := c.newCmd(root, "pm", "Package manager: lock, install, repair, env, doctor, gc", "")
	pmCmd.Annotations = map[string]string{argparseTopAnnotation: "pm/cli.py"}
	pmCmd.Args = c.pmVerbArgs
	pmCmd.FParseErrWhitelist.UnknownFlags = true
	c.Bind("pm", func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return c.Execute(append([]string{cmd.Name()}, args...))
		}
		return &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: cmd"}
	})

	lock := c.newCmd(pmCmd, "lock", "relock uv.lock from pyproject.toml (or --bump a tool pin in pm/lock.json)",
		"With no arguments: re-resolve uv.lock from pyproject.toml; changes no environment and writes nothing when the lock is current. With --bump NAME VERSION: pin a pm tool's artifacts in pm/lock.json; uv.lock is untouched.")
	sf(lock, "bump", "", "", "pin tool NAME at VERSION in pm/lock.json instead of relocking uv.lock")
	c.pos(lock, pOpt("version").withMetavar("VERSION"))

	install := c.newCmd(pmCmd, "install", "install packages (default: all required + optional defaults)", "")
	c.pos(install, pAny("names"))
	af(install, "extra", "", "enable a declared dependency extra in the venv (repeatable)")
	af(install, "without", "", "leave an optional default package (chromium, cua-driver) or a recorded extra outside [all] (matrix, ...) out of this and every later default install and update; `icebath pm install NAME` / `--extra NAME` opts back in (repeatable)")
	bf(install, "tools-only", "", "install the tool closure, put it on PATH, and stop before the venv sync")
	bf(install, "trust-recorded", "", "trust the recorded tool digest instead of re-hashing every entry. shell activation only. a deliberate install re-checks the bytes")
	sf(install, "target", "", "", "stage for a cross target (e.g. linux-arm64-bionic on a glibc CI host); requires explicit package names")

	env := c.newCmd(pmCmd, "env", "print composed env of installed packages", "")
	c.pos(env, pAny("names"))

	doctor := c.newCmd(pmCmd, "doctor", "check installed state against the lockfile", "")
	bf(doctor, "repair", "", "remove corrupt entries from PM's uv cache so they are fetched again")

	c.newCmd(pmCmd, "repair", "rebuild the recorded dependency environment without changing its graph", "")

	environment := c.newCmd(pmCmd, "environment", "make a named side environment current and print its python",
		"Build or reuse ICEBATH_HOME/environments/NAME, an isolated venv that never touches Icebath's own: --extra installs the pins of a declared extra, --requirement an ad hoc bounded requirement (both repeatable). Prints only the interpreter path on stdout.")
	c.pos(environment, p("name"))
	af(environment, "extra", "", "")
	af(environment, "requirement", "", "")

	c.newCmd(pmCmd, "gc", "remove store entries nothing references", "")

	c.newCmd(pmCmd, "status", "print the latest pm sync receipt (machine-readable)", "")

	update := c.newCmd(pmCmd, "update", "resolve latest versions and re-pin the lockfile", "")
	c.pos(update, pAny("names"))
	bf(update, "check", "", "dry-run: print what would change, write nothing (exit 1 if updates exist)")
	sf(update, "target", "", "", "resolve for a different target instead of this machine (e.g. win32-arm64)")
	af(update, "skip-target", "", "leave TARGET's pins as they are, e.g. linux-arm64-bionic while the termux pool lags upstream (repeatable)")
	c.choice(update, "skip-target", pm.AllTargets...)
	bf(update, "uv", "", "also refresh uv.lock + venv (uv update + sync)")
	bf(update, "npm", "", "also refresh package-lock.json (npm update)")
	bf(update, "termux", "", "repin the termux pool archives the pool has retired (the runtime-lib table + the bionic lock rows); --check reports without writing")

	worker := c.newCmd(pmCmd, "worker", "isolated package manager worker", "")
	worker.Hidden = true
	pmParsers(pmCmd, lock, install, environment, update)
	if pmHandlers != nil {
		pmHandlers(c)
	}

	takeover := c.newCmd(root, "update-takeover", "finish an update a historical updater handed off", "")
	takeover.Hidden = true
	c.pos(takeover, p("context"), p("result"))
	finish := c.newCmd(root, "update-finish", "finish an update in the selected checkout", "")
	finish.Hidden = true
	c.pos(finish, p("context"), p("result"))
	restartRecovery := c.newCmd(root, "update-restart-recovery", "restart supervised gateways and serve units in a fresh process", "")
	restartRecovery.Hidden, restartRecovery.DisableFlagParsing = true, true
	c.pos(restartRecovery, pRem("args"))
	updateCompletion := c.newCmd(root, "update-completion", "run a source update's post commit completion", "")
	updateCompletion.Hidden, updateCompletion.DisableFlagParsing = true, true
	c.pos(updateCompletion, pRem("args"))
	venvSync := c.newCmd(root, "venv-sync", "report or sync a checkout's Python dependencies", "")
	venvSync.Hidden, venvSync.DisableFlagParsing = true, true
	c.pos(venvSync, pRem("args"))
	completion := c.newCmd(root, "source-completion", "finish a source update's launchers, products and maintenance", "")
	completion.Hidden, completion.DisableFlagParsing = true, true
	c.pos(completion, pRem("args"))
	importHealth := c.newCmd(root, "import-health", "answer an update's import health probe", "")
	importHealth.Hidden, importHealth.DisableFlagParsing = true, true
	c.pos(importHealth, pRem("args"))
}

// Stores the pm/cli.py parser shapes, its dests, metavars and positional help, the verb list leaving hidden entries out
func pmParsers(pmCmd, lock, install, environment, update *cobra.Command) {
	opt := func(name, dest, metavar, nargs string) apAction {
		a := apAction{Kind: 'f', Opts: []string{"--" + name}, Dest: dest, Nargs: nargs}
		if metavar != "" {
			a.Metavar = []string{metavar}
		}
		return a
	}
	verbs := apAction{Kind: 's', Dest: "cmd", Nargs: "A...", Required: true}
	for _, sub := range pmCmd.Commands() {
		if !sub.Hidden {
			verbs.Choices = append(verbs.Choices, sub.Name())
			verbs.Sub = append(verbs.Sub, apChoice{Name: sub.Name(), Help: sub.Short, Listed: true})
		}
	}
	argparseOwn.Store(pmCmd, &apParser{Acts: []apAction{apHelpActionTemplate, verbs}})
	argparseOwn.Store(lock, &apParser{Desc: true, Acts: []apAction{apHelpActionTemplate, opt("bump", "name", "NAME", ""),
		{Kind: 'p', Dest: "version", Nargs: "?", Metavar: []string{"VERSION"}, Help: "the tool version (only with --bump)"}}})
	argparseOwn.Store(install, &apParser{Acts: []apAction{apHelpActionTemplate, {Kind: 'p', Dest: "names", Nargs: "*"},
		opt("extra", "extra", "NAME", ""), opt("without", "without", "NAME", ""), opt("tools-only", "tools_only", "", "0"),
		opt("trust-recorded", "trust_recorded", "", "0"),
		opt("target", "target", "", "")}})
	argparseOwn.Store(environment, &apParser{Desc: true, Acts: []apAction{apHelpActionTemplate, {Kind: 'p', Dest: "name"},
		opt("extra", "extra", "EXTRA", ""), opt("requirement", "requirement", "REQ", "")}})
	skip := opt("skip-target", "skip_target", "TARGET", "")
	skip.Choices = pm.AllTargets
	argparseOwn.Store(update, &apParser{Acts: []apAction{apHelpActionTemplate,
		{Kind: 'p', Dest: "names", Nargs: "*", Help: "packages to check/update (default: all with a latest source)"},
		opt("check", "check", "", "0"), opt("target", "target", "", ""), skip,
		opt("uv", "uv", "", "0"), opt("npm", "npm", "", "0"), opt("termux", "termux", "", "0")}})
}

// Refuses a pm line naming no verb or one pm/cli.py does not list, a verb after unknown options passing
func (c *CLI) pmVerbArgs(cmd *cobra.Command, args []string) error {
	items, _ := apItems(cmd)
	verbs := items[len(items)-1].Choices
	token := ""
	if len(args) > 0 {
		token = args[0]
	} else if i := slices.Index(c.argv, cmd.Name()); i >= 0 {
		for _, tok := range c.argv[i+1:] {
			if !strings.HasPrefix(tok, "-") {
				token = tok
				break
			}
		}
	}
	switch {
	case token == "":
		return &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: cmd"}
	case slices.Contains(verbs, token):
		return nil
	}
	return &ArgparseError{Cmd: cmd, Msg: "argument cmd: invalid choice: " + strconv.Quote(token) + " (choose from " + reprList(verbs, strconv.Quote) + ")"}
}
